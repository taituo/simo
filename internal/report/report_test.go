package report

import (
	"testing"
	"time"

	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/spec"
)

func TestWindowParsing(t *testing.T) {
	src := []byte(`
simo: 1
name: w
clock: { start: 2026-10-05T00:00:00Z, duration: 2d, tick: 10s }
classes: { c: { states: { up: {} } } }
fleet: [ { class: c, count: 1 } ]
`)
	s, err := spec.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	w, err := compile.Compile(s, src)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		from, to, dur string
		want          [2]int64
	}{
		{"", "", "", [2]int64{0, 17280}},
		{"day2 09:15", "", "30m", [2]int64{11970, 12150}},
		{"2026-10-06T09:15:00Z", "2026-10-06T09:45:00Z", "", [2]int64{11970, 12150}},
		{"26h", "", "", [2]int64{9360, 17280}},
		{"day2 23:00", "", "5h", [2]int64{16920, 17280}}, // clipped at the end
		{"", "", "15s", [2]int64{0, 2}},                  // partial tick rounds up
	}
	for _, c := range cases {
		f, to, err := Window(w, c.from, c.to, c.dur)
		if err != nil || [2]int64{f, to} != c.want {
			t.Errorf("Window(%q, %q, %q) = [%d, %d) %v, want %v", c.from, c.to, c.dur, f, to, err, c.want)
		}
	}
	for _, bad := range [][3]string{{"day3 00:00", "", ""}, {"tomorrow", "", ""}, {"10:00", "09:00", ""}} {
		if _, _, err := Window(w, bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("Window(%q) accepted", bad)
		}
	}
	if d, err := ParseTime(w, "2026-10-05T00:01:30Z"); err != nil || d != 90*time.Second {
		t.Errorf("ParseTime RFC 3339 = %v, %v", d, err)
	}
}

func TestFormatting(t *testing.T) {
	if got := Commas(1234567); got != "1,234,567" {
		t.Errorf("Commas = %s", got)
	}
	if got := Plural(2, "class"); got != "2 classes" {
		t.Errorf("Plural = %s", got)
	}
	if got := Offset(33*time.Hour + 15*time.Minute); got != "day2 09:15:00" {
		t.Errorf("Offset = %s", got)
	}
	if got := Span(7 * 24 * time.Hour); got != "7d" {
		t.Errorf("Span = %s", got)
	}
}
