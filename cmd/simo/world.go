package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/render"
	"github.com/taituo/simo/internal/spec"
	"github.com/taituo/simo/internal/store"
)

// isWorld reports whether dir holds a SQLite world (built with run --db).
func isWorld(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, store.WorldFile))
	return err == nil
}

func openWorld(fs *flag.FlagSet, args []string) (*store.Store, error) {
	dir, err := oneArg(fs, args, "world directory")
	if err != nil {
		return nil, err
	}
	if !isWorld(dir) {
		return nil, fmt.Errorf("%s is not a world directory (build one with: simo run SEED --db %s)", dir, dir)
	}
	return store.Open(dir)
}

// devicesMatching resolves a device name, or a prefix ending in *, to ids.
// An empty pattern returns nil, meaning all devices.
func devicesMatching(w *ir.World, pattern string) ([]uint32, error) {
	if pattern == "" {
		return nil, nil
	}
	ids := []uint32{}
	prefix, isPrefix := strings.CutSuffix(pattern, "*")
	for _, d := range w.Devices {
		if d.Name == pattern || (isPrefix && strings.HasPrefix(d.Name, prefix)) {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no device matches %q", pattern)
	}
	return ids, nil
}

func clock(w *ir.World, minute int64) string {
	return w.Start.Add(time.Duration(minute) * time.Minute).Format("2006-01-02 15:04")
}

// buildWorld implements run --db.
func buildWorld(w *ir.World, raw []byte, dir string, from, to int64, materialize bool, every string, workers int, stderr io.Writer) error {
	opt := store.BuildOptions{From: from, To: to, Materialize: materialize, Workers: workers}
	if every != "" {
		d, err := spec.ParseDuration(every)
		if err != nil || d < w.Tick {
			return fmt.Errorf("--checkpoint-every: want a duration of at least one tick, like 1h")
		}
		opt.CheckpointEvery = int64(d / w.Tick)
	}
	started := time.Now()
	res, err := store.Build(dir, w, raw, opt)
	if err != nil {
		return err
	}
	days := ""
	if materialize {
		names := make([]string, len(res.MaterializedDays))
		for i, d := range res.MaterializedDays {
			names[i] = "day" + strconv.Itoa(d+1)
		}
		days = fmt.Sprintf(", raw events stored for %s", strings.Join(names, " "))
		if len(names) == 0 {
			days = ", no whole day in the window to store"
		}
	}
	fmt.Fprintf(stderr, "built %s in %s: %s records (%s simulated), %s checkpoints, %s rollup rows%s\n",
		dir, time.Since(started).Round(100*time.Millisecond), commas(res.Stats.Records),
		fmtSpan(time.Duration(to-from)*w.Tick), commas(int64(res.Checkpoints)), commas(res.Rollups), days)
	return nil
}

// logsWorld implements logs on a world directory.
func logsWorld(dir string, wd window, f render.Format, device, level, event, grep, mode string, limit int, truth, explain bool, stdout, stderr io.Writer) error {
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer st.Close()
	w := st.World
	m, err := store.ParseMode(mode)
	if err != nil {
		return err
	}
	q := store.Query{Mode: m}
	if wd.from != "" || wd.to != "" || wd.dur != "" {
		from, to, err := wd.ticks(w)
		if err != nil {
			return err
		}
		q.FromNs, q.ToNs = from*w.TickNs, to*w.TickNs
	}
	if q.Devices, err = devicesMatching(w, device); err != nil {
		return err
	}
	if level != "" {
		l := spec.LevelIndex(level)
		if l < 0 {
			return fmt.Errorf("unknown level %q", level)
		}
		q.MinLevel = uint8(l)
	}
	if event != "" {
		for i, e := range w.Events {
			if e.ID == event {
				q.Templates = append(q.Templates, uint16(i))
			}
		}
		if q.Templates == nil {
			return fmt.Errorf("no event %q", event)
		}
	}
	rend, err := render.New(w)
	if err != nil {
		return err
	}
	rend.ShowCause = truth
	bw := bufio.NewWriterSize(stdout, 1<<20)
	defer bw.Flush()
	var buf []byte
	shown := 0
	segs, err := st.Events(q, func(r *record.Record) error {
		if grep != "" {
			buf = rend.Message(buf[:0], r)
			if !strings.Contains(string(buf), grep) {
				return nil
			}
		}
		buf = rend.Line(buf[:0], r, f)
		buf = append(buf, '\n')
		if _, err := bw.Write(buf); err != nil {
			return err
		}
		shown++
		if limit > 0 && shown >= limit {
			return store.ErrStop
		}
		return nil
	})
	if explain {
		for _, s := range segs {
			how := "regenerated from a checkpoint"
			if s.FromSQL {
				how = "read from stored events"
			}
			fmt.Fprintf(stderr, "%s to %s: %s\n", fmtOffset(time.Duration(s.FromNs)), fmtOffset(time.Duration(s.ToNs)), how)
		}
	}
	return err
}

func cmdStats(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var wd window
	wd.register(fs, "")
	by := fs.String("by", "event", "group by any of: "+strings.Join(store.RollupKeys, ", ")+" (comma separated)")
	step := fs.String("step", "", "bucket size, e.g. 5m or 1h (default: one bucket)")
	device := fs.String("device", "", "only this device; a trailing * matches a prefix")
	st, err := openWorld(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	w := st.World
	q := store.RollupQuery{}
	if *by != "" {
		q.By = strings.Split(*by, ",")
	}
	if wd.from != "" || wd.to != "" || wd.dur != "" {
		from, to, err := wd.ticks(w)
		if err != nil {
			return err
		}
		q.FromMin, q.ToMin = store.MinuteOf(from*w.TickNs), store.MinuteOf(to*w.TickNs+int64(time.Minute)-1)
	}
	if *step != "" {
		d, err := spec.ParseDuration(*step)
		if err != nil || d < time.Minute {
			return errors.New("--step: want at least 1m")
		}
		q.StepMin = int64(d / time.Minute)
	}
	if q.Devices, err = devicesMatching(w, *device); err != nil {
		return err
	}
	rows, err := st.Rollup(q)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	header := "from\t" + strings.Join(q.By, "\t") + "\tcount"
	fmt.Fprintln(tw, strings.TrimSuffix(strings.ReplaceAll(header, "\t\t", "\t"), "\t"))
	for _, r := range rows {
		cells := append([]string{clock(w, r.Minute)}, r.Keys...)
		cells = append(cells, commas(r.N))
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if len(rows) == 0 {
		fmt.Fprintf(stderr, "no counts in the window (rollups cover the build window, %s to %s)\n",
			fmtOffset(time.Duration(st.BuildFrom)*w.Tick), fmtOffset(time.Duration(st.BuildTo)*w.Tick))
	}
	return tw.Flush()
}

func cmdMetrics(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var wd window
	wd.register(fs, "")
	device := fs.String("device", "", "device name (required)")
	metric := fs.String("metric", "", "metric name (default: all of the device's metrics)")
	step := fs.String("step", "5m", "bucket size, e.g. 1m or 1h")
	st, err := openWorld(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	w := st.World
	ids, err := devicesMatching(w, *device)
	if err != nil {
		return err
	}
	if len(ids) != 1 {
		return errors.New("--device: name exactly one device")
	}
	dev := &w.Devices[ids[0]]
	cls := &w.Classes[dev.Class]
	d, err := spec.ParseDuration(*step)
	if err != nil || d < time.Minute {
		return errors.New("--step: want at least 1m")
	}
	var from, to int64
	if wd.from != "" || wd.to != "" || wd.dur != "" {
		f, t, err := wd.ticks(w)
		if err != nil {
			return err
		}
		from, to = store.MinuteOf(f*w.TickNs), store.MinuteOf(t*w.TickNs+int64(time.Minute)-1)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "from\tmetric\tmin\tavg\tmax\t")
	found := false
	for mi, m := range cls.Metrics {
		if *metric != "" && m.Name != *metric {
			continue
		}
		found = true
		rows, err := st.Metrics(dev.ID, mi, from, to, int64(d/time.Minute))
		if err != nil {
			return err
		}
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%.2f\t%.2f\t%.2f\t\n", clock(w, r.Minute), m.Name, r.Min, r.Avg, r.Max)
		}
	}
	if !found {
		return fmt.Errorf("%s has no metric %q", dev.Name, *metric)
	}
	return tw.Flush()
}

func cmdDevices(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	fs.SetOutput(stderr)
	class := fs.String("class", "", "only this class")
	site := fs.Int("site", 0, "only this site")
	st, err := openWorld(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	w := st.World
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "device\tclass\tsite")
	for _, d := range w.Devices {
		cls := w.Classes[d.Class].Name
		if (*class != "" && cls != *class) || (*site != 0 && d.Site != *site) {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\n", d.Name, cls, d.Site)
	}
	return tw.Flush()
}

func cmdMaterialize(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("materialize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var wd window
	wd.register(fs, "")
	workers := fs.Int("workers", 0, "worker goroutines (0 = all CPUs)")
	st, err := openWorld(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	w := st.World
	var fromNs, toNs int64
	if wd.from != "" || wd.to != "" || wd.dur != "" {
		from, to, err := wd.ticks(w)
		if err != nil {
			return err
		}
		fromNs, toNs = from*w.TickNs, to*w.TickNs
	}
	started := time.Now()
	days, err := st.Materialize(fromNs, toNs, *workers)
	for _, d := range days {
		fmt.Fprintf(stdout, "stored day%d\n", d+1)
	}
	if err != nil {
		return err
	}
	if len(days) == 0 {
		fmt.Fprintln(stderr, "nothing to do: every whole day in the window is already stored")
		return nil
	}
	fmt.Fprintf(stderr, "stored %s in %s\n", plural(len(days), "day"), time.Since(started).Round(100*time.Millisecond))
	return nil
}

func cmdTruth(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("truth", flag.ContinueOnError)
	fs.SetOutput(stderr)
	st, err := openWorld(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	w := st.World
	if len(w.Faults) == 0 {
		fmt.Fprintln(stdout, "no scheduled faults")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "cause\tstart\tend\ttarget\tdevices\tlabel")
	for _, f := range w.Faults {
		n := 0
		for _, in := range f.Member {
			if in {
				n++
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%s\n", f.ID, fmtOffset(time.Duration(f.Start)*w.Tick), fmtOffset(time.Duration(f.End)*w.Tick), f.Target, n, f.Label)
	}
	return tw.Flush()
}
