package spec

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/taituo/simo/internal/dist"
)

var unitSeconds = map[string]float64{
	"ns": 1e-9, "us": 1e-6, "ms": 1e-3,
	"s": 1, "sec": 1, "m": 60, "min": 60, "h": 3600, "hour": 3600,
	"d": 86400, "day": 86400, "days": 86400, "w": 604800, "week": 604800,
}

// Seconds parses a duration such as "90s", "10m", "1.5h", "7d" or a bare
// unit like "day" (one of it), and returns seconds.
func Seconds(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || (i == 0 && (s[i] == '-' || s[i] == '+'))) {
		i++
	}
	num, unit := s[:i], strings.TrimSpace(s[i:])
	n := 1.0
	if num != "" {
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		n = v
	}
	if unit == "" {
		if n == 0 {
			return 0, nil
		}
		return 0, fmt.Errorf("duration %q needs a unit (s, m, h, d)", s)
	}
	mult, ok := unitSeconds[unit]
	if !ok {
		return 0, fmt.Errorf("duration %q has unknown unit %q", s, unit)
	}
	return n * mult, nil
}

// ParseDuration parses a duration (see Seconds).
func ParseDuration(s string) (time.Duration, error) {
	sec, err := Seconds(s)
	if err != nil {
		return 0, err
	}
	return time.Duration(math.Round(sec * 1e9)), nil
}

// ParseRate parses "<number>/<duration>", such as "0.03/s", "1/2h" or
// "-0.05/day", and returns events per second.
func ParseRate(s string) (float64, error) {
	i := strings.IndexByte(s, '/')
	if i < 0 {
		return 0, fmt.Errorf("rate %q needs a time unit, like 0.5/s or 1/2h", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s[:i]), 64)
	if err != nil {
		return 0, fmt.Errorf("bad rate %q", s)
	}
	d, err := Seconds(s[i+1:])
	if err != nil {
		return 0, fmt.Errorf("bad rate %q: %v", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("rate %q divides by a non-positive time", s)
	}
	return n / d, nil
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RateExpr is a base rate times named patterns: "0.03/s * store_hours".
type RateExpr struct {
	PerSec   float64
	Patterns []string
}

// ParseRateExpr parses a rate expression. Empty or "0" means never.
func ParseRateExpr(s string) (RateExpr, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return RateExpr{}, nil
	}
	r := RateExpr{PerSec: 1}
	haveRate := false
	for _, term := range strings.Split(s, "*") {
		t := strings.TrimSpace(term)
		switch {
		case strings.Contains(t, "/"):
			if haveRate {
				return RateExpr{}, fmt.Errorf("rate %q has two per-time parts", s)
			}
			v, err := ParseRate(t)
			if err != nil {
				return RateExpr{}, err
			}
			r.PerSec *= v
			haveRate = true
		case identRe.MatchString(t):
			r.Patterns = append(r.Patterns, t)
		default:
			v, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return RateExpr{}, fmt.Errorf("rate %q: cannot read %q", s, t)
			}
			r.PerSec *= v
		}
	}
	if !haveRate {
		return RateExpr{}, fmt.Errorf("rate %q needs a per-time part, like 0.03/s", s)
	}
	if r.PerSec < 0 {
		return RateExpr{}, fmt.Errorf("rate %q is negative", s)
	}
	return r, nil
}

var callRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*\((.*)\)$`)

// parseCall splits "name(a, b)" into name and arguments.
func parseCall(s string) (name string, args []string, ok bool) {
	m := callRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", nil, false
	}
	if strings.TrimSpace(m[2]) != "" {
		for _, a := range strings.Split(m[2], ",") {
			args = append(args, strings.TrimSpace(a))
		}
	}
	return m[1], args, true
}

// TransKind is the law of a transition.
type TransKind int

// Transition laws.
const (
	TransRate    TransKind = iota // constant rate: "1/2h"
	TransHealth                   // health_rate(r0, gamma: g): r0*exp(g*(1-health))
	TransFixed                    // fixed(90s): after a fixed time in the state
	TransCommand                  // on_command(restart): only when a command runs
)

// Transition is a parsed transition law.
type Transition struct {
	Kind    TransKind
	PerSec  float64
	Gamma   float64
	Fixed   time.Duration
	Command string
}

// ParseTransition parses a transition law.
func ParseTransition(s string) (Transition, error) {
	name, args, isCall := parseCall(s)
	if !isCall {
		r, err := ParseRate(s)
		if err != nil {
			return Transition{}, err
		}
		if r < 0 {
			return Transition{}, fmt.Errorf("transition rate %q is negative", s)
		}
		return Transition{Kind: TransRate, PerSec: r}, nil
	}
	switch name {
	case "rate":
		if len(args) != 1 {
			return Transition{}, fmt.Errorf("rate() takes one argument")
		}
		return ParseTransition(args[0])
	case "fixed":
		if len(args) != 1 {
			return Transition{}, fmt.Errorf("fixed() takes one duration, like fixed(90s)")
		}
		d, err := ParseDuration(args[0])
		if err != nil {
			return Transition{}, err
		}
		if d <= 0 {
			return Transition{}, fmt.Errorf("fixed() needs a positive duration")
		}
		return Transition{Kind: TransFixed, Fixed: d}, nil
	case "on_command":
		if len(args) != 1 || !identRe.MatchString(args[0]) {
			return Transition{}, fmt.Errorf("on_command() takes one command name")
		}
		return Transition{Kind: TransCommand, Command: args[0]}, nil
	case "health_rate":
		if len(args) != 2 {
			return Transition{}, fmt.Errorf("health_rate() takes a rate and a gamma, like health_rate(0.02/day, gamma: 4)")
		}
		r, err := ParseRate(args[0])
		if err != nil {
			return Transition{}, err
		}
		g := strings.TrimSpace(args[1])
		for _, p := range []string{"gamma:", "gamma="} {
			g = strings.TrimSpace(strings.TrimPrefix(g, p))
		}
		gamma, err := strconv.ParseFloat(g, 64)
		if err != nil {
			return Transition{}, fmt.Errorf("bad gamma %q", args[1])
		}
		return Transition{Kind: TransHealth, PerSec: r, Gamma: gamma}, nil
	}
	return Transition{}, fmt.Errorf("unknown transition law %q (want a rate, health_rate, fixed or on_command)", name)
}

// ParseMultiplier parses "x40", "x0.6" or a plain number.
func ParseMultiplier(s string) (float64, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "x"), "×")
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("bad multiplier %q, want something like x40", s)
	}
	return v, nil
}

// ParseDist parses a unitless distribution: a number, uniform(a, b),
// normal(mean, sd), lognormal(mu, sigma) or exp(mean).
func ParseDist(s string) (dist.Dist, error) {
	return parseDist(s, func(a string) (float64, error) { return strconv.ParseFloat(a, 64) })
}

// ParseDurationDist parses a distribution of durations, in seconds:
// "20m", "exp(30m)", "uniform(10m, 40m)".
func ParseDurationDist(s string) (dist.Dist, error) {
	return parseDist(s, Seconds)
}

func parseDist(s string, num func(string) (float64, error)) (dist.Dist, error) {
	name, args, isCall := parseCall(s)
	if !isCall {
		v, err := num(strings.TrimSpace(s))
		if err != nil {
			return dist.Dist{}, fmt.Errorf("bad value %q", s)
		}
		return dist.Dist{Kind: dist.Const, A: v}, nil
	}
	vals := make([]float64, len(args))
	for i, a := range args {
		v, err := num(a)
		if err != nil {
			return dist.Dist{}, fmt.Errorf("%s(): bad argument %q", name, a)
		}
		vals[i] = v
	}
	want := map[string]int{"uniform": 2, "normal": 2, "lognormal": 2, "exp": 1}
	n, known := want[name]
	if !known {
		return dist.Dist{}, fmt.Errorf("unknown distribution %q (want uniform, normal, lognormal or exp)", name)
	}
	if len(vals) != n {
		return dist.Dist{}, fmt.Errorf("%s() takes %d arguments", name, n)
	}
	d := dist.Dist{Kind: dist.Kind(name), A: vals[0]}
	if n == 2 {
		d.B = vals[1]
	}
	if (name == "normal" || name == "lognormal") && d.B < 0 {
		return dist.Dist{}, fmt.Errorf("%s(): spread must not be negative", name)
	}
	if name == "uniform" && d.B < d.A {
		return dist.Dist{}, fmt.Errorf("uniform(%g, %g): upper bound below lower", d.A, d.B)
	}
	return d, nil
}

// ParseTimePoint parses a time within the run as an offset from its start:
// "day2 09:15" (day 1 is the first), "09:15", or a duration like "26h".
func ParseTimePoint(s string) (time.Duration, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	day := 1
	hadDay := false
	if strings.HasPrefix(t, "day") {
		hadDay = true
		rest := strings.TrimPrefix(t, "day")
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		n, err := strconv.Atoi(rest[:i])
		if err != nil || n < 1 {
			return 0, fmt.Errorf("bad time %q: want dayN with N >= 1", s)
		}
		day = n
		t = strings.TrimSpace(rest[i:])
	}
	base := time.Duration(day-1) * 24 * time.Hour
	switch {
	case t == "":
		return base, nil
	case strings.Contains(t, ":"):
		parts := strings.Split(t, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return 0, fmt.Errorf("bad clock time in %q", s)
		}
		var hms [3]int
		for i, p := range parts {
			v, err := strconv.Atoi(p)
			if err != nil || v < 0 {
				return 0, fmt.Errorf("bad clock time in %q", s)
			}
			hms[i] = v
		}
		if hms[0] > 23 || hms[1] > 59 || hms[2] > 59 {
			return 0, fmt.Errorf("clock time out of range in %q", s)
		}
		return base + time.Duration(hms[0])*time.Hour + time.Duration(hms[1])*time.Minute + time.Duration(hms[2])*time.Second, nil
	case hadDay:
		return 0, fmt.Errorf("bad time %q: want dayN HH:MM", s)
	default:
		return ParseDuration(t)
	}
}

// Target selects devices: "site[17]/pos_terminal[*]", "pos_terminal[3]",
// "site[*]/pos_terminal[1]". Indexes are 1-based; * means all.
type Target struct {
	Site  int // -1 = any site
	Class string
	Index int // -1 = any device; 1-based within the site (or class)
}

var targetRe = regexp.MustCompile(`^(?:site\[(\d+|\*)\]/)?([A-Za-z_][A-Za-z0-9_-]*)\[(\d+|\*)\]$`)

// ParseTarget parses a device selector.
func ParseTarget(s string) (Target, error) {
	m := targetRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Target{}, fmt.Errorf("bad target %q, want like site[17]/pos_terminal[*]", s)
	}
	t := Target{Site: -1, Class: m[2], Index: -1}
	if m[1] != "" && m[1] != "*" {
		t.Site, _ = strconv.Atoi(m[1])
	}
	if m[3] != "*" {
		t.Index, _ = strconv.Atoi(m[3])
	}
	return t, nil
}
