package spec

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExamplesAreValid(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if len(paths) == 0 {
		t.Fatal("no examples found")
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, is := range Validate(s) {
			t.Errorf("%s: %s", filepath.Base(p), is)
		}
	}
}

const base = `
simo: 1
name: t
clock: { start: 2026-10-05T00:00:00Z, duration: 1d, tick: 1s }
classes:
  c:
    states:
      ok:   { to: { bad: 1/1h } }
      bad:  { to: { ok: 1/10m } }
    events:
      - { id: e, level: INFO, text: "hello {n:int(1, 9)}", rate: 1/s }
fleet:
  - { class: c, count: 3 }
`

func issuesFor(t *testing.T, src string) Issues {
	t.Helper()
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return Validate(s)
}

func TestBaseIsClean(t *testing.T) {
	if is := issuesFor(t, base); len(is) != 0 {
		t.Fatalf("unexpected issues: %v", is)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		old, new, wantPath, wantMsg string
	}{
		{"bad: 1/1h", "gone: 1/1h", "classes.c.states.ok.to.gone", "unknown target state"},
		{"bad: 1/1h", "bad: 1/1parsec", "classes.c.states.ok.to.bad", "unknown unit"},
		{"bad: 1/1h", "bad: soon", "classes.c.states.ok.to.bad", "needs a time unit"},
		{"rate: 1/s", "rate: 1/s * nightly", "classes.c.events[0].rate", "unknown pattern"},
		{"level: INFO", "level: LOUD", "classes.c.events[0].level", "unknown level"},
		{"int(1, 9)", "nope(1)", "classes.c.events[0].text", "unknown generator"},
		{"int(1, 9)", "vocab", "classes.c.events[0].text", "unknown vocab"},
		{"tick: 1s", "tick: 0s", "clock.tick", "positive duration"},
		{"class: c,", "class: d,", "fleet[0].class", "unknown class"},
		{"simo: 1", "simo: 2", "simo", "not supported"},
	}
	for _, c := range cases {
		src := strings.Replace(base, c.old, c.new, 1)
		found := false
		for _, is := range issuesFor(t, src).Errors() {
			if is.Path == c.wantPath && strings.Contains(is.Msg, c.wantMsg) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q -> %q: want error at %s containing %q, got %v", c.old, c.new, c.wantPath, c.wantMsg, issuesFor(t, src))
		}
	}
}

func TestLintWarnings(t *testing.T) {
	// A state only a command can leave, with no responder: devices get stuck.
	src := strings.Replace(base, "bad:  { to: { ok: 1/10m } }", "bad:  {}", 1)
	src = strings.Replace(src, "    events:", "    commands:\n      fix: { from: [bad], to: ok }\n    events:", 1)
	assertWarning(t, src, "classes.c.states.bad", "only a command can leave")

	// Unreachable state.
	src = strings.Replace(base, "bad:  { to: { ok: 1/10m } }", "bad:  { to: { ok: 1/10m } }\n      lost: {}", 1)
	assertWarning(t, src, "classes.c.states.lost", "unreachable")

	// Huge volume.
	src = strings.Replace(base, "count: 3", "count: 5000", 1)
	assertWarning(t, src, "fleet", "lines per simulated day")
}

func assertWarning(t *testing.T, src, path, msg string) {
	t.Helper()
	for _, is := range issuesFor(t, src) {
		if is.Severity == Warning && is.Path == path && strings.Contains(is.Msg, msg) {
			return
		}
	}
	t.Errorf("want warning at %s containing %q, got %v", path, msg, issuesFor(t, src))
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	if _, err := Parse([]byte(strings.Replace(base, "fleet:", "fleeet:", 1))); err == nil {
		t.Error("typo in a top-level key was accepted")
	}
	if _, err := Parse([]byte(strings.Replace(base, "events:", "evnets:", 1))); err == nil {
		t.Error("typo in a class key was accepted")
	}
}

func TestParsers(t *testing.T) {
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-12*math.Max(1, math.Abs(b)) }
	for s, want := range map[string]float64{"0.03/s": 0.03, "1/2h": 1.0 / 7200, "-0.05/day": -0.05 / 86400, "6/m": 0.1} {
		if got, err := ParseRate(s); err != nil || !near(got, want) {
			t.Errorf("ParseRate(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	r, err := ParseRateExpr("0.03/s * store_hours * 2 * weekly")
	if err != nil || !near(r.PerSec, 0.06) || strings.Join(r.Patterns, ",") != "store_hours,weekly" {
		t.Errorf("ParseRateExpr = %+v, %v", r, err)
	}
	for s, want := range map[string]time.Duration{"day2 09:15": 33*time.Hour + 15*time.Minute, "09:15": 9*time.Hour + 15*time.Minute, "26h": 26 * time.Hour, "day1": 0} {
		if got, err := ParseTimePoint(s); err != nil || got != want {
			t.Errorf("ParseTimePoint(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	tr, err := ParseTransition("health_rate(0.02/day, gamma: 4)")
	if err != nil || tr.Kind != TransHealth || tr.Gamma != 4 || !near(tr.PerSec, 0.02/86400) {
		t.Errorf("ParseTransition(health_rate) = %+v, %v", tr, err)
	}
	if tr, err := ParseTransition("fixed(90s)"); err != nil || tr.Kind != TransFixed || tr.Fixed != 90*time.Second {
		t.Errorf("ParseTransition(fixed) = %+v, %v", tr, err)
	}
	tg, err := ParseTarget("site[17]/pos_terminal[*]")
	if err != nil || tg != (Target{Site: 17, Class: "pos_terminal", Index: -1}) {
		t.Errorf("ParseTarget = %+v, %v", tg, err)
	}
	d, err := ParseDurationDist("exp(30m)")
	if err != nil || d.Kind != "exp" || d.A != 1800 {
		t.Errorf("ParseDurationDist = %+v, %v", d, err)
	}
}
