package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/spec"
)

// Mode selects how Events reads.
type Mode int

// Read modes.
const (
	Auto  Mode = iota // SQL for materialized days, regeneration for the rest
	SQL               // only materialized days; error otherwise
	Regen             // always regenerate from the nearest checkpoint
)

// ParseMode parses auto, sql or regen.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(s) {
	case "auto", "":
		return Auto, nil
	case "sql":
		return SQL, nil
	case "regen", "regenerate":
		return Regen, nil
	}
	return 0, fmt.Errorf("unknown mode %q (want auto, sql or regen)", s)
}

// ErrStop can be returned by an Events callback to stop early; Events then
// returns nil.
var ErrStop = errors.New("stop")

// Query selects events.
type Query struct {
	FromNs, ToNs int64    // [from, to) in ns since the start; ToNs 0 = end of run
	Devices      []uint32 // nil = all
	Templates    []uint16 // nil = all
	MinLevel     uint8
	Mode         Mode
	Workers      int
}

// Segment reports how one part of a query was answered.
type Segment struct {
	FromNs, ToNs int64
	FromSQL      bool
}

// Events streams matching records in engine order. It answers each simulated
// day from its materialized file when there is one (and the mode allows),
// and regenerates the rest from the nearest checkpoint. It returns the
// segments it used.
func (s *Store) Events(q Query, fn func(*record.Record) error) ([]Segment, error) {
	w := s.World
	end := w.Ticks * w.TickNs
	if q.ToNs <= 0 || q.ToNs > end {
		q.ToNs = end
	}
	if q.FromNs < 0 || q.FromNs >= q.ToNs {
		return nil, fmt.Errorf("empty window [%d, %d) ns", q.FromNs, q.ToNs)
	}
	mat, err := s.MaterializedDays()
	if err != nil {
		return nil, err
	}
	// Split the window by day, then merge neighbouring regenerated parts.
	var segs []Segment
	for d := int(q.FromNs / dayNs); d < Days(w); d++ {
		a, b := daySpan(w, d)
		a, b = max(a, q.FromNs), min(b, q.ToNs)
		if a >= b {
			break
		}
		useSQL := mat[d] && q.Mode != Regen
		if q.Mode == SQL && !mat[d] {
			return nil, fmt.Errorf("day %d is not materialized (run simo materialize, or use mode auto)", d+1)
		}
		if n := len(segs); n > 0 && !useSQL && !segs[n-1].FromSQL {
			segs[n-1].ToNs = b
			continue
		}
		segs = append(segs, Segment{a, b, useSQL})
	}

	f := newFilter(q)
	for _, seg := range segs {
		var err error
		if seg.FromSQL {
			err = s.sqlEvents(int(seg.FromNs/dayNs), seg, q, f, fn)
		} else {
			err = s.regenEvents(seg, q, f, fn)
		}
		if errors.Is(err, ErrStop) {
			return segs, nil
		}
		if err != nil {
			return segs, err
		}
	}
	return segs, nil
}

type filter struct {
	devices   map[uint32]bool
	templates map[uint16]bool
	minLevel  uint8
}

func newFilter(q Query) *filter {
	f := &filter{minLevel: q.MinLevel}
	if q.Devices != nil {
		f.devices = map[uint32]bool{}
		for _, d := range q.Devices {
			f.devices[d] = true
		}
	}
	if q.Templates != nil {
		f.templates = map[uint16]bool{}
		for _, t := range q.Templates {
			f.templates[t] = true
		}
	}
	return f
}

func (f *filter) match(r *record.Record) bool {
	return r.Level >= f.minLevel &&
		(f.devices == nil || f.devices[r.Device]) &&
		(f.templates == nil || f.templates[r.Template])
}

func (s *Store) regenEvents(seg Segment, q Query, f *filter, fn func(*record.Record) error) error {
	w := s.World
	fromTick := seg.FromNs / w.TickNs
	toTick := min((seg.ToNs+w.TickNs-1)/w.TickNs, w.Ticks)
	cp, err := s.Checkpoint(fromTick)
	if err != nil {
		return err
	}
	_, err = engine.Run(w, engine.Options{Start: cp, From: fromTick, To: toTick, Workers: q.Workers}, func(recs []record.Record) error {
		for i := range recs {
			r := &recs[i]
			if r.TS < seg.FromNs || r.TS >= seg.ToNs || !f.match(r) {
				continue
			}
			if err := fn(r); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func (s *Store) dayDB(day int) (*sql.DB, error) {
	if db, ok := s.days[day]; ok {
		return db, nil
	}
	db, err := openDB(filepath.Join(s.Dir, dayFile(day)), false)
	if err != nil {
		return nil, err
	}
	s.days[day] = db
	return db, nil
}

func (s *Store) sqlEvents(day int, seg Segment, q Query, f *filter, fn func(*record.Record) error) error {
	db, err := s.dayDB(day)
	if err != nil {
		return err
	}
	where := []string{"ts >= ?", "ts < ?", "level >= ?"}
	args := []any{seg.FromNs, seg.ToNs, q.MinLevel}
	if q.Devices != nil {
		where = append(where, "device IN ("+placeholders(len(q.Devices))+")")
		for _, d := range q.Devices {
			args = append(args, d)
		}
	}
	if q.Templates != nil {
		where = append(where, "template IN ("+placeholders(len(q.Templates))+")")
		for _, t := range q.Templates {
			args = append(args, t)
		}
	}
	rows, err := db.Query("SELECT ts, device, template, level, flags, trace, span, parent, s0, s1, s2, s3, s4, cause, skew FROM events WHERE "+
		strings.Join(where, " AND ")+" ORDER BY ts, rowid", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r record.Record
		var trace int64
		if err := rows.Scan(&r.TS, &r.Device, &r.Template, &r.Level, &r.Flags, &trace, &r.Span, &r.Parent,
			&r.Slots[0], &r.Slots[1], &r.Slots[2], &r.Slots[3], &r.Slots[4], &r.Cause, &r.Skew); err != nil {
			return err
		}
		r.Trace = uint64(trace)
		if !f.match(&r) {
			continue
		}
		if err := fn(&r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Checkpoint returns the latest checkpoint at or before tick, or nil if
// there is none (the engine then starts from tick 0).
func (s *Store) Checkpoint(tick int64) (*engine.Checkpoint, error) {
	var blob []byte
	err := s.db.QueryRow("SELECT state FROM checkpoints WHERE tick <= ? ORDER BY tick DESC LIMIT 1", tick).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return engine.UnmarshalCheckpoint(blob)
}

// MaterializedDays returns the set of 0-based days with stored raw events.
func (s *Store) MaterializedDays() (map[int]bool, error) {
	rows, err := s.db.Query("SELECT day FROM materialized")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int]bool{}
	for rows.Next() {
		var d int
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		m[d-1] = true
	}
	return m, rows.Err()
}

// Materialize stores raw events for every day fully inside [fromNs, toNs)
// that is not stored yet. Each run of consecutive days is generated in one
// engine pass from the nearest checkpoint, and checkpoints taken on the way
// are saved, so later reads past the build window start closer. It returns
// the days (0-based) it wrote.
func (s *Store) Materialize(fromNs, toNs int64, workers int) ([]int, error) {
	w := s.World
	end := w.Ticks * w.TickNs
	if toNs <= 0 || toNs > end {
		toNs = end
	}
	mat, err := s.MaterializedDays()
	if err != nil {
		return nil, err
	}
	var runs [][2]int // inclusive day ranges
	for d := 0; d < Days(w); d++ {
		a, b := daySpan(w, d)
		if a < fromNs || b > toNs || mat[d] {
			continue
		}
		if n := len(runs); n > 0 && runs[n-1][1] == d-1 {
			runs[n-1][1] = d
		} else {
			runs = append(runs, [2]int{d, d})
		}
	}
	if err := os.MkdirAll(filepath.Join(s.Dir, EventsDir), 0o755); err != nil {
		return nil, err
	}
	register := func(day int, n int64, sha string) error {
		_, err := s.db.Exec("INSERT OR REPLACE INTO materialized (day, file, records, sha256) VALUES (?, ?, ?, ?)", day+1, dayFile(day), n, sha)
		return err
	}
	var done []int
	for _, r := range runs {
		a, _ := daySpan(w, r[0])
		_, b := daySpan(w, r[1])
		fromTick, toTick := a/w.TickNs, min((b+w.TickNs-1)/w.TickNs, w.Ticks)
		cp, err := s.Checkpoint(fromTick)
		if err != nil {
			return done, err
		}
		ds := newDayStream(s.Dir, w, a, b, register)
		var cerr error
		_, err = engine.Run(w, engine.Options{
			Start: cp, From: fromTick, To: toTick, Workers: workers,
			CheckpointEvery: s.CheckpointEvery,
			OnCheckpoint: func(c *engine.Checkpoint) {
				blob, err := c.MarshalBinary()
				if err == nil {
					_, err = s.db.Exec("INSERT OR IGNORE INTO checkpoints (tick, state) VALUES (?, ?)", c.Tick, blob)
				}
				if cerr == nil {
					cerr = err
				}
			},
		}, func(recs []record.Record) error {
			for i := range recs {
				if recs[i].TS >= a && recs[i].TS < b {
					if err := ds.add(&recs[i]); err != nil {
						return err
					}
				}
			}
			return cerr
		})
		if err != nil {
			return done, err
		}
		if cerr != nil {
			return done, cerr
		}
		days, err := ds.finish()
		done = append(done, days...)
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// RollupQuery selects per-minute event counts.
type RollupQuery struct {
	FromMin, ToMin int64    // [from, to) in minutes since the start; ToMin 0 = all
	StepMin        int64    // bucket size in minutes; 0 = one bucket for the whole window
	By             []string // any of: event, level, device, site, class
	Devices        []uint32 // nil = all
}

// RollupRow is one bucket of counts.
type RollupRow struct {
	Minute int64    // start of the bucket, minutes since the start
	Keys   []string // values of the By columns
	N      int64
}

var rollupColumns = map[string]string{
	"event":  "c.name || '/' || t.event",
	"level":  "t.level",
	"device": "d.name",
	"site":   "d.site",
	"class":  "c.name",
}

// RollupKeys lists the accepted RollupQuery.By values.
var RollupKeys = []string{"event", "level", "device", "site", "class"}

// Rollup aggregates the stored 1-minute counts. It covers the build window
// only.
func (s *Store) Rollup(q RollupQuery) ([]RollupRow, error) {
	if q.ToMin <= 0 {
		q.ToMin = 1 << 50
	}
	step := q.StepMin
	if step <= 0 {
		step = 1 << 50
	}
	var cols []string
	for _, b := range q.By {
		c, ok := rollupColumns[b]
		if !ok {
			return nil, fmt.Errorf("cannot group by %q (want %s)", b, strings.Join(RollupKeys, ", "))
		}
		cols = append(cols, c)
	}
	sel := "((r.minute - ?) / ?) * ? + ? AS bucket"
	group := "bucket"
	if len(cols) > 0 {
		sel += ", " + strings.Join(cols, ", ")
		group += ", " + strings.Join(cols, ", ")
	}
	args := []any{q.FromMin, step, step, q.FromMin, q.FromMin, q.ToMin}
	where := "r.minute >= ? AND r.minute < ?"
	if q.Devices != nil {
		where += " AND r.device IN (" + placeholders(len(q.Devices)) + ")"
		for _, d := range q.Devices {
			args = append(args, d)
		}
	}
	query := "SELECT " + sel + ", SUM(r.n) FROM rollup_1m r JOIN templates t ON t.id = r.template JOIN devices d ON d.id = r.device JOIN classes c ON c.id = d.class WHERE " +
		where + " GROUP BY " + group + " ORDER BY " + group
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RollupRow
	for rows.Next() {
		vals := make([]any, len(cols)+2)
		var bucket, n int64
		vals[0] = &bucket
		keys := make([]sql.NullString, len(cols))
		for i := range keys {
			vals[i+1] = &keys[i]
		}
		vals[len(vals)-1] = &n
		if err := rows.Scan(vals...); err != nil {
			return nil, err
		}
		row := RollupRow{Minute: bucket, N: n}
		for i, k := range keys {
			v := k.String
			if q.By[i] == "level" {
				if l, err := strconv.Atoi(v); err == nil && l >= 0 && l < len(spec.Levels) {
					v = spec.Levels[l]
				}
			}
			row.Keys = append(row.Keys, v)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// MetricRow is one bucket of a metric series.
type MetricRow struct {
	Minute        int64
	Min, Avg, Max float64
}

// Metrics returns a device's metric aggregated into step-minute buckets.
func (s *Store) Metrics(device uint32, metric int, fromMin, toMin, stepMin int64) ([]MetricRow, error) {
	if toMin <= 0 {
		toMin = 1 << 50
	}
	if stepMin <= 0 {
		stepMin = 1
	}
	rows, err := s.db.Query(`SELECT ((minute - ?) / ?) * ? + ? AS bucket, MIN(vmin), AVG(vavg), MAX(vmax)
		FROM metrics_1m WHERE device = ? AND metric = ? AND minute >= ? AND minute < ?
		GROUP BY bucket ORDER BY bucket`, fromMin, stepMin, stepMin, fromMin, device, metric, fromMin, toMin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricRow
	for rows.Next() {
		var r MetricRow
		if err := rows.Scan(&r.Minute, &r.Min, &r.Avg, &r.Max); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MinuteOf converts ns since the start to whole minutes.
func MinuteOf(ns int64) int64 { return ns / int64(time.Minute) }
