// Package report holds what the CLI and the MCP server both print: time
// parsing for windows, the preview summary, and compact text tables. Output
// is plain text, one item per line, so it reads well for people and costs
// agents little context.
package report

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/render"
	"github.com/taituo/simo/internal/spec"
	"github.com/taituo/simo/internal/store"
)

// Offset prints an offset from the start of the run as "dayN HH:MM:SS".
func Offset(d time.Duration) string {
	day := int(d / (24 * time.Hour))
	rest := d - time.Duration(day)*24*time.Hour
	h, m, s := int(rest.Hours()), int(rest.Minutes())%60, int(rest.Seconds())%60
	return fmt.Sprintf("day%d %02d:%02d:%02d", day+1, h, m, s)
}

// Span prints a length of time: "7d", "1h0m0s", "2d 3h0m0s".
func Span(d time.Duration) string {
	day := 24 * time.Hour
	switch {
	case d >= day && d%day == 0:
		return fmt.Sprintf("%dd", d/day)
	case d >= day:
		return fmt.Sprintf("%dd %s", d/day, d%day)
	}
	return d.String()
}

// Commas prints an integer with thousands separators.
func Commas(n int64) string {
	if n < 0 {
		return "-" + Commas(-n)
	}
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Plural prints "1 device" or "3 devices".
func Plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	if strings.HasSuffix(word, "s") {
		return fmt.Sprintf("%d %ses", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Clock prints the wall time of a minute offset, "2006-01-02 15:04" UTC.
func Clock(w *ir.World, minute int64) string {
	return w.Start.Add(time.Duration(minute) * time.Minute).Format("2006-01-02 15:04")
}

// ParseTime parses a time within the run and returns its offset from the
// start: "day2 09:15", "09:15", an offset like "26h", or an absolute RFC 3339
// timestamp such as one copied from a log line.
func ParseTime(w *ir.World, s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Sub(w.Start), nil
	}
	return spec.ParseTimePoint(s)
}

// Window resolves from, to and a length (for) to a [from, to) tick window.
// An empty from is the start of the run; with neither to nor for, the window
// runs to the end.
func Window(w *ir.World, from, to, dur string) (int64, int64, error) {
	var f, t time.Duration
	var err error
	if from != "" {
		if f, err = ParseTime(w, from); err != nil {
			return 0, 0, fmt.Errorf("from: %w", err)
		}
	}
	end := time.Duration(w.Ticks) * w.Tick
	t = end
	switch {
	case to != "":
		if t, err = ParseTime(w, to); err != nil {
			return 0, 0, fmt.Errorf("to: %w", err)
		}
	case dur != "":
		d, err := spec.ParseDuration(dur)
		if err != nil {
			return 0, 0, fmt.Errorf("for: %w", err)
		}
		t = f + d
	}
	f, t = max(f, 0), min(t, end)
	if f >= t {
		return 0, 0, fmt.Errorf("empty window: %s to %s (the run covers %s to %s)", Offset(f), Offset(t), Offset(0), Offset(end))
	}
	return int64(f / w.Tick), int64((t + w.Tick - 1) / w.Tick), nil
}

// PreviewOptions control Preview.
type PreviewOptions struct {
	From, To int64 // tick window
	Lines    int   // sample lines to show
	Format   render.Format
	Workers  int
}

// Preview simulates a window and writes a summary: record counts per event,
// time in state per class, and evenly spaced sample lines.
func Preview(w *ir.World, opt PreviewOptions, out io.Writer) error {
	const keep = 500000
	var sample []record.Record
	st, err := engine.Run(w, engine.Options{From: opt.From, To: opt.To, Workers: opt.Workers}, func(recs []record.Record) error {
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
	span := time.Duration(opt.To-opt.From) * w.Tick
	fmt.Fprintf(out, "%s: %s in %s, tick %s, previewing %s from %s\n", w.Name, Plural(len(w.Devices), "device"),
		Plural(len(w.Classes), "class"), w.Tick, Span(span), Offset(time.Duration(opt.From)*w.Tick))
	fmt.Fprintf(out, "records: %s (%.1f per second), transitions: %s\n\n", Commas(st.Records),
		float64(st.Records)/span.Seconds(), Commas(st.Transitions))

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "event\tlevel\tcount\tper hour\t")
	for e, n := range st.ByEvent {
		ev := &w.Events[e]
		fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%.0f\t\n", w.Classes[ev.Class].Name, ev.ID, spec.Levels[ev.Level], Commas(n), float64(n)/span.Hours())
	}
	tw.Flush()

	fmt.Fprintln(out, "\ntime in state:")
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
		fmt.Fprintf(out, "  %s: %s\n", cls.Name, strings.Join(parts, ", "))
	}

	if opt.Lines > 0 && len(sample) > 0 {
		fmt.Fprintln(out, "\nsample lines:")
		n := min(opt.Lines, len(sample))
		var buf []byte
		for i := 0; i < n; i++ {
			rec := sample[i*len(sample)/n]
			buf = rend.Line(buf[:0], &rec, opt.Format)
			fmt.Fprintf(out, "  %s\n", buf)
		}
	}
	return nil
}

// RollupTable writes rollup rows with a column per By key.
func RollupTable(w *ir.World, by []string, rows []store.RollupRow, out io.Writer) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(append(append([]string{"from"}, by...), "count"), "\t"))
	for _, r := range rows {
		cells := append([]string{Clock(w, r.Minute)}, r.Keys...)
		fmt.Fprintln(tw, strings.Join(append(cells, Commas(r.N)), "\t"))
	}
	return tw.Flush()
}

// MetricSeries is one metric's rows.
type MetricSeries struct {
	Name string
	Unit string
	Rows []store.MetricRow
}

// MetricsTable writes min, avg and max per bucket.
func MetricsTable(w *ir.World, series []MetricSeries, out io.Writer) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "from\tmetric\tmin\tavg\tmax\t")
	for _, s := range series {
		for _, r := range s.Rows {
			fmt.Fprintf(tw, "%s\t%s\t%.2f\t%.2f\t%.2f\t\n", Clock(w, r.Minute), s.Name, r.Min, r.Avg, r.Max)
		}
	}
	return tw.Flush()
}

// DevicesTable writes the given devices.
func DevicesTable(w *ir.World, devices []ir.Device, out io.Writer) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "device\tclass\tsite")
	for _, d := range devices {
		fmt.Fprintf(tw, "%s\t%s\t%d\n", d.Name, w.Classes[d.Class].Name, d.Site)
	}
	return tw.Flush()
}

// TruthTable writes the scheduled faults: the answer key.
func TruthTable(w *ir.World, out io.Writer) error {
	if len(w.Faults) == 0 {
		_, err := fmt.Fprintln(out, "no scheduled faults")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "cause\tstart\tend\ttarget\tdevices\tlabel")
	for _, f := range w.Faults {
		n := 0
		for _, in := range f.Member {
			if in {
				n++
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%s\n", f.ID, Offset(time.Duration(f.Start)*w.Tick), Offset(time.Duration(f.End)*w.Tick), f.Target, n, f.Label)
	}
	return tw.Flush()
}

// DescribeWorld writes what an observer may know about a world: its time
// range, fleet, event catalog and metrics. It reveals no hidden state.
func DescribeWorld(st *store.Store, out io.Writer) error {
	w := st.World
	end := time.Duration(w.Ticks) * w.Tick
	fmt.Fprintf(out, "world %s\n", w.Name)
	fmt.Fprintf(out, "time: %s to %s (%s, tick %s)\n", w.Start.Format(time.RFC3339), w.Start.Add(end).Format(time.RFC3339), Span(end), w.Tick)
	fmt.Fprintf(out, "counts and metrics cover: %s to %s\n", Offset(time.Duration(st.BuildFrom)*w.Tick), Offset(time.Duration(st.BuildTo)*w.Tick))
	if days, err := st.MaterializedDays(); err == nil && len(days) > 0 {
		fmt.Fprintf(out, "stored days: %d of %d (others are regenerated on read)\n", len(days), store.Days(w))
	}
	fmt.Fprintf(out, "\ndevices: %d\n", len(w.Devices))
	for ci, c := range w.Classes {
		n, sites := 0, map[int]bool{}
		for _, d := range w.Devices {
			if d.Class == ci {
				n++
				sites[d.Site] = true
			}
		}
		where := ""
		if len(sites) > 1 || !sites[0] {
			where = fmt.Sprintf(" across %s", Plural(len(sites), "site"))
		}
		fmt.Fprintf(out, "  %s: %d%s\n", c.Name, n, where)
	}
	fmt.Fprintln(out, "\nevents (class/event, level, message template):")
	for _, e := range w.Events {
		fmt.Fprintf(out, "  %s/%s %s %q\n", w.Classes[e.Class].Name, e.ID, spec.Levels[e.Level], e.Tmpl.Text)
	}
	hasMetrics := false
	for _, c := range w.Classes {
		for _, m := range c.Metrics {
			if !hasMetrics {
				fmt.Fprintln(out, "\nmetrics (class/metric, unit):")
				hasMetrics = true
			}
			fmt.Fprintf(out, "  %s/%s %s\n", c.Name, m.Name, m.Unit)
		}
	}
	return nil
}
