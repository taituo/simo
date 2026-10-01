package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/spec"
)

// rfc9293 lists the transitions RFC 9293 allows (Figure 5 plus the reset and
// simultaneous-open cases in section 3.10). It is written independently of
// the example seed.
var rfc9293 = &Reference{
	Name: "RFC 9293",
	Allowed: map[string][]string{
		"CLOSED":       {"LISTEN", "SYN-SENT"},
		"LISTEN":       {"SYN-RECEIVED", "SYN-SENT", "CLOSED"},
		"SYN-SENT":     {"SYN-RECEIVED", "ESTABLISHED", "CLOSED"},
		"SYN-RECEIVED": {"ESTABLISHED", "FIN-WAIT-1", "LISTEN", "CLOSED"},
		"ESTABLISHED":  {"FIN-WAIT-1", "CLOSE-WAIT", "CLOSED"},
		"FIN-WAIT-1":   {"FIN-WAIT-2", "CLOSING", "TIME-WAIT", "CLOSED"},
		"FIN-WAIT-2":   {"TIME-WAIT", "CLOSED"},
		"CLOSING":      {"TIME-WAIT", "CLOSED"},
		"TIME-WAIT":    {"CLOSED"},
		"CLOSE-WAIT":   {"LAST-ACK", "CLOSED"},
		"LAST-ACK":     {"CLOSED"},
	},
}

type run struct {
	seed  *spec.Seed
	world *ir.World
	trace []engine.Transition
	recs  []record.Record
}

func simulate(t *testing.T, src []byte, to int64) run {
	t.Helper()
	s, err := spec.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	w, err := compile.Compile(s, src)
	if err != nil {
		t.Fatal(err)
	}
	r := run{seed: s, world: w}
	_, err = engine.Run(w, engine.Options{To: to, OnTransition: func(tr engine.Transition) { r.trace = append(r.trace, tr) }},
		func(b []record.Record) error {
			r.recs = append(r.recs, b...)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func readExample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func report(t *testing.T, what string, vs []Violation) {
	t.Helper()
	for i, v := range vs {
		if i == 10 {
			t.Errorf("... and %d more", len(vs)-10)
			break
		}
		t.Errorf("%s: %s", what, v)
	}
}

func TestTCPFollowsRFC9293(t *testing.T) {
	r := simulate(t, readExample(t, "tcp-connections.yaml"), 0)
	if len(r.trace) < 10000 {
		t.Fatalf("only %d transitions; the fixture should exercise the machine", len(r.trace))
	}
	report(t, "RFC 9293", CheckTransitions(r.world, "tcp_conn", rfc9293, r.trace))

	// Every RFC state should be visited.
	seen := map[string]bool{}
	cls := &r.world.Classes[0]
	for _, tr := range r.trace {
		seen[cls.States[tr.To].Name] = true
	}
	for s := range rfc9293.Allowed {
		if !seen[s] {
			t.Errorf("state %s never entered", s)
		}
	}
}

// The referee must actually catch an illegal transition.
func TestRefereeCatchesIllegalTCPTransition(t *testing.T) {
	src := strings.Replace(string(readExample(t, "tcp-connections.yaml")),
		"CLOSED:       { to: { LISTEN: 1/20s, SYN-SENT: 1/10s } }",
		"CLOSED:       { to: { LISTEN: 1/20s, SYN-SENT: 1/10s, ESTABLISHED: 1/30s } }", 1)
	r := simulate(t, []byte(src), 6000)
	vs := CheckTransitions(r.world, "tcp_conn", rfc9293, r.trace)
	if len(vs) == 0 {
		t.Fatal("CLOSED -> ESTABLISHED was not reported")
	}
	if !strings.Contains(vs[0].Msg, "CLOSED -> ESTABLISHED is not allowed by RFC 9293") {
		t.Errorf("unexpected violation: %s", vs[0])
	}
}

// For every example, the engine may only make transitions the seed declares,
// and every log line must be legal for its device's state.
func TestExamplesConformToTheirSeeds(t *testing.T) {
	cases := map[string]int64{
		"retail-pos.yaml":      2 * 86400, // two days at 1 s ticks: crashes, restarts and the fault
		"tcp-connections.yaml": 0,
		"k8s-web-api.yaml":     18 * 3600, // OOM kills, crash loops, rollout restarts from Running
	}
	for name, to := range cases {
		t.Run(name, func(t *testing.T) {
			r := simulate(t, readExample(t, name), to)
			if len(r.trace) == 0 {
				t.Fatal("no transitions to check")
			}
			for _, cname := range r.seed.Classes.Keys {
				ref, err := FromSeed(r.seed, cname)
				if err != nil {
					t.Fatal(err)
				}
				report(t, "seed", CheckTransitions(r.world, cname, ref, r.trace))
			}
			report(t, "records", CheckRecords(r.world, r.seed, r.trace, r.recs))
		})
	}
}

func TestCheckRecordsCatchesIllegalLines(t *testing.T) {
	r := simulate(t, readExample(t, "retail-pos.yaml"), 2*86400)
	// Find a device that went down, and forge a transaction while it was down.
	cls := &r.world.Classes[0]
	down := cls.StateIndex("down")
	txn := -1
	for i, e := range r.world.Events {
		if e.ID == "txn_ok" {
			txn = i
		}
	}
	for _, tr := range r.trace {
		if tr.To == down {
			forged := record.Record{TS: tr.Tick * r.world.TickNs, Device: tr.Device, Template: uint16(txn)}
			vs := CheckRecords(r.world, r.seed, r.trace, []record.Record{forged})
			if len(vs) != 1 {
				t.Fatalf("forged txn_ok in state down: got %d violations, want 1", len(vs))
			}
			return
		}
	}
	t.Fatal("no device went down in two days; cannot test")
}
