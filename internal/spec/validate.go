package spec

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/taituo/simo/internal/tmpl"
)

// Severity of a validation issue.
type Severity int

// Severities.
const (
	Error Severity = iota
	Warning
)

// Issue is one validation finding, with the YAML path it concerns.
type Issue struct {
	Severity Severity
	Path     string
	Msg      string
}

func (i Issue) String() string {
	sev := "error"
	if i.Severity == Warning {
		sev = "warning"
	}
	if i.Path == "" {
		return fmt.Sprintf("%s: %s", sev, i.Msg)
	}
	return fmt.Sprintf("%s: %s: %s", sev, i.Path, i.Msg)
}

// Issues is a list of findings.
type Issues []Issue

// Errors returns only the errors.
func (is Issues) Errors() Issues {
	var out Issues
	for _, i := range is {
		if i.Severity == Error {
			out = append(out, i)
		}
	}
	return out
}

// Err returns an error listing all errors, or nil if there are none.
func (is Issues) Err() error {
	errs := is.Errors()
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.String()
	}
	return errors.New("invalid seed:\n  " + strings.Join(msgs, "\n  "))
}

// Levels are the accepted event levels, in increasing severity.
var Levels = []string{"DEBUG", "INFO", "NOTICE", "WARN", "ERROR", "CRIT"}

// LevelIndex returns the index of a level name, or -1.
func LevelIndex(name string) int {
	for i, l := range Levels {
		if strings.EqualFold(l, name) {
			return i
		}
	}
	if strings.EqualFold(name, "WARNING") {
		return 3
	}
	return -1
}

// LintVolumePerDay is the expected daily line count above which the linter
// warns, so a typo does not create a terabyte.
const LintVolumePerDay = 1e8

type validator struct {
	s      *Seed
	issues Issues
}

func (v *validator) errf(path, format string, a ...any) {
	v.issues = append(v.issues, Issue{Error, path, fmt.Sprintf(format, a...)})
}

func (v *validator) warnf(path, format string, a ...any) {
	v.issues = append(v.issues, Issue{Warning, path, fmt.Sprintf(format, a...)})
}

// Validate checks a seed and lints it. Errors make the seed unusable;
// warnings point at likely mistakes.
func Validate(s *Seed) Issues {
	v := &validator{s: s}
	v.top()
	v.patterns()
	v.vocab()
	v.classes()
	v.fleet()
	v.faults()
	v.operators()
	if len(v.issues.Errors()) == 0 {
		v.lint()
	}
	return v.issues
}

func (v *validator) top() {
	s := v.s
	if s.Simo != Version {
		v.errf("simo", "format version %d is not supported (want %d)", s.Simo, Version)
	}
	if s.Name == "" {
		v.warnf("name", "no name; outputs will be called %q", "world")
	}
	if _, err := time.Parse(time.RFC3339Nano, string(s.Clock.Start)); err != nil {
		v.errf("clock.start", "want an RFC 3339 time like 2026-10-05T00:00:00Z, got %q", s.Clock.Start)
	}
	dur, err := Seconds(string(s.Clock.Duration))
	if err != nil || dur <= 0 {
		v.errf("clock.duration", "want a positive duration like 7d, got %q", s.Clock.Duration)
	}
	tick, err := Seconds(string(s.Clock.Tick))
	switch {
	case err != nil || tick <= 0:
		v.errf("clock.tick", "want a positive duration like 1s, got %q", s.Clock.Tick)
	case tick < 1e-3:
		v.errf("clock.tick", "tick %q is below 1ms", s.Clock.Tick)
	case dur > 0 && tick > dur:
		v.errf("clock.tick", "tick is longer than the whole run")
	case dur > 0 && dur/tick > 1e11:
		v.errf("clock", "%.3g ticks is too many; use a longer tick or a shorter duration", dur/tick)
	}
}

func (v *validator) patterns() {
	for _, name := range v.s.Patterns.Keys {
		p := v.s.Patterns.Values[name]
		path := "patterns." + name
		if !identRe.MatchString(name) {
			v.errf(path, "pattern names must be identifiers")
		}
		if p.Fourier == nil {
			v.errf(path, "only fourier patterns are supported so far")
			continue
		}
		if sec, err := Seconds(string(p.Fourier.Period)); err != nil || sec <= 0 {
			v.errf(path+".fourier.period", "want a positive duration, got %q", p.Fourier.Period)
		}
		if len(p.Fourier.Harmonics) == 0 {
			v.errf(path+".fourier.harmonics", "need at least one [k, amplitude, peak]")
		}
		for i, h := range p.Fourier.Harmonics {
			hp := fmt.Sprintf("%s.fourier.harmonics[%d]", path, i)
			if len(h) != 3 {
				v.errf(hp, "want [k, amplitude, peak], like [1, 0.9, 14h]")
				continue
			}
			if k, err := strconv.ParseFloat(string(h[0]), 64); err != nil || k <= 0 {
				v.errf(hp, "k must be a positive number, got %q", h[0])
			}
			if _, err := strconv.ParseFloat(string(h[1]), 64); err != nil {
				v.errf(hp, "amplitude must be a number, got %q", h[1])
			}
			if _, err := Seconds(string(h[2])); err != nil {
				v.errf(hp, "peak must be a time within the period, like 14h, got %q", h[2])
			}
		}
	}
}

func (v *validator) vocab() {
	for _, name := range v.s.Vocab.Keys {
		w := v.s.Vocab.Values[name]
		if len(w.Words) == 0 {
			v.errf("vocab."+name+".words", "a vocab needs at least one word")
		}
		if w.Zipf < 0 {
			v.errf("vocab."+name+".zipf", "zipf exponent must not be negative")
		}
	}
}

func (v *validator) classes() {
	if v.s.Classes.Len() == 0 {
		v.errf("classes", "define at least one device class")
	}
	for _, cname := range v.s.Classes.Keys {
		v.class(cname, v.s.Classes.Values[cname])
	}
}

func (v *validator) class(cname string, c Class) {
	path := "classes." + cname
	if !identRe.MatchString(cname) {
		v.errf(path, "class names must be identifiers")
	}
	if c.States.Len() == 0 {
		v.errf(path+".states", "a class needs at least one state")
		return
	}
	if c.Initial != "" && c.States.Index(c.Initial) < 0 {
		v.errf(path+".initial", "unknown state %q", c.Initial)
	}
	events := map[string]bool{}
	for i, e := range c.Events {
		if events[e.ID] {
			v.errf(fmt.Sprintf("%s.events[%d].id", path, i), "duplicate event id %q", e.ID)
		}
		events[e.ID] = true
	}
	usesHealth := false
	for _, sname := range c.States.Keys {
		st := c.States.Values[sname]
		sp := path + ".states." + sname
		fixed := 0
		for _, to := range st.To.Keys {
			tp := sp + ".to." + to
			if c.States.Index(to) < 0 {
				v.errf(tp, "unknown target state %q", to)
			}
			tr, err := ParseTransition(string(st.To.Values[to]))
			if err != nil {
				v.errf(tp, "%v", err)
				continue
			}
			switch tr.Kind {
			case TransFixed:
				fixed++
			case TransHealth:
				usesHealth = true
			case TransCommand:
				if c.Commands.Index(tr.Command) < 0 {
					v.errf(tp, "unknown command %q", tr.Command)
				}
			}
		}
		if fixed > 1 {
			v.errf(sp+".to", "at most one fixed() transition per state")
		}
		for _, e := range st.Emit {
			if !events[e] {
				v.errf(sp+".emit", "unknown event %q", e)
			}
		}
	}
	if c.Health != nil {
		if _, err := ParseRate(string(c.Health.Drift)); err != nil && c.Health.Drift != "" {
			v.errf(path+".health.drift", "%v", err)
		}
		if c.Health.Noise < 0 {
			v.errf(path+".health.noise", "noise must not be negative")
		}
	} else if usesHealth {
		v.warnf(path, "health_rate transitions but no health block; health stays at 1")
	}
	for _, mname := range c.Metrics.Keys {
		m := c.Metrics.Values[mname]
		mp := path + ".metrics." + mname
		if !identRe.MatchString(mname) {
			v.errf(mp, "metric names must be identifiers")
		}
		if m.OU == nil {
			v.errf(mp, "only ou metrics are supported so far")
			continue
		}
		if th, err := ParseRate(string(m.OU.Theta)); err != nil || th <= 0 {
			v.errf(mp+".ou.theta", "want a positive rate like 1/10m, got %q", m.OU.Theta)
		}
		if m.OU.Sigma < 0 {
			v.errf(mp+".ou.sigma", "sigma must not be negative")
		}
		for s := range m.InState {
			if c.States.Index(s) < 0 {
				v.errf(mp+".in_state."+s, "unknown state %q", s)
			}
		}
	}
	for i, e := range c.Events {
		v.event(fmt.Sprintf("%s.events[%d]", path, i), c, e)
	}
	for _, cmd := range c.Commands.Keys {
		cm := c.Commands.Values[cmd]
		cp := path + ".commands." + cmd
		if len(cm.From) == 0 {
			v.errf(cp+".from", "list the states the command can run from")
		}
		for _, s := range cm.From {
			if c.States.Index(s) < 0 {
				v.errf(cp+".from", "unknown state %q", s)
			}
		}
		if c.States.Index(cm.To) < 0 {
			v.errf(cp+".to", "unknown state %q", cm.To)
		}
		for _, e := range cm.Emit {
			if !events[e] {
				v.errf(cp+".emit", "unknown event %q", e)
			}
		}
	}
}

func (v *validator) event(path string, c Class, e Event) {
	if !identRe.MatchString(e.ID) {
		v.errf(path+".id", "event ids must be identifiers, got %q", e.ID)
	}
	if LevelIndex(e.Level) < 0 {
		v.errf(path+".level", "unknown level %q (want one of %s)", e.Level, strings.Join(Levels, ", "))
	}
	t, err := tmpl.Parse(e.Text)
	if err != nil {
		v.errf(path+".text", "%v", err)
	} else {
		for _, sl := range t.Slots {
			v.slot(path+".text", c, sl)
		}
	}
	r, err := ParseRateExpr(string(e.Rate))
	if err != nil {
		v.errf(path+".rate", "%v", err)
	}
	for _, p := range r.Patterns {
		if v.s.Patterns.Index(p) < 0 {
			v.errf(path+".rate", "unknown pattern %q", p)
		}
	}
	for s, m := range e.InState {
		if c.States.Index(s) < 0 {
			v.errf(path+".in_state."+s, "unknown state %q", s)
		}
		if _, err := ParseMultiplier(string(m)); err != nil {
			v.errf(path+".in_state."+s, "%v", err)
		}
	}
	for _, s := range e.OnlyIn {
		if c.States.Index(s) < 0 {
			v.errf(path+".only_in", "unknown state %q", s)
		}
	}
	if e.Excite != nil {
		v.warnf(path+".excite", "self-excitation (Hawkes bursts) arrives in phase 4; ignored for now")
	}
}

func (v *validator) slot(path string, c Class, sl tmpl.Slot) {
	info, ok := tmpl.Generators[sl.Gen]
	if !ok {
		v.errf(path, "{%s}: unknown generator %q", sl.Name, sl.Gen)
		return
	}
	if len(sl.Args) < info.MinArgs || (info.MaxArgs >= 0 && len(sl.Args) > info.MaxArgs) {
		v.errf(path, "{%s}: wrong number of arguments for %s", sl.Name, info.Doc)
		return
	}
	numeric := func(args ...string) {
		for _, a := range args {
			if _, err := strconv.ParseFloat(a, 64); err != nil {
				v.errf(path, "{%s}: argument %q is not a number", sl.Name, a)
			}
		}
	}
	switch sl.Gen {
	case "hex":
		if n, err := strconv.Atoi(sl.Args[0]); err != nil || n < 1 || n > 64 {
			v.errf(path, "{%s}: hex(n) needs 1 <= n <= 64", sl.Name)
		}
	case "int", "uniform", "normal", "lognormal", "exp":
		numeric(sl.Args...)
	case "vocab":
		name := sl.Name
		if len(sl.Args) == 1 {
			name = sl.Args[0]
		}
		if v.s.Vocab.Index(name) < 0 {
			v.errf(path, "{%s}: unknown vocab %q", sl.Name, name)
		}
	case "ip":
		if p, err := netip.ParsePrefix(sl.Args[0]); err != nil || !p.Addr().Is4() {
			v.errf(path, "{%s}: ip() needs an IPv4 CIDR like 192.0.2.0/24", sl.Name)
		}
	case "metric":
		if c.Metrics.Index(sl.Args[0]) < 0 {
			v.errf(path, "{%s}: unknown metric %q", sl.Name, sl.Args[0])
		}
	}
}

var namePlaceholderRe = regexp.MustCompile(`\{([a-z]+)(?::(\d+))?\}`)

func (v *validator) fleet() {
	if len(v.s.Fleet) == 0 {
		v.errf("fleet", "add at least one fleet entry, like {class: x, count: 10}")
	}
	for i, f := range v.s.Fleet {
		path := fmt.Sprintf("fleet[%d]", i)
		c, ok := v.s.Classes.Get(f.Class)
		if !ok {
			v.errf(path+".class", "unknown class %q", f.Class)
		}
		switch {
		case f.Sites < 0:
			v.errf(path+".sites", "sites must not be negative")
		case f.Sites > 0:
			if f.PerSite == "" {
				v.errf(path+".per_site", "set per_site when sites is set, like per_site: uniform(4, 8)")
			} else if _, err := ParseDist(string(f.PerSite)); err != nil {
				v.errf(path+".per_site", "%v", err)
			}
		default:
			if f.Count == "" {
				v.errf(path+".count", "set count, or sites and per_site")
			} else if _, err := ParseDist(string(f.Count)); err != nil {
				v.errf(path+".count", "%v", err)
			}
		}
		for _, m := range namePlaceholderRe.FindAllStringSubmatch(f.Name, -1) {
			switch m[1] {
			case "site", "n", "i", "class":
			default:
				v.errf(path+".name", "unknown placeholder {%s}; use {site}, {n}, {i} or {class}", m[1])
			}
		}
		for key, d := range f.Jitter {
			jp := path + ".jitter." + key
			if key != "rate_scale" {
				metric, ok := strings.CutSuffix(key, ".mean")
				if !ok || c.Metrics.Index(metric) < 0 {
					v.errf(jp, "jitter keys are rate_scale or <metric>.mean")
				}
			}
			if _, err := ParseDist(string(d)); err != nil {
				v.errf(jp, "%v", err)
			}
		}
	}
}

func (v *validator) faults() {
	for i, f := range v.s.Faults {
		path := fmt.Sprintf("faults[%d]", i)
		if _, err := ParseTimePoint(string(f.At)); err != nil {
			v.errf(path+".at", "%v", err)
		}
		if f.For != "" {
			if d, err := Seconds(string(f.For)); err != nil || d <= 0 {
				v.errf(path+".for", "want a positive duration, got %q", f.For)
			}
		}
		t, err := ParseTarget(f.Target)
		if err != nil {
			v.errf(path+".target", "%v", err)
			continue
		}
		c, ok := v.s.Classes.Get(t.Class)
		if !ok {
			v.errf(path+".target", "unknown class %q", t.Class)
			continue
		}
		if len(f.Effects) == 0 {
			v.errf(path+".effects", "a fault needs at least one effect")
		}
		for j, e := range f.Effects {
			ep := fmt.Sprintf("%s.effects[%d]", path, j)
			n := 0
			if e.Multiply != nil {
				n++
				if e.Multiply.By < 0 {
					v.errf(ep, "multiply.by must not be negative")
				}
				if !tagUsed(c, e.Multiply.Tag) {
					v.warnf(ep, "no %s event has tag %q, so this effect does nothing", t.Class, e.Multiply.Tag)
				}
			}
			if e.Health != nil {
				n++
			}
			if n != 1 {
				v.errf(ep, "each effect is exactly one of multiply or health")
			}
		}
	}
}

func tagUsed(c Class, tag string) bool {
	if tag == "*" {
		return len(c.Events) > 0
	}
	for _, e := range c.Events {
		for _, t := range e.Tags {
			if t == tag {
				return true
			}
		}
	}
	return false
}

func (v *validator) operators() {
	if v.s.Operators == nil || v.s.Operators.Responder == nil {
		return
	}
	r := v.s.Operators.Responder
	path := "operators.responder"
	if len(r.States) == 0 {
		v.errf(path+".states", "list the states the responder reacts to")
	}
	for _, s := range r.States {
		found := false
		for _, c := range v.s.Classes.Values {
			if c.States.Index(s) >= 0 {
				found = true
			}
		}
		if !found {
			v.errf(path+".states", "no class has state %q", s)
		}
	}
	found := false
	for _, c := range v.s.Classes.Values {
		if c.Commands.Index(r.Command) >= 0 {
			found = true
		}
	}
	if !found {
		v.errf(path+".command", "no class has command %q", r.Command)
	}
	if d, err := ParseDurationDist(string(r.Delay)); err != nil {
		v.errf(path+".delay", "%v", err)
	} else if d.Mean() < 0 {
		v.errf(path+".delay", "delay must not be negative")
	}
}

// lint looks for seeds that are valid but probably wrong.
func (v *validator) lint() {
	responds := map[string]bool{}
	if v.s.Operators != nil && v.s.Operators.Responder != nil {
		for _, s := range v.s.Operators.Responder.States {
			responds[s] = true
		}
	}
	for _, cname := range v.s.Classes.Keys {
		c := v.s.Classes.Values[cname]
		path := "classes." + cname
		initial := c.Initial
		if initial == "" {
			initial = c.States.Keys[0]
		}
		// Reachability over all transitions and commands.
		next := map[string][]string{}
		for _, s := range c.States.Keys {
			next[s] = append(next[s], c.States.Values[s].To.Keys...)
		}
		for _, cmd := range c.Commands.Values {
			for _, f := range cmd.From {
				next[f] = append(next[f], cmd.To)
			}
		}
		seen := map[string]bool{initial: true}
		queue := []string{initial}
		for len(queue) > 0 {
			s := queue[0]
			queue = queue[1:]
			for _, n := range next[s] {
				if !seen[n] {
					seen[n] = true
					queue = append(queue, n)
				}
			}
		}
		for _, s := range c.States.Keys {
			if !seen[s] {
				v.warnf(path+".states."+s, "unreachable from the initial state %q", initial)
			}
		}
		// States that only a command can leave.
		for _, s := range c.States.Keys {
			if !seen[s] {
				continue
			}
			st := c.States.Values[s]
			auto := false
			for _, to := range st.To.Keys {
				tr, _ := ParseTransition(string(st.To.Values[to]))
				if tr.Kind != TransCommand {
					auto = true
				}
			}
			byCommand := false
			for _, cmd := range c.Commands.Values {
				for _, f := range cmd.From {
					if f == s {
						byCommand = true
					}
				}
			}
			switch {
			case auto:
			case byCommand && !responds[s]:
				v.warnf(path+".states."+s, "only a command can leave this state and no operators.responder covers it; in batch runs devices stay here forever")
			case !byCommand && st.To.Len() == 0:
				v.warnf(path+".states."+s, "absorbing state: nothing ever leaves it")
			}
		}
		// Events that can never appear.
		emitted := map[string]bool{}
		for _, st := range c.States.Values {
			for _, e := range st.Emit {
				emitted[e] = true
			}
		}
		for _, cmd := range c.Commands.Values {
			for _, e := range cmd.Emit {
				emitted[e] = true
			}
		}
		for i, e := range c.Events {
			r, _ := ParseRateExpr(string(e.Rate))
			if r.PerSec == 0 && !emitted[e.ID] {
				v.warnf(fmt.Sprintf("%s.events[%d]", path, i), "event %q has no rate and is never emitted on enter or by a command", e.ID)
			}
		}
	}
	// Rough volume estimate (patterns and states taken as 1).
	perDay := 0.0
	for _, f := range v.s.Fleet {
		c := v.s.Classes.Values[f.Class]
		devices := 0.0
		if f.Sites > 0 {
			d, _ := ParseDist(string(f.PerSite))
			devices = float64(f.Sites) * d.Mean()
		} else {
			d, _ := ParseDist(string(f.Count))
			devices = d.Mean()
		}
		for _, e := range c.Events {
			r, _ := ParseRateExpr(string(e.Rate))
			perDay += devices * r.PerSec * 86400
		}
	}
	if perDay > LintVolumePerDay {
		v.warnf("fleet", "roughly %.3g lines per simulated day; check units and counts", perDay)
	}
}
