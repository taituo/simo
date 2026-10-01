// Package engine is the CPU reference engine. It steps every device tick by
// tick, using only counter-based draws, so the output is a pure function of
// the compiled world and does not depend on the number of workers.
//
// Per device and tick the order is fixed:
//
//  1. fault starts (health effects)
//  2. manual and operator commands
//  3. slow state every SlowEvery ticks: health drift, metric OU steps
//  4. the timer transition, else stochastic transitions (at most one
//     transition per device per tick)
//  5. event counts: rate x state multiplier x patterns x faults -> Poisson
package engine

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"sync"

	"github.com/taituo/simo/internal/dist"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/rng"
)

// Random streams. Each model component draws from its own stream, so adding
// a template does not shift the draws of anything else.
const (
	streamHealth  = 1
	streamMetric  = 2
	streamTrans   = 3
	streamOps     = 4
	streamInit    = 6
	streamCount   = 0x10000 // + event id
	streamEmit    = 0x20000 // + event id
	streamEnter   = 0x30000 // + event id
	streamCommand = 0x40000 // + event id
)

// CauseKind says why a transition happened.
type CauseKind uint8

// Transition causes.
const (
	CauseRate     CauseKind = iota // stochastic transition
	CauseTimer                     // fixed() timer
	CauseOperator                  // simulated responder ran a command
	CauseCommand                   // manual command (CLI, MCP, tests)
)

// Transition is one state change, reported in deterministic order.
type Transition struct {
	Tick    int64
	Device  uint32
	From    int
	To      int
	Cause   CauseKind
	Command int // command index for operator and manual causes
}

// MetricSample is one metric reading.
type MetricSample struct {
	Tick   int64
	Device uint32
	Metric int
	Value  float64
}

// Command is a manual command applied at a tick.
type Command struct {
	Tick    int64
	Device  uint32
	Command string
}

// Options control a run.
type Options struct {
	// From and To bound the output window in ticks; To 0 means the whole run.
	// The engine always simulates from tick 0 (checkpoints come in phase 2).
	From, To int64
	// Workers is the number of goroutines; 0 means GOMAXPROCS.
	Workers int
	// BatchTicks is the number of ticks per batch; 0 means 256.
	BatchTicks int64
	// OnTransition, if set, receives every transition in the window.
	OnTransition func(Transition)
	// MetricEvery is the number of ticks between metric samples, rounded up
	// to a multiple of the world's SlowEvery; 0 means no samples.
	MetricEvery int64
	// OnMetric receives metric samples.
	OnMetric func(MetricSample)
	// Commands are manual commands to apply.
	Commands []Command
	// Start resumes the run from a checkpoint instead of tick 0. From must
	// not be before Start.Tick.
	Start *Checkpoint
	// CheckpointEvery is the number of ticks between checkpoints sent to
	// OnCheckpoint; 0 means none. Checkpoints are taken at multiples of it,
	// also before the output window.
	CheckpointEvery int64
	// OnCheckpoint receives each checkpoint. It may keep it.
	OnCheckpoint func(*Checkpoint)
}

// Stats summarise a run's output window.
type Stats struct {
	Ticks       int64
	Records     int64
	ByEvent     []int64   // per global event
	Transitions int64     // all transitions in the window
	StateTicks  [][]int64 // [class][state] device-ticks in the window
}

// Sink receives each batch of records, sorted by time. The slice is reused
// after the call returns.
type Sink func([]record.Record) error

type sim struct {
	w          *ir.World
	dt, slowDt float64
	from, to   int64
	metEvery   int64
	trace      bool

	state    []uint16
	deadline []int64
	pendTick []int64
	health   []float64
	metric   []float64
	metOff   []int
	seq      []uint32
	seqOff   []int
	first    []int // first global event id per class

	ouA, ouB [][]float64 // per class, per metric

	manual map[int64][]manualCmd
}

type manualCmd struct {
	device  int
	command int
}

type shard struct {
	lo, hi     int
	recs       []record.Record
	trans      []Transition
	mets       []MetricSample
	byEvent    []int64
	stateTicks [][]int64
	nTrans     int64
}

// Run simulates the world and sends records in the window to sink.
func Run(w *ir.World, opt Options, sink Sink) (*Stats, error) {
	s := &sim{w: w, dt: w.TickSeconds(), from: opt.From, to: opt.To, trace: opt.OnTransition != nil}
	if s.to <= 0 || s.to > w.Ticks {
		s.to = w.Ticks
	}
	if s.from < 0 || s.from > s.to {
		return nil, fmt.Errorf("window [%d, %d) is outside the run", opt.From, s.to)
	}
	if opt.MetricEvery > 0 && opt.OnMetric != nil {
		s.metEvery = (opt.MetricEvery + w.SlowEvery - 1) / w.SlowEvery * w.SlowEvery
	}
	s.slowDt = float64(w.SlowEvery) * s.dt
	if err := s.init(opt.Commands); err != nil {
		return nil, err
	}
	begin := int64(0)
	if opt.Start != nil {
		if err := s.load(opt.Start); err != nil {
			return nil, err
		}
		begin = opt.Start.Tick
		if s.from < begin {
			return nil, fmt.Errorf("window starts at tick %d, before the checkpoint at tick %d", s.from, begin)
		}
	}
	every := opt.CheckpointEvery
	if opt.OnCheckpoint == nil {
		every = 0
	}

	workers := opt.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > len(w.Devices) {
		workers = len(w.Devices)
	}
	shards := make([]*shard, workers)
	for i := range shards {
		sh := &shard{
			lo:      i * len(w.Devices) / workers,
			hi:      (i + 1) * len(w.Devices) / workers,
			byEvent: make([]int64, len(w.Events)),
		}
		for _, c := range w.Classes {
			sh.stateTicks = append(sh.stateTicks, make([]int64, len(c.States)))
		}
		shards[i] = sh
	}

	batch := opt.BatchTicks
	if batch <= 0 {
		batch = 256
	}
	np := len(w.Patterns)
	pf := make([]float64, batch*int64(np))
	startUnix := float64(w.Start.UnixNano()) / 1e9
	var merged []record.Record
	var trans []Transition
	var mets []MetricSample

	for b0, b1 := begin, begin; b0 < s.to; b0 = b1 {
		b1 = min(b0+batch, s.to)
		if every > 0 {
			if b0%every == 0 && (b0 > begin || opt.Start == nil) {
				opt.OnCheckpoint(s.snapshot(b0))
			}
			b1 = min(b1, (b0/every+1)*every)
		}
		for t := b0; t < b1; t++ {
			unix := startUnix + float64(t)*s.dt
			for p := range w.Patterns {
				pf[(t-b0)*int64(np)+int64(p)] = w.Patterns[p].At(unix)
			}
		}
		var active []int
		for fi, f := range w.Faults {
			if f.Start < b1 && f.End > b0 {
				active = append(active, fi)
			}
		}
		var wg sync.WaitGroup
		for _, sh := range shards {
			wg.Add(1)
			go func(sh *shard) {
				defer wg.Done()
				s.runShard(sh, b0, b1, pf, active)
			}(sh)
		}
		wg.Wait()

		merged, trans, mets = merged[:0], trans[:0], mets[:0]
		for _, sh := range shards {
			merged = append(merged, sh.recs...)
			trans = append(trans, sh.trans...)
			mets = append(mets, sh.mets...)
			sh.recs, sh.trans, sh.mets = sh.recs[:0], sh.trans[:0], sh.mets[:0]
		}
		if b1 <= s.from {
			continue
		}
		slices.SortFunc(merged, compareRecords)
		slices.SortStableFunc(trans, func(a, b Transition) int {
			if a.Tick != b.Tick {
				return cmpInt64(a.Tick, b.Tick)
			}
			return int(a.Device) - int(b.Device)
		})
		slices.SortStableFunc(mets, func(a, b MetricSample) int {
			if a.Tick != b.Tick {
				return cmpInt64(a.Tick, b.Tick)
			}
			if a.Device != b.Device {
				return int(a.Device) - int(b.Device)
			}
			return a.Metric - b.Metric
		})
		if len(merged) > 0 {
			if err := sink(merged); err != nil {
				return nil, err
			}
		}
		for _, tr := range trans {
			opt.OnTransition(tr)
		}
		for _, m := range mets {
			opt.OnMetric(m)
		}
	}

	if every > 0 && s.to%every == 0 && s.to < w.Ticks && s.to > begin {
		opt.OnCheckpoint(s.snapshot(s.to))
	}

	st := &Stats{Ticks: s.to - s.from, ByEvent: make([]int64, len(w.Events))}
	for _, c := range w.Classes {
		st.StateTicks = append(st.StateTicks, make([]int64, len(c.States)))
	}
	for _, sh := range shards {
		for e, n := range sh.byEvent {
			st.ByEvent[e] += n
			st.Records += n
		}
		for c := range sh.stateTicks {
			for i, n := range sh.stateTicks[c] {
				st.StateTicks[c][i] += n
			}
		}
		st.Transitions += sh.nTrans
	}
	return st, nil
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareRecords is a total order, so the merged stream does not depend on
// how devices were split across workers.
func compareRecords(a, b record.Record) int {
	if a.TS != b.TS {
		return cmpInt64(a.TS, b.TS)
	}
	if a.Device != b.Device {
		return int(a.Device) - int(b.Device)
	}
	if a.Template != b.Template {
		return int(a.Template) - int(b.Template)
	}
	for i := range a.Slots {
		if a.Slots[i] != b.Slots[i] {
			return cmpInt64(int64(a.Slots[i]), int64(b.Slots[i]))
		}
	}
	return 0
}

func (s *sim) init(cmds []Command) error {
	w := s.w
	n := len(w.Devices)
	s.state = make([]uint16, n)
	s.deadline = make([]int64, n)
	s.pendTick = make([]int64, n)
	s.health = make([]float64, n)
	s.metOff = make([]int, n+1)
	s.seqOff = make([]int, n+1)
	for _, c := range w.Classes {
		first := 0
		if len(c.Events) > 0 {
			first = c.Events[0]
		}
		s.first = append(s.first, first)
		a := make([]float64, len(c.Metrics))
		b := make([]float64, len(c.Metrics))
		for m, mm := range c.Metrics {
			a[m] = math.Exp(-mm.Theta * s.slowDt)
			b[m] = math.Sqrt(1 - a[m]*a[m])
		}
		s.ouA = append(s.ouA, a)
		s.ouB = append(s.ouB, b)
	}
	for d, dev := range w.Devices {
		c := &w.Classes[dev.Class]
		s.metOff[d+1] = s.metOff[d] + len(c.Metrics)
		s.seqOff[d+1] = s.seqOff[d] + len(c.Events)
	}
	s.metric = make([]float64, s.metOff[n])
	s.seq = make([]uint32, s.seqOff[n])
	for d, dev := range w.Devices {
		c := &w.Classes[dev.Class]
		s.state[d] = uint16(c.Initial)
		s.health[d] = 1
		s.deadline[d] = -1
		s.pendTick[d] = -1
		if f := c.States[c.Initial].Fixed; f != nil {
			s.deadline[d] = f.Ticks
		}
		for m, mm := range c.Metrics {
			z := dist.NormalInv(rng.Unit(rng.Draw(dev.Key, streamInit, 0, uint32(m))[0]))
			s.metric[s.metOff[d]+m] = mm.Mean + dev.MetricOffset[m] + mm.StateShift[c.Initial] + mm.SD*z
		}
		s.scheduleResponder(d, 0)
	}
	s.manual = map[int64][]manualCmd{}
	for _, cm := range cmds {
		if int(cm.Device) >= n {
			return fmt.Errorf("command %q: no device %d", cm.Command, cm.Device)
		}
		c := &w.Classes[w.Devices[cm.Device].Class]
		ci := -1
		for i, x := range c.Commands {
			if x.Name == cm.Command {
				ci = i
			}
		}
		if ci < 0 {
			return fmt.Errorf("class %s has no command %q", c.Name, cm.Command)
		}
		s.manual[cm.Tick] = append(s.manual[cm.Tick], manualCmd{int(cm.Device), ci})
	}
	return nil
}

func (s *sim) runShard(sh *shard, b0, b1 int64, pf []float64, active []int) {
	np := int64(len(s.w.Patterns))
	for t := b0; t < b1; t++ {
		pfT := pf[(t-b0)*np : (t-b0+1)*np]
		slow := t%s.w.SlowEvery == 0
		ivs := s.manual[t]
		for d := sh.lo; d < sh.hi; d++ {
			s.step(sh, d, t, pfT, slow, active, ivs)
		}
	}
}

func (s *sim) step(sh *shard, d int, t int64, pf []float64, slow bool, active []int, ivs []manualCmd) {
	w := s.w
	dev := &w.Devices[d]
	cls := &w.Classes[dev.Class]
	moved := false

	for _, fi := range active {
		f := &w.Faults[fi]
		if f.Start == t && f.Member[d] && f.HealthDelta != 0 {
			s.health[d] = clamp01(s.health[d] + f.HealthDelta)
		}
	}
	for _, iv := range ivs {
		if iv.device == d && !moved {
			moved = s.command(sh, d, t, iv.command, CauseCommand)
		}
	}
	if s.pendTick[d] == t {
		s.pendTick[d] = -1
		if r := w.Responder; !moved && r.States[dev.Class][s.state[d]] && r.Command[dev.Class] >= 0 {
			moved = s.command(sh, d, t, r.Command[dev.Class], CauseOperator)
		}
		s.scheduleResponder(d, t) // re-arm if the device still needs attention
	}
	if slow {
		s.updateSlow(sh, d, t)
	}
	if !moved && s.deadline[d] == t {
		moved = true
		s.transition(sh, d, t, cls.States[s.state[d]].Fixed.To, CauseTimer, -1)
	}
	if !moved {
		st := &cls.States[s.state[d]]
		if len(st.Rates) > 0 {
			wd := rng.Draw(dev.Key, streamTrans, t, 0)
			if st.Const {
				if wd[0] < st.LeaveThr {
					i := 0
					for wd[1] >= st.PickThr[i] {
						i++
					}
					s.transition(sh, d, t, st.Rates[i].To, CauseRate, -1)
				}
			} else {
				total := 0.0
				for _, r := range st.Rates {
					total += s.rate(r, d)
				}
				if total > 0 && wd[0] < ir.Threshold(-math.Expm1(-total*s.dt)) {
					target := rng.Unit(wd[1]) * total
					pick, acc := len(st.Rates)-1, 0.0
					for i, r := range st.Rates {
						acc += s.rate(r, d)
						if target < acc {
							pick = i
							break
						}
					}
					s.transition(sh, d, t, st.Rates[pick].To, CauseRate, -1)
				}
			}
		}
	}

	cur := int(s.state[d])
	if t >= s.from {
		sh.stateTicks[dev.Class][cur]++
	}
	for _, e := range cls.Events {
		ev := &w.Events[e]
		m := ev.StateMult[cur]
		if ev.PerSec == 0 || m == 0 {
			continue
		}
		lam := ev.PerSec * dev.RateScale * m
		for _, p := range ev.Patterns {
			lam *= pf[p]
		}
		var cause uint32
		for _, fi := range active {
			f := &w.Faults[fi]
			if t < f.Start || t >= f.End || !f.Member[d] {
				continue
			}
			if fm := f.EventMult[e]; fm != 1 {
				lam *= fm
				if cause == 0 && fm > 1 && fm >= m {
					cause = f.ID
				}
			}
		}
		if lam <= 0 {
			continue
		}
		seq := rng.NewSeq(dev.Key, streamCount+uint32(e), t, 0)
		n := dist.Poisson(lam*s.dt, &seq)
		for i := 0; i < n; i++ {
			s.emit(sh, d, t, e, streamEmit, uint32(i), cause)
		}
	}
}

func (s *sim) rate(r ir.RateTrans, d int) float64 {
	if r.Health {
		return r.PerSec * math.Exp(r.Gamma*(1-s.health[d]))
	}
	return r.PerSec
}

func (s *sim) updateSlow(sh *shard, d int, t int64) {
	w := s.w
	dev := &w.Devices[d]
	cls := &w.Classes[dev.Class]
	if cls.Health {
		z := dist.NormalInv(rng.Unit(rng.Draw(dev.Key, streamHealth, t, 0)[0]))
		s.health[d] = clamp01(s.health[d] + cls.Drift*s.slowDt + cls.Noise*math.Sqrt(s.slowDt)*z)
	}
	out := s.metEvery > 0 && t >= s.from && t%s.metEvery == 0
	cur := s.state[d]
	for m := range cls.Metrics {
		mm := &cls.Metrics[m]
		z := dist.NormalInv(rng.Unit(rng.Draw(dev.Key, streamMetric, t, uint32(m))[0]))
		mean := mm.Mean + dev.MetricOffset[m] + mm.StateShift[cur]
		i := s.metOff[d] + m
		s.metric[i] = mean + (s.metric[i]-mean)*s.ouA[dev.Class][m] + mm.SD*s.ouB[dev.Class][m]*z
		if out {
			sh.mets = append(sh.mets, MetricSample{Tick: t, Device: uint32(d), Metric: m, Value: s.metric[i]})
		}
	}
}

func (s *sim) transition(sh *shard, d int, t int64, to int, cause CauseKind, cmd int) {
	w := s.w
	dev := &w.Devices[d]
	cls := &w.Classes[dev.Class]
	from := int(s.state[d])
	s.state[d] = uint16(to)
	st := &cls.States[to]
	s.deadline[d] = -1
	if st.Fixed != nil {
		s.deadline[d] = t + st.Fixed.Ticks
	}
	if t >= s.from {
		sh.nTrans++
		if s.trace {
			sh.trans = append(sh.trans, Transition{Tick: t, Device: uint32(d), From: from, To: to, Cause: cause, Command: cmd})
		}
	}
	for i, e := range st.Emit {
		s.emit(sh, d, t, e, streamEnter, uint32(i), 0)
	}
	s.scheduleResponder(d, t)
}

func (s *sim) scheduleResponder(d int, t int64) {
	r := s.w.Responder
	if r == nil || s.pendTick[d] >= 0 {
		return
	}
	dev := &s.w.Devices[d]
	if !r.States[dev.Class][s.state[d]] || r.Command[dev.Class] < 0 {
		return
	}
	u := rng.Unit(rng.Draw(dev.Key, streamOps, t, 0)[0])
	delay := int64(math.Round(r.Delay.Sample(u) / s.dt))
	s.pendTick[d] = t + max(delay, 1)
}

func (s *sim) command(sh *shard, d int, t int64, ci int, cause CauseKind) bool {
	dev := &s.w.Devices[d]
	cmd := &s.w.Classes[dev.Class].Commands[ci]
	to := cmd.To[s.state[d]]
	if to < 0 {
		return false
	}
	if cmd.ResetHealth {
		s.health[d] = 1
	}
	s.transition(sh, d, t, to, cause, ci)
	for i, e := range cmd.Emit {
		s.emit(sh, d, t, e, streamCommand, uint32(i), 0)
	}
	return true
}

func (s *sim) emit(sh *shard, d int, t int64, e int, stream uint32, idx uint32, cause uint32) {
	w := s.w
	dev := &w.Devices[d]
	ev := &w.Events[e]
	si := s.seqOff[d] + e - s.first[dev.Class]
	s.seq[si]++
	if t < s.from {
		return
	}
	a := rng.Draw(dev.Key, stream+uint32(e), t, idx*2)
	b := rng.Draw(dev.Key, stream+uint32(e), t, idx*2+1)
	r := record.Record{
		TS:       t*w.TickNs + int64((uint64(a[0])*uint64(w.TickNs))>>32),
		Device:   uint32(d),
		Template: uint16(e),
		Level:    ev.Level,
		Slots:    [5]uint32{a[1], a[2], a[3], b[0], b[1]},
		Cause:    cause,
	}
	for i, src := range ev.Slots {
		switch src.Kind {
		case ir.SlotMetric:
			r.Slots[i] = math.Float32bits(float32(s.metric[s.metOff[d]+src.Metric]))
		case ir.SlotSeq:
			r.Slots[i] = s.seq[si]
		}
	}
	sh.recs = append(sh.recs, r)
	sh.byEvent[e]++
}

func clamp01(x float64) float64 {
	return math.Min(1, math.Max(0, x))
}
