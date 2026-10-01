package engine_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/record"
)

// A run resumed from any checkpoint must produce exactly the records and
// transitions of the full run from that tick on, and taking checkpoints
// must not change the output.
func TestResumeFromCheckpointMatchesFullRun(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "retail-pos.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	w := compileYAML(t, string(raw))
	const hour = 3600
	to := int64(12 * hour) // includes crashes, operator restarts and timers

	plain, _, _ := runAll(t, w, engine.Options{To: to})
	var cps []*engine.Checkpoint
	full, fullTrans, _ := runAll(t, w, engine.Options{To: to, CheckpointEvery: 3 * hour, OnCheckpoint: func(cp *engine.Checkpoint) {
		cps = append(cps, cp)
	}})
	if hashRecords(plain) != hashRecords(full) {
		t.Fatal("taking checkpoints changed the output")
	}
	// Every 3 h from 0, plus one at the end of the window for later runs.
	if len(cps) != 5 || cps[0].Tick != 0 || cps[4].Tick != to {
		t.Fatalf("got %d checkpoints, want ticks 0, 3h, ..., 12h", len(cps))
	}

	for _, cp := range cps[1:4] {
		// Round-trip through the binary encoding, as the store does.
		b, err := cp.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		back, err := engine.UnmarshalCheckpoint(b)
		if err != nil {
			t.Fatal(err)
		}
		from := cp.Tick + 1800 // start the window inside the segment
		got, gotTrans, _ := runAll(t, w, engine.Options{Start: back, From: from, To: to})
		var want []record.Record
		for _, r := range full {
			if r.TS >= from*w.TickNs {
				want = append(want, r)
			}
		}
		var wantTrans []engine.Transition
		for _, tr := range fullTrans {
			if tr.Tick >= from {
				wantTrans = append(wantTrans, tr)
			}
		}
		if len(got) == 0 || hashRecords(got) != hashRecords(want) {
			t.Errorf("resume at tick %d: %d records, want %d identical ones", cp.Tick, len(got), len(want))
		}
		if !slices.Equal(gotTrans, wantTrans) {
			t.Errorf("resume at tick %d: transitions differ (%d vs %d)", cp.Tick, len(gotTrans), len(wantTrans))
		}
	}
}

func TestCheckpointValidation(t *testing.T) {
	w := compileYAML(t, commandSeed)
	var cp *engine.Checkpoint
	runAll(t, w, engine.Options{To: 600, CheckpointEvery: 300, OnCheckpoint: func(c *engine.Checkpoint) {
		if c.Tick == 300 {
			cp = c
		}
	}})
	if cp == nil || cp.Tick != 300 {
		t.Fatalf("want a checkpoint at tick 300, got %+v", cp)
	}
	sink := func([]record.Record) error { return nil }
	if _, err := engine.Run(w, engine.Options{Start: cp, From: 100}, sink); err == nil {
		t.Error("window before the checkpoint was accepted")
	}
	other := compileYAML(t, rateSeed)
	if _, err := engine.Run(other, engine.Options{Start: cp, From: 300}, sink); err == nil {
		t.Error("checkpoint from another world was accepted")
	}
	b, _ := cp.MarshalBinary()
	if _, err := engine.UnmarshalCheckpoint(b[:len(b)/2]); err == nil {
		t.Error("truncated checkpoint was accepted")
	}
}
