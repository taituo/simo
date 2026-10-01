package store

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/spec"
)

const hourNs = int64(time.Hour)

func loadRetail(t testing.TB) (*ir.World, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "retail-pos.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := spec.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	w, err := compile.Compile(s, raw)
	if err != nil {
		t.Fatal(err)
	}
	return w, raw
}

// fixture is one world built for two simulated days with both days
// materialized, plus a reference run of the engine from tick 0.
type fixture struct {
	dir   string
	res   *BuildResult
	ref   []record.Record // engine from tick 0 to 50 h
	world *ir.World
}

var (
	fixOnce sync.Once
	fix     fixture
	fixErr  error
)

func getFixture(t *testing.T) *fixture {
	t.Helper()
	fixOnce.Do(func() {
		w, raw := loadRetail(t)
		dir, err := os.MkdirTemp("", "simo-store-test-")
		if err != nil {
			fixErr = err
			return
		}
		fix.dir, fix.world = dir, w
		fix.res, fixErr = Build(dir, w, raw, BuildOptions{To: 48 * 3600, Materialize: true})
		if fixErr != nil {
			return
		}
		_, fixErr = engine.Run(w, engine.Options{To: 50 * 3600}, func(b []record.Record) error {
			fix.ref = append(fix.ref, b...)
			return nil
		})
	})
	if fixErr != nil {
		t.Fatal(fixErr)
	}
	return &fix
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fix.dir != "" {
		os.RemoveAll(fix.dir)
	}
	os.Exit(code)
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func collect(t *testing.T, s *Store, q Query) ([]record.Record, []Segment) {
	t.Helper()
	var out []record.Record
	segs, err := s.Events(q, func(r *record.Record) error {
		out = append(out, *r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out, segs
}

func digest(recs []record.Record) [32]byte {
	h := sha256.New()
	var buf [record.Size]byte
	for i := range recs {
		recs[i].Encode(buf[:])
		h.Write(buf[:])
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}

func refWindow(f *fixture, from, to int64, keep func(*record.Record) bool) []record.Record {
	var out []record.Record
	for i := range f.ref {
		r := &f.ref[i]
		if r.TS >= from && r.TS < to && (keep == nil || keep(r)) {
			out = append(out, *r)
		}
	}
	return out
}

func TestBuildSummary(t *testing.T) {
	f := getFixture(t)
	if got := f.res.MaterializedDays; len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("materialized days %v, want [0 1]", got)
	}
	// Hourly from tick 0 through 47 h, plus one at the end of the window.
	if f.res.Checkpoints != 49 {
		t.Errorf("%d checkpoints, want 49", f.res.Checkpoints)
	}
	s := open(t, f.dir)
	if s.BuildFrom != 0 || s.BuildTo != 48*3600 {
		t.Errorf("build window [%d, %d)", s.BuildFrom, s.BuildTo)
	}
}

// The phase 2 exit gate: a regenerated window equals the stored one byte for
// byte, and both equal a plain engine run from tick 0.
func TestRegeneratedWindowEqualsStored(t *testing.T) {
	f := getFixture(t)
	s := open(t, f.dir)
	windows := []struct {
		name     string
		from, to int64
	}{
		{"afternoon", 13 * hourNs, 15*hourNs + 30*int64(time.Minute)},
		{"across midnight", 22 * hourNs, 26 * hourNs},
		{"day 2 with the fault", 24 * hourNs, 48 * hourNs},
	}
	for _, wd := range windows {
		t.Run(wd.name, func(t *testing.T) {
			stored, segs := collect(t, s, Query{FromNs: wd.from, ToNs: wd.to, Mode: SQL})
			for _, sg := range segs {
				if !sg.FromSQL {
					t.Fatalf("mode sql used regeneration for %+v", sg)
				}
			}
			regen, segs := collect(t, s, Query{FromNs: wd.from, ToNs: wd.to, Mode: Regen})
			if len(segs) != 1 || segs[0].FromSQL {
				t.Fatalf("mode regen segments %+v", segs)
			}
			want := refWindow(f, wd.from, wd.to, nil)
			if len(stored) == 0 {
				t.Fatal("empty window")
			}
			if digest(stored) != digest(want) {
				t.Errorf("stored window (%d records) differs from the engine (%d)", len(stored), len(want))
			}
			if digest(regen) != digest(want) {
				t.Errorf("regenerated window (%d records) differs from the engine (%d)", len(regen), len(want))
			}
		})
	}
}

func TestFiltersAgreeAcrossModes(t *testing.T) {
	f := getFixture(t)
	s := open(t, f.dir)
	devices := []uint32{3, 40, 41, 42}
	q := Query{FromNs: 30 * hourNs, ToNs: 36 * hourNs, Devices: devices, MinLevel: 3}
	keep := func(r *record.Record) bool {
		return r.Level >= 3 && (r.Device == 3 || (r.Device >= 40 && r.Device <= 42))
	}
	want := refWindow(f, q.FromNs, q.ToNs, keep)
	if len(want) == 0 {
		t.Fatal("filter matches nothing; pick other devices")
	}
	for _, mode := range []Mode{SQL, Regen} {
		q.Mode = mode
		got, _ := collect(t, s, q)
		if digest(got) != digest(want) {
			t.Errorf("mode %d: %d records, want %d", mode, len(got), len(want))
		}
	}
}

// Beyond the build window nothing is stored, so auto mode regenerates from
// the last checkpoint.
func TestAutoRegeneratesBeyondTheBuild(t *testing.T) {
	f := getFixture(t)
	s := open(t, f.dir)
	got, segs := collect(t, s, Query{FromNs: 47 * hourNs, ToNs: 50 * hourNs})
	if len(segs) != 2 || !segs[0].FromSQL || segs[1].FromSQL {
		t.Errorf("segments %+v, want SQL for day 2 then regeneration for day 3", segs)
	}
	if want := refWindow(f, 47*hourNs, 50*hourNs, nil); digest(got) != digest(want) {
		t.Errorf("%d records, want %d", len(got), len(want))
	}
	if _, err := s.Events(Query{FromNs: 49 * hourNs, ToNs: 50 * hourNs, Mode: SQL}, func(*record.Record) error { return nil }); err == nil {
		t.Error("mode sql on a day that is not materialized was accepted")
	}
}

func TestStopEarly(t *testing.T) {
	f := getFixture(t)
	s := open(t, f.dir)
	n := 0
	_, err := s.Events(Query{FromNs: 0, ToNs: 48 * hourNs}, func(*record.Record) error {
		n++
		if n == 10 {
			return ErrStop
		}
		return nil
	})
	if err != nil || n != 10 {
		t.Errorf("stop early: n=%d err=%v", n, err)
	}
}

func TestRollupsMatchRecords(t *testing.T) {
	f := getFixture(t)
	s := open(t, f.dir)
	rows, err := s.Rollup(RollupQuery{By: []string{"event"}})
	if err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	byEvent := map[string]int64{}
	for _, r := range rows {
		total += r.N
		byEvent[r.Keys[0]] = r.N
	}
	if total != f.res.Stats.Records {
		t.Errorf("rollups sum to %d, build wrote %d records", total, f.res.Stats.Records)
	}
	for e, n := range f.res.Stats.ByEvent {
		ev := f.world.Events[e]
		key := f.world.Classes[ev.Class].Name + "/" + ev.ID
		if byEvent[key] != n {
			t.Errorf("%s: rollup %d, records %d", key, byEvent[key], n)
		}
	}

	// Hourly WARN counts by device for day 2 match the reference run.
	rows, err = s.Rollup(RollupQuery{FromMin: 24 * 60, ToMin: 48 * 60, StepMin: 60, By: []string{"level", "device"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{}
	for _, r := range refWindow(f, 24*hourNs, 48*hourNs, nil) {
		if spec.Levels[r.Level] == "WARN" {
			want[f.world.Devices[r.Device].Name]++
		}
	}
	got := map[string]int64{}
	for _, r := range rows {
		if r.Keys[0] == "WARN" {
			got[r.Keys[1]] += r.N
		}
	}
	for dev, n := range want {
		if got[dev] != n {
			t.Errorf("%s: %d WARN in rollups, %d in records", dev, got[dev], n)
		}
	}
	if _, err := s.Rollup(RollupQuery{By: []string{"colour"}}); err == nil {
		t.Error("unknown group key was accepted")
	}
}

func TestMetrics(t *testing.T) {
	f := getFixture(t)
	s := open(t, f.dir)
	rows, err := s.Metrics(5, 0, 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 48*60 {
		t.Fatalf("%d minute samples, want %d", len(rows), 48*60)
	}
	hourly, err := s.Metrics(5, 0, 0, 0, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(hourly) != 48 || hourly[1].Minute != 60 {
		t.Fatalf("hourly buckets: %d, second at minute %d", len(hourly), hourly[1].Minute)
	}
	for _, r := range hourly {
		if !(r.Min <= r.Avg && r.Avg <= r.Max) || r.Avg < 20 || r.Avg > 80 {
			t.Errorf("implausible temperature bucket %+v", r)
		}
	}
}

func TestMaterializeLater(t *testing.T) {
	w, raw := loadRetail(t)
	dir := t.TempDir()
	if _, err := Build(dir, w, raw, BuildOptions{To: 24 * 3600}); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir)
	before, segs := collect(t, s, Query{FromNs: 10 * hourNs, ToNs: 12 * hourNs})
	if segs[0].FromSQL {
		t.Fatal("nothing is materialized yet, but SQL was used")
	}
	var cpBefore int
	s.DB().QueryRow("SELECT COUNT(*) FROM checkpoints").Scan(&cpBefore)

	// Day 1 lies inside the build; day 2 lies beyond it.
	days, err := s.Materialize(0, 48*hourNs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0] != 0 || days[1] != 1 {
		t.Fatalf("materialized %v, want [0 1]", days)
	}
	var cpAfter int
	s.DB().QueryRow("SELECT COUNT(*) FROM checkpoints").Scan(&cpAfter)
	if cpAfter != cpBefore+24 {
		t.Errorf("checkpoints %d -> %d, want 24 new hourly ones for day 2", cpBefore, cpAfter)
	}

	after, segs := collect(t, s, Query{FromNs: 10 * hourNs, ToNs: 12 * hourNs})
	if !segs[0].FromSQL {
		t.Fatal("day 1 is materialized, but SQL was not used")
	}
	if digest(before) != digest(after) {
		t.Error("records changed after materializing")
	}
	stored, _ := collect(t, s, Query{FromNs: 24 * hourNs, ToNs: 48 * hourNs, Mode: SQL})
	regen, _ := collect(t, s, Query{FromNs: 24 * hourNs, ToNs: 48 * hourNs, Mode: Regen})
	if len(stored) == 0 || digest(stored) != digest(regen) {
		t.Errorf("day 2: stored %d records, regenerated %d, or they differ", len(stored), len(regen))
	}
	// The stored day hash matches the regenerated day.
	var sha string
	if err := s.DB().QueryRow("SELECT sha256 FROM materialized WHERE day = 2").Scan(&sha); err != nil {
		t.Fatal(err)
	}
	if d := digest(regen); sha != hexString(d[:]) {
		t.Errorf("stored day hash %s, regenerated %x", sha, d)
	}
	if again, err := s.Materialize(0, 48*hourNs, 0); err != nil || len(again) != 0 {
		t.Errorf("materializing stored days again wrote %v (err %v), want nothing", again, err)
	}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func TestOpenChecks(t *testing.T) {
	w, raw := loadRetail(t)
	dir := t.TempDir()
	if _, err := Build(dir, w, raw, BuildOptions{To: 3600}); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(dir, w, raw, BuildOptions{To: 3600}); err == nil {
		t.Error("building into an existing world was accepted")
	}
	edited := strings.Replace(string(raw), "master_seed: 42", "master_seed: 43", 1)
	if err := os.WriteFile(filepath.Join(dir, SeedFile), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("edited seed: err = %v", err)
	}
	if _, err := Open(t.TempDir()); err == nil {
		t.Error("opening an empty directory succeeded")
	}
	if !errors.Is(ErrStop, ErrStop) {
		t.Fatal("unreachable")
	}
}
