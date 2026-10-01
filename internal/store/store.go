// Package store keeps a simulated world on disk in SQLite.
//
// It stores the recipe, not the meal. Every record is a pure function of the
// seed, so a world directory keeps the seed, hourly engine checkpoints,
// 1-minute rollups, metrics, transitions and ground truth, and stores raw
// events only for days someone materializes. Reading a window that is not
// materialized loads the nearest checkpoint and regenerates just that window.
//
// Layout of a world directory:
//
//	seed.yaml              the seed (its hash must match world.db)
//	world.db               catalog, checkpoints, rollups, metrics, transitions, truth
//	events/day-0001.db     raw events of simulated day 1, if materialized
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "github.com/mattn/go-sqlite3" // SQLite driver (cgo)

	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/spec"
)

// File and directory names inside a world directory.
const (
	WorldFile = "world.db"
	SeedFile  = "seed.yaml"
	EventsDir = "events"
)

// SchemaVersion is bumped when the on-disk layout changes.
const SchemaVersion = 1

const dayNs = int64(86400e9)

const worldSchema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE classes (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE states (class INTEGER NOT NULL, id INTEGER NOT NULL, name TEXT NOT NULL, PRIMARY KEY (class, id));
CREATE TABLE devices (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, class INTEGER NOT NULL, site INTEGER NOT NULL, idx INTEGER NOT NULL);
CREATE TABLE templates (id INTEGER PRIMARY KEY, class INTEGER NOT NULL, event TEXT NOT NULL, level INTEGER NOT NULL, text TEXT NOT NULL, tags TEXT NOT NULL);
CREATE TABLE metrics (class INTEGER NOT NULL, id INTEGER NOT NULL, name TEXT NOT NULL, unit TEXT NOT NULL, PRIMARY KEY (class, id));
CREATE TABLE truth (cause INTEGER PRIMARY KEY, label TEXT NOT NULL, target TEXT NOT NULL, start_tick INTEGER NOT NULL, end_tick INTEGER NOT NULL);
CREATE TABLE truth_devices (cause INTEGER NOT NULL, device INTEGER NOT NULL, PRIMARY KEY (cause, device)) WITHOUT ROWID;
CREATE TABLE checkpoints (tick INTEGER PRIMARY KEY, state BLOB NOT NULL);
CREATE TABLE transitions (tick INTEGER NOT NULL, device INTEGER NOT NULL, from_state INTEGER NOT NULL, to_state INTEGER NOT NULL, cause TEXT NOT NULL);
CREATE INDEX transitions_device ON transitions (device, tick);
CREATE TABLE rollup_1m (minute INTEGER NOT NULL, device INTEGER NOT NULL, template INTEGER NOT NULL, n INTEGER NOT NULL, PRIMARY KEY (minute, device, template)) WITHOUT ROWID;
CREATE TABLE metrics_1m (device INTEGER NOT NULL, metric INTEGER NOT NULL, minute INTEGER NOT NULL, vmin REAL NOT NULL, vavg REAL NOT NULL, vmax REAL NOT NULL, PRIMARY KEY (device, metric, minute)) WITHOUT ROWID;
CREATE TABLE materialized (day INTEGER PRIMARY KEY, file TEXT NOT NULL, records INTEGER NOT NULL, sha256 TEXT NOT NULL);
`

const eventsSchema = `
CREATE TABLE events (ts INTEGER NOT NULL, device INTEGER NOT NULL, template INTEGER NOT NULL, level INTEGER NOT NULL,
  flags INTEGER NOT NULL, trace INTEGER NOT NULL, span INTEGER NOT NULL, parent INTEGER NOT NULL,
  s0 INTEGER NOT NULL, s1 INTEGER NOT NULL, s2 INTEGER NOT NULL, s3 INTEGER NOT NULL, s4 INTEGER NOT NULL,
  cause INTEGER NOT NULL, skew INTEGER NOT NULL);
`

// Indexes on day files are created after the bulk insert. Rows are inserted
// in engine order, so (ts, rowid) is the engine's total order.
const eventsIndexes = `
CREATE INDEX events_ts ON events (ts);
CREATE INDEX events_device_ts ON events (device, ts);
`

func openDB(path string, bulk bool) (*sql.DB, error) {
	params := "_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000"
	if bulk {
		params = "_journal_mode=OFF&_synchronous=OFF"
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?"+params)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Store is an open world directory.
type Store struct {
	Dir   string
	World *ir.World
	Seed  []byte

	// BuildFrom and BuildTo are the tick window the world was built for;
	// rollups, metrics and transitions cover it.
	BuildFrom, BuildTo int64
	// CheckpointEvery is the checkpoint spacing in ticks.
	CheckpointEvery int64

	db   *sql.DB
	days map[int]*sql.DB
}

// Open opens a world directory and checks that its seed and engine match.
func Open(dir string) (*Store, error) {
	seed, err := os.ReadFile(filepath.Join(dir, SeedFile))
	if err != nil {
		return nil, fmt.Errorf("%s is not a world directory: %w", dir, err)
	}
	s, err := spec.Parse(seed)
	if err != nil {
		return nil, err
	}
	w, err := compile.Compile(s, seed)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, WorldFile)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("%s is not a world directory: %w", dir, err)
	}
	db, err := openDB(dbPath, false)
	if err != nil {
		return nil, err
	}
	st := &Store{Dir: dir, World: w, Seed: seed, db: db, days: map[int]*sql.DB{}}
	meta, err := st.meta()
	if err != nil {
		db.Close()
		return nil, err
	}
	switch {
	case meta["schema"] != strconv.Itoa(SchemaVersion):
		err = fmt.Errorf("world schema %q is not supported (want %d)", meta["schema"], SchemaVersion)
	case meta["engine"] != ir.EngineVersion:
		err = fmt.Errorf("world was built by engine %s, this is %s; rebuild it", meta["engine"], ir.EngineVersion)
	case meta["seed_sha256"] != w.SeedSHA256:
		err = errors.New("seed.yaml does not match the world it was built from")
	}
	if err == nil {
		st.BuildFrom, err = strconv.ParseInt(meta["build_from"], 10, 64)
	}
	if err == nil {
		st.BuildTo, err = strconv.ParseInt(meta["build_to"], 10, 64)
	}
	if err == nil {
		st.CheckpointEvery, err = strconv.ParseInt(meta["checkpoint_every"], 10, 64)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) meta() (map[string]string, error) {
	rows, err := s.db.Query("SELECT key, value FROM meta")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// Close closes all database handles.
func (s *Store) Close() error {
	var first error
	for _, db := range s.days {
		if err := db.Close(); err != nil && first == nil {
			first = err
		}
	}
	if err := s.db.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// DB exposes world.db for ad hoc SQL.
func (s *Store) DB() *sql.DB { return s.db }

func dayFile(day int) string {
	return filepath.Join(EventsDir, fmt.Sprintf("day-%04d.db", day+1))
}

// Days returns the number of simulated days in the run (the last may be
// partial).
func Days(w *ir.World) int {
	end := w.Ticks * w.TickNs
	return int((end + dayNs - 1) / dayNs)
}

// daySpan returns day d's [from, to) in ns, clipped to the end of the run.
func daySpan(w *ir.World, d int) (int64, int64) {
	return int64(d) * dayNs, min(int64(d+1)*dayNs, w.Ticks*w.TickNs)
}
