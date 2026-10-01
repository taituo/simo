package engine_test

import (
	"crypto/sha256"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/spec"
)

func compileYAML(t *testing.T, src string) *ir.World {
	t.Helper()
	s, err := spec.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	w, err := compile.Compile(s, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// runAll collects every record and transition of a run.
func runAll(t *testing.T, w *ir.World, opt engine.Options) ([]record.Record, []engine.Transition, *engine.Stats) {
	t.Helper()
	var recs []record.Record
	var trans []engine.Transition
	opt.OnTransition = func(tr engine.Transition) { trans = append(trans, tr) }
	st, err := engine.Run(w, opt, func(b []record.Record) error {
		recs = append(recs, b...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return recs, trans, st
}

func hashRecords(recs []record.Record) [32]byte {
	h := sha256.New()
	var buf [record.Size]byte
	for i := range recs {
		recs[i].Encode(buf[:])
		h.Write(buf[:])
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

const ctmcSeed = `
simo: 1
name: ctmc
master_seed: 7
clock: { start: 2026-01-05T00:00:00Z, duration: 3d, tick: 10s }
classes:
  m:
    states:
      a: { to: { b: 1/30m, c: 1/2h } }
      b: { to: { a: 1/10m, c: 1/1h } }
      c: { to: { a: 1/20m } }
fleet:
  - { class: m, count: 400 }
`

// The long-run share of time in each state must match the stationary
// distribution pi of the rate matrix Q (pi Q = 0, sum pi = 1), and mean
// holding times must match 1/(total exit rate).
func TestStationaryDistribution(t *testing.T) {
	w := compileYAML(t, ctmcSeed)
	_, trans, st := runAll(t, w, engine.Options{})

	// Rate matrix from the seed (per second).
	r := func(per float64) float64 { return 1 / per }
	q := [3][3]float64{
		{0, r(1800), r(7200)},
		{r(600), 0, r(3600)},
		{r(1200), 0, 0},
	}
	for i := range q {
		for j := range q {
			if i != j {
				q[i][i] -= q[i][j]
			}
		}
	}
	pi := stationary(q)

	ticks := st.StateTicks[0]
	total := float64(ticks[0] + ticks[1] + ticks[2])
	for i, p := range pi {
		got := float64(ticks[i]) / total
		t.Logf("state %s: time share %.4f, stationary %.4f", w.Classes[0].States[i].Name, got, p)
		if math.Abs(got-p) > 0.01 {
			t.Errorf("state %s: time share %.4f, stationary %.4f", w.Classes[0].States[i].Name, got, p)
		}
	}

	// Mean holding time per state from completed sojourns.
	enter := map[uint32]int64{}
	sum := make([]float64, 3)
	n := make([]float64, 3)
	for _, tr := range trans {
		if t0, ok := enter[tr.Device]; ok {
			sum[tr.From] += float64(tr.Tick-t0) * w.TickSeconds()
			n[tr.From]++
		}
		enter[tr.Device] = tr.Tick
	}
	for i := range q {
		want := -1 / q[i][i]
		got := sum[i] / n[i]
		t.Logf("state %s: mean holding %.0fs over %v sojourns, want %.0fs", w.Classes[0].States[i].Name, got, n[i], want)
		if math.Abs(got-want)/want > 0.03 {
			t.Errorf("state %s: mean holding %.0fs over %v sojourns, want %.0fs", w.Classes[0].States[i].Name, got, n[i], want)
		}
	}
}

// stationary solves pi Q = 0 with sum(pi) = 1 by Gaussian elimination.
func stationary(q [3][3]float64) [3]float64 {
	// Rows: Q^T with the last row replaced by ones.
	var a [3][4]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			a[i][j] = q[j][i]
		}
	}
	a[2] = [4]float64{1, 1, 1, 1}
	for c := 0; c < 3; c++ {
		p := c
		for r := c + 1; r < 3; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[p][c]) {
				p = r
			}
		}
		a[c], a[p] = a[p], a[c]
		for r := 0; r < 3; r++ {
			if r != c {
				f := a[r][c] / a[c][c]
				for k := c; k < 4; k++ {
					a[r][k] -= f * a[c][k]
				}
			}
		}
	}
	return [3]float64{a[0][3] / a[0][0], a[1][3] / a[1][1], a[2][3] / a[2][2]}
}

const rateSeed = `
simo: 1
name: rates
master_seed: 11
clock: { start: 2026-01-05T00:00:00Z, duration: 1d, tick: 1s }
patterns:
  daily: { fourier: { period: 24h, harmonics: [[1, 0.8, 15h]] } }
classes:
  box:
    states:
      up: {}
    events:
      - { id: flat, level: INFO, text: "flat", rate: 0.05/s }
      - { id: wave, level: INFO, text: "wave", rate: 0.05/s * daily }
fleet:
  - { class: box, count: 40 }
`

// Event counts must match rate x time (and the integral of the pattern).
func TestEventRates(t *testing.T) {
	w := compileYAML(t, rateSeed)
	_, _, st := runAll(t, w, engine.Options{})
	devices := float64(len(w.Devices))
	expectFlat := 0.05 * devices * float64(w.Ticks)
	expectWave := 0.0
	start := float64(w.Start.Unix())
	for tick := int64(0); tick < w.Ticks; tick++ {
		expectWave += 0.05 * devices * w.Patterns[0].At(start+float64(tick))
	}
	for i, want := range []float64{expectFlat, expectWave} {
		got := float64(st.ByEvent[i])
		if sd := math.Sqrt(want); math.Abs(got-want) > 5*sd {
			t.Errorf("event %s: %v events, want %.0f +- %.0f", w.Events[i].ID, got, want, 5*sd)
		}
	}
}

func TestOutputDoesNotDependOnWorkers(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "retail-pos.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	w := compileYAML(t, string(raw))
	a, ta, _ := runAll(t, w, engine.Options{To: 12 * 3600, Workers: 1})
	b, tb, _ := runAll(t, w, engine.Options{To: 12 * 3600, Workers: 7})
	if len(a) == 0 || len(ta) == 0 {
		t.Fatalf("nothing to compare: %d records, %d transitions", len(a), len(ta))
	}
	if hashRecords(a) != hashRecords(b) {
		t.Error("record streams differ between 1 and 7 workers")
	}
	if !slices.Equal(ta, tb) {
		t.Error("transition traces differ between 1 and 7 workers")
	}
}

func TestWindowIsASliceOfTheFullRun(t *testing.T) {
	w := compileYAML(t, rateSeed)
	full, _, _ := runAll(t, w, engine.Options{To: 7200})
	part, _, _ := runAll(t, w, engine.Options{From: 3600, To: 7200})
	var want []record.Record
	for _, r := range full {
		if r.TS >= 3600*w.TickNs {
			want = append(want, r)
		}
	}
	if len(part) == 0 || hashRecords(part) != hashRecords(want) {
		t.Fatalf("window run (%d records) differs from the same window of the full run (%d records)", len(part), len(want))
	}
}

const commandSeed = `
simo: 1
name: cmds
master_seed: 3
clock: { start: 2026-01-05T00:00:00Z, duration: 1h, tick: 1s }
classes:
  svc:
    states:
      running:    { }
      restarting: { to: { running: fixed(30s) }, emit: [started] }
    events:
      - { id: started, level: NOTICE, text: "started #{n:seq}" }
    commands:
      restart: { from: [running], to: restarting }
fleet:
  - { class: svc, count: 2 }
`

func TestManualCommands(t *testing.T) {
	w := compileYAML(t, commandSeed)
	recs, trans, _ := runAll(t, w, engine.Options{Commands: []engine.Command{
		{Tick: 100, Device: 1, Command: "restart"},
		{Tick: 110, Device: 1, Command: "restart"}, // not allowed while restarting: ignored
		{Tick: 500, Device: 1, Command: "restart"},
	}})
	if len(trans) != 4 {
		t.Fatalf("got %d transitions, want 4: %+v", len(trans), trans)
	}
	want := []engine.Transition{
		{Tick: 100, Device: 1, From: 0, To: 1, Cause: engine.CauseCommand, Command: 0},
		{Tick: 130, Device: 1, From: 1, To: 0, Cause: engine.CauseTimer, Command: -1},
		{Tick: 500, Device: 1, From: 0, To: 1, Cause: engine.CauseCommand, Command: 0},
		{Tick: 530, Device: 1, From: 1, To: 0, Cause: engine.CauseTimer, Command: -1},
	}
	for i := range want {
		if trans[i] != want[i] {
			t.Errorf("transition %d: got %+v, want %+v", i, trans[i], want[i])
		}
	}
	if len(recs) != 2 || recs[0].Slots[0] != 1 || recs[1].Slots[0] != 2 {
		t.Errorf("want two 'started' records with seq 1 and 2, got %+v", recs)
	}
}

func TestRejectsBadWindow(t *testing.T) {
	w := compileYAML(t, commandSeed)
	if _, err := engine.Run(w, engine.Options{From: 10, To: 5}, func([]record.Record) error { return nil }); err == nil {
		t.Error("window with From > To was accepted")
	}
}
