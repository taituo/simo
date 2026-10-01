// Package compile turns a validated seed into the engine's tables: it expands
// the fleet, draws per-device jitter, resolves names to indexes and
// precomputes integer thresholds.
package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/taituo/simo/internal/dist"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/rng"
	"github.com/taituo/simo/internal/spec"
	"github.com/taituo/simo/internal/tmpl"
)

// Random streams used at compile time.
const (
	streamFleet  = 7
	streamJitter = 5
)

// Compile validates and compiles a seed. raw is the seed file's bytes, hashed
// into the world for the manifest.
func Compile(s *spec.Seed, raw []byte) (*ir.World, error) {
	if err := spec.Validate(s).Err(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	w := &ir.World{
		Name:       s.Name,
		MasterSeed: s.MasterSeed,
		SeedSHA256: hex.EncodeToString(sum[:]),
		Vocab:      map[string]ir.Vocab{},
	}
	if w.Name == "" {
		w.Name = "world"
	}
	start, _ := time.Parse(time.RFC3339Nano, string(s.Clock.Start))
	w.Start = start.UTC()
	tick, _ := spec.ParseDuration(string(s.Clock.Tick))
	dur, _ := spec.ParseDuration(string(s.Clock.Duration))
	w.Tick = tick
	w.TickNs = tick.Nanoseconds()
	w.Ticks = int64(dur / tick)
	w.SlowEvery = int64(time.Minute / tick)
	if w.SlowEvery < 1 {
		w.SlowEvery = 1
	}

	patternIdx := map[string]int{}
	for _, name := range s.Patterns.Keys {
		p := s.Patterns.Values[name]
		period, _ := spec.Seconds(string(p.Fourier.Period))
		cp := ir.Pattern{Name: name, Period: period}
		for _, h := range p.Fourier.Harmonics {
			k, _ := strconv.ParseFloat(string(h[0]), 64)
			amp, _ := strconv.ParseFloat(string(h[1]), 64)
			peak, _ := spec.Seconds(string(h[2]))
			cp.Harmonics = append(cp.Harmonics, ir.Harmonic{K: k, Amp: amp, Peak: peak})
		}
		patternIdx[name] = len(w.Patterns)
		w.Patterns = append(w.Patterns, cp)
	}
	for _, name := range s.Vocab.Keys {
		v := s.Vocab.Values[name]
		w.Vocab[name] = ir.Vocab{Words: v.Words, Zipf: dist.NewZipf(len(v.Words), v.Zipf)}
	}

	classIdx := map[string]int{}
	for _, cname := range s.Classes.Keys {
		c, err := compileClass(w, cname, s.Classes.Values[cname], patternIdx)
		if err != nil {
			return nil, fmt.Errorf("classes.%s: %w", cname, err)
		}
		classIdx[cname] = len(w.Classes)
		w.Classes = append(w.Classes, c)
	}
	if err := expandFleet(w, s, classIdx); err != nil {
		return nil, err
	}
	if err := compileFaults(w, s, classIdx); err != nil {
		return nil, err
	}
	compileResponder(w, s)
	return w, nil
}

func compileClass(w *ir.World, cname string, c spec.Class, patternIdx map[string]int) (ir.Class, error) {
	dt := w.TickSeconds()
	out := ir.Class{Name: cname}
	for _, sname := range c.States.Keys {
		out.States = append(out.States, ir.State{Name: sname})
	}
	if c.Initial != "" {
		out.Initial = out.StateIndex(c.Initial)
	}
	if c.Health != nil {
		out.Health = true
		if c.Health.Drift != "" {
			out.Drift, _ = spec.ParseRate(string(c.Health.Drift))
		}
		out.Noise = c.Health.Noise / math.Sqrt(86400)
	}
	for _, mname := range c.Metrics.Keys {
		m := c.Metrics.Values[mname]
		theta, _ := spec.ParseRate(string(m.OU.Theta))
		cm := ir.Metric{Name: mname, Unit: m.Unit, Mean: m.OU.Mean, Theta: theta, SD: m.OU.Sigma, StateShift: make([]float64, len(out.States))}
		for s, shift := range m.InState {
			cm.StateShift[out.StateIndex(s)] = shift.Mean
		}
		out.Metrics = append(out.Metrics, cm)
	}

	// Events, in declaration order, appended to the global table.
	localEvent := map[string]int{}
	classID := len(w.Classes)
	for _, e := range c.Events {
		t, err := tmpl.Parse(e.Text)
		if err != nil {
			return out, err
		}
		r, _ := spec.ParseRateExpr(string(e.Rate))
		ce := ir.Event{
			ID:        e.ID,
			Class:     classID,
			Level:     uint8(spec.LevelIndex(e.Level)),
			Tags:      e.Tags,
			PerSec:    r.PerSec,
			StateMult: make([]float64, len(out.States)),
			Tmpl:      t,
		}
		for _, p := range r.Patterns {
			ce.Patterns = append(ce.Patterns, patternIdx[p])
		}
		for i := range ce.StateMult {
			ce.StateMult[i] = 1
		}
		if len(e.OnlyIn) > 0 {
			for i := range ce.StateMult {
				ce.StateMult[i] = 0
			}
			for _, s := range e.OnlyIn {
				ce.StateMult[out.StateIndex(s)] = 1
			}
		}
		for s, m := range e.InState {
			ce.StateMult[out.StateIndex(s)], _ = spec.ParseMultiplier(string(m))
		}
		for _, sl := range t.Slots {
			src := ir.SlotSource{Kind: ir.SlotRandom}
			switch sl.Gen {
			case "metric":
				src = ir.SlotSource{Kind: ir.SlotMetric, Metric: c.Metrics.Index(sl.Args[0])}
			case "seq":
				src = ir.SlotSource{Kind: ir.SlotSeq}
			}
			ce.Slots = append(ce.Slots, src)
		}
		if len(w.Events) >= math.MaxUint16 {
			return out, fmt.Errorf("too many event templates (max %d)", math.MaxUint16)
		}
		localEvent[e.ID] = len(w.Events)
		out.Events = append(out.Events, len(w.Events))
		w.Events = append(w.Events, ce)
	}
	emitIDs := func(names []string) []int {
		ids := make([]int, len(names))
		for i, n := range names {
			ids[i] = localEvent[n]
		}
		return ids
	}

	// Commands: destination per state. A state's on_command(x) target
	// overrides the command's own "to".
	for _, cmdName := range c.Commands.Keys {
		cm := c.Commands.Values[cmdName]
		cc := ir.Command{Name: cmdName, To: make([]int, len(out.States)), ResetHealth: cm.ResetHealth, Emit: emitIDs(cm.Emit)}
		for i := range cc.To {
			cc.To[i] = -1
		}
		for _, f := range cm.From {
			cc.To[out.StateIndex(f)] = out.StateIndex(cm.To)
		}
		out.Commands = append(out.Commands, cc)
	}

	for si, sname := range c.States.Keys {
		st := c.States.Values[sname]
		cs := &out.States[si]
		cs.Emit = emitIDs(st.Emit)
		for _, to := range st.To.Keys {
			tr, _ := spec.ParseTransition(string(st.To.Values[to]))
			dest := out.StateIndex(to)
			switch tr.Kind {
			case spec.TransRate:
				cs.Rates = append(cs.Rates, ir.RateTrans{To: dest, PerSec: tr.PerSec})
			case spec.TransHealth:
				cs.Rates = append(cs.Rates, ir.RateTrans{To: dest, PerSec: tr.PerSec, Gamma: tr.Gamma, Health: true})
			case spec.TransFixed:
				ticks := int64(math.Round(tr.Fixed.Seconds() / dt))
				if ticks < 1 {
					ticks = 1
				}
				cs.Fixed = &ir.FixedTrans{To: dest, Ticks: ticks}
			case spec.TransCommand:
				ci := c.Commands.Index(tr.Command)
				out.Commands[ci].To[si] = dest
			}
		}
		cs.Const = true
		total := 0.0
		for _, r := range cs.Rates {
			if r.Health {
				cs.Const = false
			}
			total += r.PerSec
		}
		if cs.Const && total > 0 {
			cs.LeaveThr = ir.Threshold(-math.Expm1(-total * dt))
			acc := 0.0
			for _, r := range cs.Rates {
				acc += r.PerSec
				cs.PickThr = append(cs.PickThr, ir.Threshold(acc/total))
			}
			cs.PickThr[len(cs.PickThr)-1] = math.MaxUint32
		}
	}
	return out, nil
}

func expandFleet(w *ir.World, s *spec.Seed, classIdx map[string]int) error {
	for ei, f := range s.Fleet {
		ci := classIdx[f.Class]
		cls := &w.Classes[ci]
		sites := f.Sites
		countDist, _ := spec.ParseDist(string(f.Count))
		if sites > 0 {
			countDist, _ = spec.ParseDist(string(f.PerSite))
		} else {
			sites = 1
		}
		jitter := map[string]dist.Dist{}
		for k, e := range f.Jitter {
			jitter[k], _ = spec.ParseDist(string(e))
		}
		i := 0
		for site := 1; site <= sites; site++ {
			siteKey := rng.EntityKey(s.MasterSeed, uint64(1)<<62|uint64(ei)<<32|uint64(site))
			n := countDist.SampleInt(rng.Unit(rng.Draw(siteKey, streamFleet, 0, 0)[0]))
			for idx := 1; idx <= n; idx++ {
				i++
				id := uint32(len(w.Devices))
				if len(w.Devices) >= math.MaxUint32 {
					return fmt.Errorf("fleet too large")
				}
				d := ir.Device{
					ID:           id,
					Class:        ci,
					Index:        idx,
					RateScale:    1,
					MetricOffset: make([]float64, len(cls.Metrics)),
					Key:          rng.EntityKey(s.MasterSeed, uint64(id)),
				}
				if f.Sites > 0 {
					d.Site = site
				}
				d.Name = deviceName(f, cls.Name, d.Site, idx, i)
				if j, ok := jitter["rate_scale"]; ok {
					d.RateScale = j.Sample(rng.Unit(rng.Draw(d.Key, streamJitter, 0, 0)[0]))
				}
				for mi, m := range cls.Metrics {
					if j, ok := jitter[m.Name+".mean"]; ok {
						d.MetricOffset[mi] = j.Sample(rng.Unit(rng.Draw(d.Key, streamJitter, 0, uint32(1+mi))[0]))
					}
				}
				w.Devices = append(w.Devices, d)
			}
		}
	}
	if len(w.Devices) == 0 {
		return fmt.Errorf("the fleet expands to zero devices")
	}
	return nil
}

func deviceName(f spec.FleetEntry, class string, site, n, i int) string {
	pattern := f.Name
	if pattern == "" {
		if f.Sites > 0 {
			pattern = "{class}-{site:02}-{n:02}"
		} else {
			pattern = "{class}-{i:04}"
		}
	}
	var b strings.Builder
	rest := pattern
	for {
		start := strings.IndexByte(rest, '{')
		if start < 0 {
			b.WriteString(rest)
			break
		}
		end := strings.IndexByte(rest[start:], '}')
		if end < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])
		key, width, _ := strings.Cut(rest[start+1:start+end], ":")
		val := ""
		switch key {
		case "class":
			val = class
		case "site", "n", "i":
			num := map[string]int{"site": site, "n": n, "i": i}[key]
			val = strconv.Itoa(num)
			if wd, err := strconv.Atoi(width); err == nil {
				val = fmt.Sprintf("%0*d", wd, num)
			}
		}
		b.WriteString(val)
		rest = rest[start+end+1:]
	}
	return b.String()
}

func compileFaults(w *ir.World, s *spec.Seed, classIdx map[string]int) error {
	for fi, f := range s.Faults {
		at, _ := spec.ParseTimePoint(string(f.At))
		t, _ := spec.ParseTarget(f.Target)
		cf := ir.Fault{
			ID:        uint32(fi + 1),
			Label:     f.Label,
			Target:    f.Target,
			Start:     int64(at / w.Tick),
			End:       w.Ticks,
			Member:    make([]bool, len(w.Devices)),
			EventMult: make([]float64, len(w.Events)),
		}
		if cf.Label == "" {
			cf.Label = fmt.Sprintf("fault %d on %s", fi+1, f.Target)
		}
		if f.For != "" {
			d, _ := spec.ParseDuration(string(f.For))
			cf.End = cf.Start + int64(d/w.Tick)
		}
		ci := classIdx[t.Class]
		for di, d := range w.Devices {
			if d.Class == ci && (t.Site < 0 || d.Site == t.Site) && (t.Index < 0 || d.Index == t.Index) {
				cf.Member[di] = true
			}
		}
		for ei := range cf.EventMult {
			cf.EventMult[ei] = 1
		}
		for _, e := range f.Effects {
			if e.Health != nil {
				cf.HealthDelta += *e.Health
			}
			if e.Multiply == nil {
				continue
			}
			for ei, ev := range w.Events {
				if ev.Class != ci {
					continue
				}
				if e.Multiply.Tag == "*" || hasTag(ev.Tags, e.Multiply.Tag) {
					cf.EventMult[ei] *= e.Multiply.By
				}
			}
		}
		w.Faults = append(w.Faults, cf)
	}
	return nil
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

func compileResponder(w *ir.World, s *spec.Seed) {
	if s.Operators == nil || s.Operators.Responder == nil {
		return
	}
	r := s.Operators.Responder
	delay, _ := spec.ParseDurationDist(string(r.Delay))
	cr := &ir.Responder{Delay: delay}
	for _, c := range w.Classes {
		states := make([]bool, len(c.States))
		for _, sn := range r.States {
			if i := c.StateIndex(sn); i >= 0 {
				states[i] = true
			}
		}
		cmd := -1
		for i, cm := range c.Commands {
			if cm.Name == r.Command {
				cmd = i
			}
		}
		cr.States = append(cr.States, states)
		cr.Command = append(cr.Command, cmd)
	}
	w.Responder = cr
}
