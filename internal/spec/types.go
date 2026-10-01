// Package spec defines the seed file format: the YAML an LLM (or a person)
// writes to describe a world. It parses and validates seeds; package compile
// turns a valid seed into the engine's tables.
package spec

// Version is the seed format version this build reads.
const Version = 1

// Seed is a whole seed file.
type Seed struct {
	Simo       int                 `yaml:"simo"`
	Name       string              `yaml:"name"`
	MasterSeed uint64              `yaml:"master_seed"`
	Clock      Clock               `yaml:"clock"`
	Patterns   OrderedMap[Pattern] `yaml:"patterns"`
	Vocab      OrderedMap[Vocab]   `yaml:"vocab"`
	Classes    OrderedMap[Class]   `yaml:"classes"`
	Fleet      []FleetEntry        `yaml:"fleet"`
	Faults     []Fault             `yaml:"faults"`
	Operators  *Operators          `yaml:"operators"`
}

// Clock sets simulated time.
type Clock struct {
	Start    Expr `yaml:"start"`    // RFC 3339, e.g. 2026-10-05T00:00:00Z
	Duration Expr `yaml:"duration"` // e.g. 7d
	Tick     Expr `yaml:"tick"`     // e.g. 1s
}

// Pattern is a named rate multiplier over time.
type Pattern struct {
	Fourier *Fourier `yaml:"fourier"`
}

// Fourier is F(t) = 1 + sum amp*cos(2*pi*k*(t-peak)/period), clamped at 0.
// Each harmonic is [k, amp, peak]; peak is a time within the period.
// Pattern time is measured from Monday 00:00 UTC.
type Fourier struct {
	Period    Expr     `yaml:"period"`
	Harmonics [][]Expr `yaml:"harmonics"`
}

// Vocab is a word list for {name:vocab} slots.
type Vocab struct {
	Zipf  float64  `yaml:"zipf"` // 0 = uniform
	Words []string `yaml:"words"`
}

// Class is a device class: a hidden state machine plus what it emits.
type Class struct {
	Initial  string              `yaml:"initial"` // default: first state
	Health   *Health             `yaml:"health"`
	States   OrderedMap[State]   `yaml:"states"`
	Metrics  OrderedMap[Metric]  `yaml:"metrics"`
	Events   []Event             `yaml:"events"`
	Commands OrderedMap[Command] `yaml:"commands"`
}

// Health is a hidden value in [0, 1] that drifts and drives health_rate
// transitions.
type Health struct {
	Drift Expr    `yaml:"drift"` // e.g. -0.05/day
	Noise float64 `yaml:"noise"` // standard deviation per sqrt(day)
}

// State is one state of a class.
type State struct {
	// To maps target states to transition laws: "1/2h" (rate),
	// "health_rate(0.02/day, gamma: 4)", "fixed(90s)" or "on_command(restart)".
	To   OrderedMap[Expr] `yaml:"to"`
	Emit []string         `yaml:"emit"` // events emitted on entering the state
}

// Metric is a numeric gauge.
type Metric struct {
	Unit    string                 `yaml:"unit"`
	OU      *OU                    `yaml:"ou"`
	InState map[string]MetricShift `yaml:"in_state"`
}

// OU is a mean-reverting Ornstein-Uhlenbeck process.
type OU struct {
	Mean  float64 `yaml:"mean"`
	Theta Expr    `yaml:"theta"` // reversion rate, e.g. 1/10m
	Sigma float64 `yaml:"sigma"` // stationary standard deviation
}

// MetricShift changes a metric while the device is in a state.
type MetricShift struct {
	Mean float64 `yaml:"mean"` // added to the mean
}

// Event is a log line template and when it fires.
type Event struct {
	ID      string          `yaml:"id"`
	Level   string          `yaml:"level"`
	Text    string          `yaml:"text"`
	Rate    Expr            `yaml:"rate"` // e.g. "0.03/s * store_hours"; empty = only emitted on enter/commands
	Tags    []string        `yaml:"tags"`
	InState map[string]Expr `yaml:"in_state"` // e.g. {degraded: x40}
	OnlyIn  []string        `yaml:"only_in"`  // states where the event can fire; others x0
	Excite  *Excite         `yaml:"excite"`   // Hawkes self-excitation (phase 4; ignored for now)
}

// Excite is Hawkes self-excitation, planned for phase 4.
type Excite struct {
	Self *struct {
		Alpha float64 `yaml:"alpha"`
		Beta  Expr    `yaml:"beta"`
	} `yaml:"self"`
}

// Command is an action an operator (simulated or real) can take.
type Command struct {
	From        []string `yaml:"from"`
	To          string   `yaml:"to"`
	ResetHealth bool     `yaml:"reset_health"`
	Emit        []string `yaml:"emit"`
}

// FleetEntry expands one class into devices.
type FleetEntry struct {
	Class   string          `yaml:"class"`
	Count   Expr            `yaml:"count"`    // devices when there are no sites
	Sites   int             `yaml:"sites"`    // number of sites
	PerSite Expr            `yaml:"per_site"` // devices per site, e.g. uniform(4, 8)
	Name    string          `yaml:"name"`     // e.g. pos-{site:02}-{n:02}
	Jitter  map[string]Expr `yaml:"jitter"`   // rate_scale or <metric>.mean
}

// Fault is a scheduled fault.
type Fault struct {
	At      Expr     `yaml:"at"`     // "day2 09:15" or an offset like "26h"
	For     Expr     `yaml:"for"`    // duration; empty = until the end
	Target  string   `yaml:"target"` // site[17]/pos_terminal[*]
	Effects []Effect `yaml:"effects"`
	Label   string   `yaml:"label"`
}

// Effect is one effect of a fault.
type Effect struct {
	Multiply *struct {
		Tag string  `yaml:"tag"` // "*" matches every event
		By  float64 `yaml:"by"`
	} `yaml:"multiply"`
	Health *float64 `yaml:"health"` // added to health when the fault starts
}

// Operators are simulated people.
type Operators struct {
	Responder *Responder `yaml:"responder"`
}

// Responder notices devices entering some states and, after a delay, runs a
// command if the command is allowed in the device's state at that moment.
type Responder struct {
	States  []string `yaml:"states"`
	Command string   `yaml:"command"`
	Delay   Expr     `yaml:"delay"` // e.g. exp(30m), uniform(10m, 40m), 20m
}
