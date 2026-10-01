// Package conformance checks engine output against independent references.
//
// A Reference is a state machine built with qmuntal/stateless, a statechart
// library that knows nothing about simo. A transition trace is replayed
// through one machine per device; any transition the reference refuses is a
// violation. References come either from a seed's declared transitions (to
// catch engine bugs) or from an external definition such as RFC 9293's TCP
// state machine (to catch seeds that are not faithful to the real thing).
package conformance

import (
	"context"
	"fmt"
	"sort"

	"github.com/qmuntal/stateless"

	"github.com/taituo/simo/internal/engine"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/spec"
)

// Reference is a set of allowed transitions between named states.
type Reference struct {
	Name    string
	Allowed map[string][]string // from -> allowed targets
}

// FromSeed builds a reference for one class from the seed file itself (not
// the compiled tables): every declared transition plus every command.
func FromSeed(s *spec.Seed, class string) (*Reference, error) {
	c, ok := s.Classes.Get(class)
	if !ok {
		return nil, fmt.Errorf("no class %q", class)
	}
	r := &Reference{Name: "seed " + class, Allowed: map[string][]string{}}
	for _, from := range c.States.Keys {
		r.Allowed[from] = append(r.Allowed[from], c.States.Values[from].To.Keys...)
	}
	for _, cmd := range c.Commands.Values {
		for _, from := range cmd.From {
			r.Allowed[from] = append(r.Allowed[from], cmd.To)
		}
	}
	return r, nil
}

func trigger(to string) string { return "to:" + to }

// machine builds a stateless machine for this reference, starting in state.
func (r *Reference) machine(state string) *stateless.StateMachine {
	sm := stateless.NewStateMachine(state)
	for from, tos := range r.Allowed {
		cfg := sm.Configure(from)
		seen := map[string]bool{}
		for _, to := range tos {
			if seen[to] {
				continue
			}
			seen[to] = true
			if to == from {
				cfg.PermitReentry(trigger(to))
			} else {
				cfg.Permit(trigger(to), to)
			}
		}
	}
	return sm
}

// Violation is one disagreement with a reference.
type Violation struct {
	Tick   int64
	Device string
	Msg    string
}

func (v Violation) String() string {
	return fmt.Sprintf("tick %d, %s: %s", v.Tick, v.Device, v.Msg)
}

// CheckTransitions replays the trace of one class's devices through the
// reference. The trace must be in engine order (by tick, then device).
func CheckTransitions(w *ir.World, class string, ref *Reference, trace []engine.Transition) []Violation {
	ci := classIndex(w, class)
	if ci < 0 {
		return []Violation{{Msg: fmt.Sprintf("no class %q", class)}}
	}
	cls := &w.Classes[ci]
	machines := map[uint32]*stateless.StateMachine{}
	var out []Violation
	ctx := context.Background()
	for _, tr := range trace {
		dev := &w.Devices[tr.Device]
		if dev.Class != ci {
			continue
		}
		from, to := cls.States[tr.From].Name, cls.States[tr.To].Name
		sm, ok := machines[tr.Device]
		if !ok {
			sm = ref.machine(cls.States[cls.Initial].Name)
			machines[tr.Device] = sm
		}
		cur, _ := sm.State(ctx)
		if cur != from {
			out = append(out, Violation{tr.Tick, dev.Name, fmt.Sprintf("trace says %s -> %s, but the device was in %v", from, to, cur)})
			sm = ref.machine(from)
			machines[tr.Device] = sm
		}
		if err := sm.Fire(trigger(to)); err != nil {
			out = append(out, Violation{tr.Tick, dev.Name, fmt.Sprintf("%s -> %s is not allowed by %s", from, to, ref.Name)})
			machines[tr.Device] = ref.machine(to)
		}
	}
	return out
}

// CheckRecords checks that every log line is legal for the state its device
// was in when it was emitted, as the seed defines it: the event has a rate
// and a non-zero multiplier in that state, or the state (or a command that
// leads to it) emits it on entry.
func CheckRecords(w *ir.World, s *spec.Seed, trace []engine.Transition, recs []record.Record) []Violation {
	type key struct {
		class, event, state string
	}
	allowed := map[key]bool{}
	for _, cname := range s.Classes.Keys {
		c := s.Classes.Values[cname]
		for _, sname := range c.States.Keys {
			st := c.States.Values[sname]
			for _, e := range c.Events {
				r, _ := spec.ParseRateExpr(string(e.Rate))
				mult := 1.0
				if len(e.OnlyIn) > 0 {
					mult = 0
					for _, o := range e.OnlyIn {
						if o == sname {
							mult = 1
						}
					}
				}
				if m, ok := e.InState[sname]; ok {
					mult, _ = spec.ParseMultiplier(string(m))
				}
				if r.PerSec > 0 && mult > 0 {
					allowed[key{cname, e.ID, sname}] = true
				}
			}
			for _, e := range st.Emit {
				allowed[key{cname, e, sname}] = true
			}
			// on_command targets also receive the command's emits.
			for _, to := range st.To.Keys {
				tr, _ := spec.ParseTransition(string(st.To.Values[to]))
				if tr.Kind == spec.TransCommand {
					for _, e := range c.Commands.Values[tr.Command].Emit {
						allowed[key{cname, e, to}] = true
					}
				}
			}
		}
		for _, cmd := range c.Commands.Values {
			for _, e := range cmd.Emit {
				allowed[key{cname, e, cmd.To}] = true
			}
		}
	}

	// Per-device transitions in order.
	byDev := map[uint32][]engine.Transition{}
	for _, tr := range trace {
		byDev[tr.Device] = append(byDev[tr.Device], tr)
	}
	for _, list := range byDev {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Tick < list[j].Tick })
	}
	pos := map[uint32]int{}
	var out []Violation
	for _, r := range recs {
		dev := &w.Devices[r.Device]
		cls := &w.Classes[dev.Class]
		tick := r.TS / w.TickNs
		state := cls.Initial
		list := byDev[r.Device]
		i := pos[r.Device]
		for i < len(list) && list[i].Tick <= tick {
			i++
		}
		pos[r.Device] = i
		if i > 0 {
			state = list[i-1].To
		}
		ev := &w.Events[r.Template]
		if !allowed[key{cls.Name, ev.ID, cls.States[state].Name}] {
			out = append(out, Violation{tick, dev.Name, fmt.Sprintf("event %s is not legal in state %s", ev.ID, cls.States[state].Name)})
		}
	}
	return out
}

func classIndex(w *ir.World, name string) int {
	for i, c := range w.Classes {
		if c.Name == name {
			return i
		}
	}
	return -1
}
