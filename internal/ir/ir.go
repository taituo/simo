// Package ir holds the compiled world: flat tables that every engine backend
// loads unchanged. Package compile builds it from a seed.
package ir

import (
	"math"
	"time"

	"github.com/taituo/simo/internal/dist"
	"github.com/taituo/simo/internal/rng"
	"github.com/taituo/simo/internal/tmpl"
)

// EngineVersion identifies the record stream an engine produces. "Same seed,
// same output" holds only for the same engine version; bump it deliberately
// when the stream for an example seed changes.
const EngineVersion = "0.2.0"

// patternEpoch is Monday 1970-01-05 00:00 UTC. Pattern time is measured from
// it, so weekly patterns start on Monday.
const patternEpoch = 345600

// World is a compiled seed.
type World struct {
	Name       string
	MasterSeed uint64
	SeedSHA256 string

	Start  time.Time
	Tick   time.Duration
	TickNs int64
	Ticks  int64 // ticks in the whole run

	// SlowEvery is the number of ticks between health and metric updates.
	SlowEvery int64

	Patterns  []Pattern
	Classes   []Class
	Events    []Event // global template table; record.Template indexes it
	Vocab     map[string]Vocab
	Devices   []Device
	Faults    []Fault
	Responder *Responder
}

// TickSeconds returns the tick length in seconds.
func (w *World) TickSeconds() float64 { return w.Tick.Seconds() }

// Pattern is a Fourier rate multiplier.
type Pattern struct {
	Name      string
	Period    float64 // seconds
	Harmonics []Harmonic
}

// Harmonic is one cosine term.
type Harmonic struct{ K, Amp, Peak float64 }

// At returns the multiplier at a Unix time in seconds, clamped at 0.
func (p *Pattern) At(unix float64) float64 {
	t := math.Mod(unix-patternEpoch, p.Period)
	f := 1.0
	for _, h := range p.Harmonics {
		f += h.Amp * math.Cos(2*math.Pi*h.K*(t-h.Peak)/p.Period)
	}
	return math.Max(f, 0)
}

// Vocab is a compiled word list.
type Vocab struct {
	Words []string
	Zipf  dist.Zipf
}

// Class is a compiled device class.
type Class struct {
	Name     string
	States   []State
	Initial  int
	Health   bool
	Drift    float64 // health change per second
	Noise    float64 // health noise per sqrt(second)
	Metrics  []Metric
	Events   []int // global event ids
	Commands []Command
}

// StateIndex returns the index of a state name, or -1.
func (c *Class) StateIndex(name string) int {
	for i, s := range c.States {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// State is a compiled state.
type State struct {
	Name  string
	Rates []RateTrans
	Fixed *FixedTrans
	Emit  []int // global event ids emitted on entering

	// For states whose rates are all constant, precomputed integer
	// thresholds: leave when draw < LeaveThr, then pick the first i with
	// draw2 < PickThr[i]. Integer decisions keep backends identical.
	Const    bool
	LeaveThr uint32
	PickThr  []uint32
}

// RateTrans is a stochastic transition.
type RateTrans struct {
	To     int
	PerSec float64
	Gamma  float64 // > 0 for health-dependent rates: PerSec*exp(Gamma*(1-health))
	Health bool
}

// FixedTrans leaves a state after a fixed number of ticks.
type FixedTrans struct {
	To    int
	Ticks int64
}

// Metric is a compiled OU gauge.
type Metric struct {
	Name       string
	Unit       string
	Mean       float64
	Theta      float64   // per second
	SD         float64   // stationary standard deviation
	StateShift []float64 // added to the mean, per state
}

// Event is a compiled log template.
type Event struct {
	ID        string
	Class     int
	Level     uint8
	Tags      []string
	PerSec    float64
	Patterns  []int
	StateMult []float64 // per state of the class
	Tmpl      *tmpl.Template
	Slots     []SlotSource
}

// SlotKind says where a slot's value comes from.
type SlotKind uint8

// Slot kinds.
const (
	SlotRandom SlotKind = iota // raw random draw, shaped by the renderer
	SlotMetric                 // the device's metric value, as float32 bits
	SlotSeq                    // per-device counter for the event
)

// SlotSource is how the engine fills one slot.
type SlotSource struct {
	Kind   SlotKind
	Metric int
}

// Command is a compiled command.
type Command struct {
	Name        string
	To          []int // destination per current state, -1 = not allowed
	ResetHealth bool
	Emit        []int
}

// Device is one simulated device.
type Device struct {
	ID           uint32
	Name         string
	Class        int
	Site         int // 1-based; 0 when the entry has no sites
	Index        int // 1-based within its site (or entry)
	RateScale    float64
	MetricOffset []float64
	Key          rng.Key
}

// Fault is a compiled scheduled fault. Fault ids start at 1; 0 means
// background in record.Cause.
type Fault struct {
	ID          uint32
	Label       string
	Target      string
	Start, End  int64     // ticks, End exclusive
	Member      []bool    // per device
	EventMult   []float64 // per global event; 1 = unaffected
	HealthDelta float64
}

// Responder is a compiled simulated operator.
type Responder struct {
	States  [][]bool // [class][state]
	Command []int    // per class, -1 = none
	Delay   dist.Dist
}

// Threshold converts a probability to a 32-bit integer threshold: an event
// with probability p happens when a uniform 32-bit draw is below it.
func Threshold(p float64) uint32 {
	switch {
	case p <= 0 || math.IsNaN(p):
		return 0
	case p >= 1:
		return math.MaxUint32
	}
	return uint32(p * 4294967296.0)
}
