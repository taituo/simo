package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/render"
	"github.com/taituo/simo/internal/spec"
)

func cmdValidate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path, err := oneArg(fs, args, "seed file")
	if err != nil {
		return err
	}
	s, raw, err := spec.Load(path)
	if err != nil {
		return err
	}
	issues := spec.Validate(s)
	for _, is := range issues {
		fmt.Fprintln(stdout, is)
	}
	if err := issues.Err(); err != nil {
		return fmt.Errorf("%s has %s", path, plural(len(issues.Errors()), "error"))
	}
	w, err := loadQuiet(s, raw)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok: %s, %s, %s, tick %s, %s\n", w.Name, plural(len(w.Classes), "class"),
		plural(len(w.Devices), "device"), w.Tick, fmtSpan(time.Duration(w.Ticks)*w.Tick))
	return nil
}

func cmdPreview(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var wd window
	wd.register(fs, "1h")
	lines := fs.Int("lines", 12, "sample lines to show")
	format := fs.String("format", "text", "sample line format: text, jsonl or syslog")
	workers := fs.Int("workers", 0, "worker goroutines (0 = all CPUs)")
	path, err := oneArg(fs, args, "seed file")
	if err != nil {
		return err
	}
	f, err := render.ParseFormat(*format)
	if err != nil {
		return err
	}
	w, _, err := loadWorld(path, stderr)
	if err != nil {
		return err
	}
	from, to, err := wd.ticks(w)
	if err != nil {
		return err
	}
	const keep = 500000
	var sample []record.Record
	st, err := engine.Run(w, engine.Options{From: from, To: to, Workers: *workers}, func(recs []record.Record) error {
		if room := keep - len(sample); room > 0 {
			sample = append(sample, recs[:min(room, len(recs))]...)
		}
		return nil
	})
	if err != nil {
		return err
	}
	rend, err := render.New(w)
	if err != nil {
		return err
	}
	span := time.Duration(to-from) * w.Tick
	fmt.Fprintf(stdout, "%s: %s in %s, tick %s, previewing %s from %s\n", w.Name, plural(len(w.Devices), "device"),
		plural(len(w.Classes), "class"), w.Tick, fmtSpan(span), fmtOffset(time.Duration(from)*w.Tick))
	fmt.Fprintf(stdout, "records: %s (%.1f per second), transitions: %s\n\n", commas(st.Records),
		float64(st.Records)/span.Seconds(), commas(st.Transitions))

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "event\tlevel\tcount\tper hour\t")
	for e, n := range st.ByEvent {
		ev := &w.Events[e]
		fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%.0f\t\n", w.Classes[ev.Class].Name, ev.ID, spec.Levels[ev.Level], commas(n), float64(n)/span.Hours())
	}
	tw.Flush()

	fmt.Fprintln(stdout, "\ntime in state:")
	for c, cls := range w.Classes {
		total := int64(0)
		for _, n := range st.StateTicks[c] {
			total += n
		}
		var parts []string
		for i, n := range st.StateTicks[c] {
			if total > 0 {
				parts = append(parts, fmt.Sprintf("%s %.2f%%", cls.States[i].Name, 100*float64(n)/float64(total)))
			}
		}
		fmt.Fprintf(stdout, "  %s: %s\n", cls.Name, strings.Join(parts, ", "))
	}

	if *lines > 0 && len(sample) > 0 {
		fmt.Fprintln(stdout, "\nsample lines:")
		n := min(*lines, len(sample))
		var buf []byte
		for i := 0; i < n; i++ {
			rec := sample[i*len(sample)/n]
			buf = rend.Line(buf[:0], &rec, f)
			fmt.Fprintf(stdout, "  %s\n", buf)
		}
	}
	return nil
}

// Manifest describes a run directory.
type Manifest struct {
	Simo          string `json:"simo"`
	Engine        string `json:"engine"`
	Backend       string `json:"backend"`
	World         string `json:"world"`
	SeedSHA256    string `json:"seed_sha256"`
	MasterSeed    uint64 `json:"master_seed"`
	Start         string `json:"start"`
	Tick          string `json:"tick"`
	FromTick      int64  `json:"from_tick"`
	ToTick        int64  `json:"to_tick"`
	Devices       int    `json:"devices"`
	Records       int64  `json:"records"`
	RecordsSHA256 string `json:"records_sha256"`
}

func cmdRun(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var wd window
	wd.register(fs, "")
	format := fs.String("format", "text", "line format: text, jsonl or syslog")
	out := fs.String("out", "", "write records.bin, seed, manifest and truth to this directory instead of lines to stdout")
	trace := fs.Bool("trace-states", false, "with --out: also write transitions.jsonl")
	metrics := fs.String("metrics", "", "with --out: also write metrics.jsonl at this step, e.g. 1m")
	truth := fs.Bool("truth", false, "include the ground-truth cause in jsonl lines (never for agents under test)")
	workers := fs.Int("workers", 0, "worker goroutines (0 = all CPUs)")
	db := fs.String("db", "", "build a SQLite world in this directory: checkpoints, rollups, metrics, transitions, truth")
	materialize := fs.Bool("materialize", false, "with --db: also store raw events for every whole day in the window")
	every := fs.String("checkpoint-every", "", "with --db: checkpoint spacing (default 1h)")
	path, err := oneArg(fs, args, "seed file")
	if err != nil {
		return err
	}
	f, err := render.ParseFormat(*format)
	if err != nil {
		return err
	}
	w, raw, err := loadWorld(path, stderr)
	if err != nil {
		return err
	}
	from, to, err := wd.ticks(w)
	if err != nil {
		return err
	}
	if *db != "" {
		if *out != "" || *trace || *metrics != "" {
			return errors.New("--db cannot be combined with --out, --trace-states or --metrics (a world stores transitions and metrics already)")
		}
		return buildWorld(w, raw, *db, from, to, *materialize, *every, *workers, stderr)
	}
	if *materialize || *every != "" {
		return errors.New("--materialize and --checkpoint-every need --db")
	}
	rend, err := render.New(w)
	if err != nil {
		return err
	}
	rend.ShowCause = *truth
	opt := engine.Options{From: from, To: to, Workers: *workers}

	if *out == "" {
		if *trace || *metrics != "" {
			return errors.New("--trace-states and --metrics need --out")
		}
		bw := bufio.NewWriterSize(stdout, 1<<20)
		var buf []byte
		_, err := engine.Run(w, opt, func(recs []record.Record) error {
			for i := range recs {
				buf = rend.Line(buf[:0], &recs[i], f)
				buf = append(buf, '\n')
				if _, err := bw.Write(buf); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		return bw.Flush()
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "seed.yaml"), raw, 0o644); err != nil {
		return err
	}
	closers := []func() error{}
	create := func(name string) (*bufio.Writer, error) {
		fh, err := os.Create(filepath.Join(*out, name))
		if err != nil {
			return nil, err
		}
		bw := bufio.NewWriterSize(fh, 1<<20)
		closers = append(closers, func() error {
			if err := bw.Flush(); err != nil {
				return err
			}
			return fh.Close()
		})
		return bw, nil
	}
	recFile, err := os.Create(filepath.Join(*out, "records.bin"))
	if err != nil {
		return err
	}
	defer recFile.Close()
	hash := sha256.New()
	rw := record.NewWriter(io.MultiWriter(recFile, hash))

	if *trace {
		tw, err := create("transitions.jsonl")
		if err != nil {
			return err
		}
		enc := json.NewEncoder(tw)
		opt.OnTransition = func(t engine.Transition) {
			_ = enc.Encode(transitionJSON(w, t))
		}
	}
	if *metrics != "" {
		step, err := spec.ParseDuration(*metrics)
		if err != nil || step <= 0 {
			return fmt.Errorf("--metrics: want a step like 1m")
		}
		mw, err := create("metrics.jsonl")
		if err != nil {
			return err
		}
		enc := json.NewEncoder(mw)
		opt.MetricEvery = int64(step / w.Tick)
		opt.OnMetric = func(m engine.MetricSample) {
			dev := &w.Devices[m.Device]
			_ = enc.Encode(map[string]any{
				"ts":     w.Start.Add(time.Duration(m.Tick) * w.Tick).Format(time.RFC3339Nano),
				"device": dev.Name,
				"metric": w.Classes[dev.Class].Metrics[m.Metric].Name,
				"value":  m.Value,
			})
		}
	}
	var lw *bufio.Writer
	explicitFormat := false
	fs.Visit(func(fl *flag.Flag) { explicitFormat = explicitFormat || fl.Name == "format" })
	if explicitFormat {
		ext := map[render.Format]string{render.Text: "log", render.JSON: "jsonl", render.Syslog: "syslog"}[f]
		if lw, err = create("logs." + ext); err != nil {
			return err
		}
	}
	var buf []byte
	st, err := engine.Run(w, opt, func(recs []record.Record) error {
		if err := rw.Write(recs); err != nil {
			return err
		}
		if lw != nil {
			for i := range recs {
				buf = rend.Line(buf[:0], &recs[i], f)
				buf = append(buf, '\n')
				if _, err := lw.Write(buf); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	if err := writeTruth(w, filepath.Join(*out, "truth.jsonl")); err != nil {
		return err
	}
	for _, c := range closers {
		if err := c(); err != nil {
			return err
		}
	}
	m := Manifest{
		Simo: version, Engine: ir.EngineVersion, Backend: "cpu", World: w.Name,
		SeedSHA256: w.SeedSHA256, MasterSeed: w.MasterSeed, Start: w.Start.Format(time.RFC3339),
		Tick: w.Tick.String(), FromTick: from, ToTick: to, Devices: len(w.Devices),
		Records: st.Records, RecordsSHA256: hex.EncodeToString(hash.Sum(nil)),
	}
	mj, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "manifest.json"), append(mj, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "wrote %s records (%s simulated) to %s\n", commas(st.Records), fmtSpan(time.Duration(to-from)*w.Tick), *out)
	return nil
}

func transitionJSON(w *ir.World, t engine.Transition) map[string]any {
	dev := &w.Devices[t.Device]
	cls := &w.Classes[dev.Class]
	cause := map[engine.CauseKind]string{engine.CauseRate: "rate", engine.CauseTimer: "timer", engine.CauseOperator: "operator", engine.CauseCommand: "command"}[t.Cause]
	if t.Command >= 0 {
		cause += ":" + cls.Commands[t.Command].Name
	}
	return map[string]any{
		"tick":   t.Tick,
		"ts":     w.Start.Add(time.Duration(t.Tick) * w.Tick).Format(time.RFC3339Nano),
		"device": dev.Name,
		"from":   cls.States[t.From].Name,
		"to":     cls.States[t.To].Name,
		"cause":  cause,
	}
}

func writeTruth(w *ir.World, path string) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(fh)
	for _, f := range w.Faults {
		var devices []string
		for d, in := range f.Member {
			if in {
				devices = append(devices, w.Devices[d].Name)
			}
		}
		if err := enc.Encode(map[string]any{
			"cause":   f.ID,
			"label":   f.Label,
			"target":  f.Target,
			"start":   w.Start.Add(time.Duration(f.Start) * w.Tick).Format(time.RFC3339),
			"end":     w.Start.Add(time.Duration(f.End) * w.Tick).Format(time.RFC3339),
			"devices": devices,
		}); err != nil {
			fh.Close()
			return err
		}
	}
	return fh.Close()
}

func cmdLogs(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var wd window
	wd.register(fs, "")
	format := fs.String("format", "text", "line format: text, jsonl or syslog")
	device := fs.String("device", "", "only this device; a trailing * matches a prefix")
	level := fs.String("level", "", "minimum level: DEBUG, INFO, NOTICE, WARN, ERROR or CRIT")
	event := fs.String("event", "", "only this event id")
	grep := fs.String("grep", "", "only lines whose message contains this text")
	limit := fs.Int("limit", 0, "stop after this many lines")
	truth := fs.Bool("truth", false, "include the ground-truth cause in jsonl lines")
	mode := fs.String("mode", "auto", "worlds only: auto, sql (stored days only) or regen (always regenerate)")
	explain := fs.Bool("explain", false, "worlds only: report which parts were read from storage and which were regenerated")
	dir, err := oneArg(fs, args, "run directory")
	if err != nil {
		return err
	}
	f, err := render.ParseFormat(*format)
	if err != nil {
		return err
	}
	if isWorld(dir) {
		return logsWorld(dir, wd, f, *device, *level, *event, *grep, *mode, *limit, *truth, *explain, stdout, stderr)
	}
	var m Manifest
	mj, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("%s is not a run directory (no manifest.json): %w", dir, err)
	}
	if err := json.Unmarshal(mj, &m); err != nil {
		return err
	}
	if m.Engine != ir.EngineVersion {
		fmt.Fprintf(stderr, "warning: records were written by engine %s, this is %s\n", m.Engine, ir.EngineVersion)
	}
	w, _, err := loadWorld(filepath.Join(dir, "seed.yaml"), io.Discard)
	if err != nil {
		return err
	}
	if w.SeedSHA256 != m.SeedSHA256 {
		return errors.New("seed.yaml does not match the manifest")
	}
	rend, err := render.New(w)
	if err != nil {
		return err
	}
	rend.ShowCause = *truth
	minLevel := 0
	if *level != "" {
		if minLevel = spec.LevelIndex(*level); minLevel < 0 {
			return fmt.Errorf("unknown level %q", *level)
		}
	}
	var fromNs, toNs int64 = 0, 1<<63 - 1
	if wd.from != "" || wd.to != "" || wd.dur != "" {
		from, to, err := wd.ticks(w)
		if err != nil {
			return err
		}
		fromNs, toNs = from*w.TickNs, to*w.TickNs
	}
	fh, err := os.Open(filepath.Join(dir, "records.bin"))
	if err != nil {
		return err
	}
	defer fh.Close()
	r := record.NewReader(fh)
	bw := bufio.NewWriterSize(stdout, 1<<20)
	defer bw.Flush()
	var buf []byte
	shown := 0
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if rec.TS < fromNs || rec.TS >= toNs || int(rec.Level) < minLevel {
			continue
		}
		if *event != "" && w.Events[rec.Template].ID != *event {
			continue
		}
		if *device != "" {
			name := w.Devices[rec.Device].Name
			if p, ok := strings.CutSuffix(*device, "*"); ok {
				if !strings.HasPrefix(name, p) {
					continue
				}
			} else if name != *device {
				continue
			}
		}
		if *grep != "" {
			buf = rend.Message(buf[:0], &rec)
			if !strings.Contains(string(buf), *grep) {
				continue
			}
		}
		buf = rend.Line(buf[:0], &rec, f)
		buf = append(buf, '\n')
		if _, err := bw.Write(buf); err != nil {
			return err
		}
		shown++
		if *limit > 0 && shown >= *limit {
			return nil
		}
	}
}
