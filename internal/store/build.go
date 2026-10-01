package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
)

// BuildOptions control Build.
type BuildOptions struct {
	// From and To bound the window in ticks; To 0 means the whole run.
	From, To int64
	// CheckpointEvery is the number of ticks between checkpoints; 0 means
	// one simulated hour.
	CheckpointEvery int64
	// Materialize stores raw events for every day fully inside the window.
	Materialize bool
	Workers     int
}

// BuildResult summarises a build.
type BuildResult struct {
	Stats            *engine.Stats
	Checkpoints      int
	Rollups          int64
	MaterializedDays []int // 0-based
}

// Build simulates a world into a new directory dir.
func Build(dir string, w *ir.World, seed []byte, opt BuildOptions) (*BuildResult, error) {
	if _, err := os.Stat(filepath.Join(dir, WorldFile)); err == nil {
		return nil, fmt.Errorf("%s already holds a world; remove it or pick another directory", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, EventsDir), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, SeedFile), seed, 0o644); err != nil {
		return nil, err
	}
	to := opt.To
	if to <= 0 || to > w.Ticks {
		to = w.Ticks
	}
	if opt.From < 0 || opt.From >= to {
		return nil, fmt.Errorf("empty build window [%d, %d)", opt.From, to)
	}
	every := opt.CheckpointEvery
	if every <= 0 {
		every = max(1, int64(time.Hour/w.Tick))
	}

	// Write into a temporary file and rename at the end, so a failed build
	// never leaves a world.db that looks complete.
	tmp := filepath.Join(dir, WorldFile+".partial")
	os.Remove(tmp)
	db, err := openDB(tmp, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.Exec(worldSchema); err != nil {
		return nil, err
	}
	if err := writeCatalog(db, w, opt.From, to, every); err != nil {
		return nil, err
	}

	wr, err := newBatchWriter(db, map[string]string{
		"rollup":       "INSERT INTO rollup_1m (minute, device, template, n) VALUES (?, ?, ?, ?)",
		"metric":       "INSERT INTO metrics_1m (device, metric, minute, vmin, vavg, vmax) VALUES (?, ?, ?, ?, ?, ?)",
		"transition":   "INSERT INTO transitions (tick, device, from_state, to_state, cause) VALUES (?, ?, ?, ?, ?)",
		"checkpoint":   "INSERT INTO checkpoints (tick, state) VALUES (?, ?)",
		"materialized": "INSERT OR REPLACE INTO materialized (day, file, records, sha256) VALUES (?, ?, ?, ?)",
	})
	if err != nil {
		return nil, err
	}
	res := &BuildResult{}
	var werr error // first error from a callback
	fail := func(err error) {
		if werr == nil && err != nil {
			werr = err
		}
	}

	roll := newRollup()
	var days *dayStream
	if opt.Materialize {
		days = newDayStream(dir, w, opt.From*w.TickNs, to*w.TickNs, func(day int, n int64, sha string) error {
			return wr.exec("materialized", day+1, dayFile(day), n, sha)
		})
	}

	eopt := engine.Options{
		From: opt.From, To: to, Workers: opt.Workers,
		CheckpointEvery: every,
		OnCheckpoint: func(cp *engine.Checkpoint) {
			b, err := cp.MarshalBinary()
			if err == nil {
				err = wr.exec("checkpoint", cp.Tick, b)
			}
			fail(err)
			res.Checkpoints++
		},
		OnTransition: func(t engine.Transition) {
			fail(wr.exec("transition", t.Tick, t.Device, t.From, t.To, CauseString(w, t)))
		},
		MetricEvery: max(1, int64(time.Minute/w.Tick)),
		OnMetric: func(m engine.MetricSample) {
			minute := m.Tick * w.TickNs / int64(time.Minute)
			fail(wr.exec("metric", m.Device, m.Metric, minute, m.Value, m.Value, m.Value))
		},
	}
	st, err := engine.Run(w, eopt, func(recs []record.Record) error {
		for i := range recs {
			roll.add(&recs[i])
			if days != nil {
				if err := days.add(&recs[i]); err != nil {
					return err
				}
			}
		}
		n, err := roll.flush(wr, recs[len(recs)-1].TS/int64(time.Minute))
		res.Rollups += n
		if err != nil {
			return err
		}
		return werr
	})
	if err != nil {
		return nil, err
	}
	if werr != nil {
		return nil, werr
	}
	n, err := roll.flush(wr, 1<<62)
	res.Rollups += n
	if err != nil {
		return nil, err
	}
	if days != nil {
		if res.MaterializedDays, err = days.finish(); err != nil {
			return nil, err
		}
	}
	if err := wr.commit(); err != nil {
		return nil, err
	}
	if _, err := db.Exec("INSERT INTO meta (key, value) VALUES ('records', ?)", st.Records); err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, WorldFile)); err != nil {
		return nil, err
	}
	res.Stats = st
	return res, nil
}

// CauseString names a transition's cause: rate, timer, operator:<cmd> or
// command:<cmd>.
func CauseString(w *ir.World, t engine.Transition) string {
	s := map[engine.CauseKind]string{engine.CauseRate: "rate", engine.CauseTimer: "timer", engine.CauseOperator: "operator", engine.CauseCommand: "command"}[t.Cause]
	if t.Command >= 0 {
		s += ":" + w.Classes[w.Devices[t.Device].Class].Commands[t.Command].Name
	}
	return s
}

func writeCatalog(db *sql.DB, w *ir.World, from, to, every int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ex := func(q string, args ...any) {
		if err == nil {
			_, err = tx.Exec(q, args...)
		}
	}
	meta := map[string]string{
		"schema":           strconv.Itoa(SchemaVersion),
		"engine":           ir.EngineVersion,
		"world":            w.Name,
		"seed_sha256":      w.SeedSHA256,
		"master_seed":      strconv.FormatUint(w.MasterSeed, 10),
		"start":            w.Start.Format(time.RFC3339Nano),
		"tick_ns":          strconv.FormatInt(w.TickNs, 10),
		"ticks":            strconv.FormatInt(w.Ticks, 10),
		"build_from":       strconv.FormatInt(from, 10),
		"build_to":         strconv.FormatInt(to, 10),
		"checkpoint_every": strconv.FormatInt(every, 10),
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		ex("INSERT INTO meta (key, value) VALUES (?, ?)", k, meta[k])
	}
	for ci, c := range w.Classes {
		ex("INSERT INTO classes (id, name) VALUES (?, ?)", ci, c.Name)
		for si, st := range c.States {
			ex("INSERT INTO states (class, id, name) VALUES (?, ?, ?)", ci, si, st.Name)
		}
		for mi, m := range c.Metrics {
			ex("INSERT INTO metrics (class, id, name, unit) VALUES (?, ?, ?, ?)", ci, mi, m.Name, m.Unit)
		}
	}
	for _, d := range w.Devices {
		ex("INSERT INTO devices (id, name, class, site, idx) VALUES (?, ?, ?, ?, ?)", d.ID, d.Name, d.Class, d.Site, d.Index)
	}
	for ei, e := range w.Events {
		ex("INSERT INTO templates (id, class, event, level, text, tags) VALUES (?, ?, ?, ?, ?, ?)", ei, e.Class, e.ID, e.Level, e.Tmpl.Text, strings.Join(e.Tags, ","))
	}
	for _, f := range w.Faults {
		ex("INSERT INTO truth (cause, label, target, start_tick, end_tick) VALUES (?, ?, ?, ?, ?)", f.ID, f.Label, f.Target, f.Start, f.End)
		for d, in := range f.Member {
			if in {
				ex("INSERT INTO truth_devices (cause, device) VALUES (?, ?)", f.ID, d)
			}
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// batchWriter runs prepared inserts inside a transaction and commits every
// commitEvery rows.
type batchWriter struct {
	db    *sql.DB
	sqls  map[string]string
	tx    *sql.Tx
	stmts map[string]*sql.Stmt
	rows  int
}

const commitEvery = 200000

func newBatchWriter(db *sql.DB, sqls map[string]string) (*batchWriter, error) {
	w := &batchWriter{db: db, sqls: sqls}
	return w, w.begin()
}

func (w *batchWriter) begin() error {
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	w.tx = tx
	w.stmts = map[string]*sql.Stmt{}
	for name, q := range w.sqls {
		st, err := tx.Prepare(q)
		if err != nil {
			return err
		}
		w.stmts[name] = st
	}
	return nil
}

func (w *batchWriter) exec(name string, args ...any) error {
	if _, err := w.stmts[name].Exec(args...); err != nil {
		return fmt.Errorf("store %s: %w", name, err)
	}
	w.rows++
	if w.rows%commitEvery == 0 {
		if err := w.commit(); err != nil {
			return err
		}
		return w.begin()
	}
	return nil
}

func (w *batchWriter) commit() error {
	for _, st := range w.stmts {
		st.Close()
	}
	return w.tx.Commit()
}

// rollup accumulates per-minute event counts per device and template.
type rollKey struct {
	minute   int64
	device   uint32
	template uint16
}

type rollup struct{ acc map[rollKey]int64 }

func newRollup() *rollup { return &rollup{acc: map[rollKey]int64{}} }

func (r *rollup) add(rec *record.Record) {
	r.acc[rollKey{rec.TS / int64(time.Minute), rec.Device, rec.Template}]++
}

// flush writes every minute before `before`; records arrive in time order,
// so those minutes are complete.
func (r *rollup) flush(w *batchWriter, before int64) (int64, error) {
	var keys []rollKey
	for k := range r.acc {
		if k.minute < before {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b rollKey) int {
		if a.minute != b.minute {
			return int(a.minute - b.minute)
		}
		if a.device != b.device {
			return int(a.device) - int(b.device)
		}
		return int(a.template) - int(b.template)
	})
	for _, k := range keys {
		if err := w.exec("rollup", k.minute, k.device, k.template, r.acc[k]); err != nil {
			return 0, err
		}
		delete(r.acc, k)
	}
	return int64(len(keys)), nil
}

// dayWriter writes one simulated day's raw events into its own file.
type dayWriter struct {
	day  int
	path string
	db   *sql.DB
	bw   *batchWriter
	n    int64
	hash hash.Hash
	buf  [record.Size]byte
}

func newDayWriter(dir string, day int) (*dayWriter, error) {
	path := filepath.Join(dir, dayFile(day))
	os.Remove(path)
	db, err := openDB(path, true)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(eventsSchema); err != nil {
		db.Close()
		return nil, err
	}
	bw, err := newBatchWriter(db, map[string]string{
		"event": "INSERT INTO events (ts, device, template, level, flags, trace, span, parent, s0, s1, s2, s3, s4, cause, skew) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &dayWriter{day: day, path: path, db: db, bw: bw, hash: sha256.New()}, nil
}

func (d *dayWriter) add(r *record.Record) error {
	r.Encode(d.buf[:])
	d.hash.Write(d.buf[:])
	d.n++
	return d.bw.exec("event", r.TS, r.Device, r.Template, r.Level, r.Flags, int64(r.Trace), r.Span, r.Parent,
		r.Slots[0], r.Slots[1], r.Slots[2], r.Slots[3], r.Slots[4], r.Cause, r.Skew)
}

// registerFunc records a finished day (0-based) in world.db.
type registerFunc func(day int, records int64, sha256 string) error

// finish commits, indexes and registers the day.
func (d *dayWriter) finish(register registerFunc) error {
	if err := d.bw.commit(); err != nil {
		return err
	}
	if _, err := d.db.Exec(eventsIndexes); err != nil {
		return err
	}
	if err := d.db.Close(); err != nil {
		return err
	}
	return register(d.day, d.n, hex.EncodeToString(d.hash.Sum(nil)))
}

// dayStream routes a time-ordered record stream into day files, for the days
// that lie fully inside [fromNs, toNs).
type dayStream struct {
	dir      string
	w        *ir.World
	register registerFunc
	covered  []int // days to materialize, ascending
	next     int   // index into covered of the next day to open
	cur      *dayWriter
	done     []int
}

func newDayStream(dir string, w *ir.World, fromNs, toNs int64, register registerFunc) *dayStream {
	ds := &dayStream{dir: dir, w: w, register: register}
	for d := 0; d < Days(w); d++ {
		a, b := daySpan(w, d)
		if a >= fromNs && b <= toNs {
			ds.covered = append(ds.covered, d)
		}
	}
	return ds
}

// advance finishes days before `day` and opens `day` if it is covered.
func (ds *dayStream) advance(day int) error {
	if ds.cur != nil && ds.cur.day < day {
		if err := ds.cur.finish(ds.register); err != nil {
			return err
		}
		ds.done = append(ds.done, ds.cur.day)
		ds.cur = nil
	}
	for ds.next < len(ds.covered) && ds.covered[ds.next] <= day {
		d := ds.covered[ds.next]
		ds.next++
		dw, err := newDayWriter(ds.dir, d)
		if err != nil {
			return err
		}
		if d < day { // a covered day with no records at all
			if err := dw.finish(ds.register); err != nil {
				return err
			}
			ds.done = append(ds.done, d)
			continue
		}
		ds.cur = dw
	}
	return nil
}

func (ds *dayStream) add(r *record.Record) error {
	day := int(r.TS / dayNs)
	if ds.cur == nil || ds.cur.day != day {
		if err := ds.advance(day); err != nil {
			return err
		}
	}
	if ds.cur != nil && ds.cur.day == day {
		return ds.cur.add(r)
	}
	return nil
}

func (ds *dayStream) finish() ([]int, error) {
	if err := ds.advance(1 << 30); err != nil {
		return nil, err
	}
	if ds.cur != nil {
		return nil, errors.New("day stream left a day open")
	}
	return ds.done, nil
}
