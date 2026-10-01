package render

import (
	"encoding/json"
	"math"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/rng"
	"github.com/taituo/simo/internal/spec"
)

const seed = `
simo: 1
name: r
clock: { start: 2026-10-05T00:00:00Z, duration: 1h, tick: 1s }
vocab:
  user: { words: [ann, bob] }
classes:
  web:
    states:
      up: {}
    metrics:
      temp: { ou: { mean: 40, theta: 1/10m, sigma: 1 } }
    events:
      - id: req
        level: WARN
        text: 'GET /x "{user:vocab}" {ip:ip(192.0.2.0/24)} {ms:lognormal(5, 0.5)|%d}ms id={id:hex(20)} t={t:metric(temp)|%.1f}'
        rate: 1/s
fleet:
  - { class: web, count: 1, name: web-1 }
`

func newRenderer(t *testing.T) *Renderer {
	t.Helper()
	s, err := spec.Parse([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	w, err := compile.Compile(s, []byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(w)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var lineRe = regexp.MustCompile(`^GET /x "(ann|bob)" (\S+) (\d+)ms id=([0-9a-f]{20}) t=41\.5$`)

func TestGeneratorsStayInRange(t *testing.T) {
	r := newRenderer(t)
	prefix := netip.MustParsePrefix("192.0.2.0/24")
	for i := uint32(0); i < 2000; i++ {
		d := rng.Draw(rng.Key{1, 2}, 0, int64(i), 0)
		rec := record.Record{Slots: [5]uint32{d[0], d[1], d[2], d[3], math.Float32bits(41.5)}}
		msg := string(r.Message(nil, &rec))
		m := lineRe.FindStringSubmatch(msg)
		if m == nil {
			t.Fatalf("message %q does not match %s", msg, lineRe)
		}
		ip := netip.MustParseAddr(m[2])
		if !prefix.Contains(ip) || ip.As4()[3] == 0 || ip.As4()[3] == 255 {
			t.Fatalf("ip %s outside the usable range of %s", ip, prefix)
		}
		if ms, _ := strconv.Atoi(m[3]); ms <= 0 {
			t.Fatalf("latency %d not positive", ms)
		}
	}
}

func TestFormats(t *testing.T) {
	r := newRenderer(t)
	rec := record.Record{TS: 1500 * 1e6, Level: 3, Slots: [5]uint32{0, 1, 2, 3, math.Float32bits(41.5)}, Cause: 7}

	text := string(r.Line(nil, &rec, Text))
	if !strings.HasPrefix(text, "2026-10-05T00:00:01.500Z web-1 WARN GET /x ") {
		t.Errorf("text line: %q", text)
	}
	sys := string(r.Line(nil, &rec, Syslog))
	if !strings.HasPrefix(sys, "<132>1 2026-10-05T00:00:01.500Z web-1 web - req - GET /x ") {
		t.Errorf("syslog line: %q", sys)
	}
	for _, show := range []bool{false, true} {
		r.ShowCause = show
		var obj map[string]any
		line := r.Line(nil, &rec, JSON)
		if err := json.Unmarshal(line, &obj); err != nil {
			t.Fatalf("invalid JSON %s: %v", line, err)
		}
		if obj["event"] != "req" || obj["host"] != "web-1" || !strings.Contains(obj["msg"].(string), `"`) {
			t.Errorf("json line: %s", line)
		}
		if _, has := obj["cause"]; has != show {
			t.Errorf("ShowCause=%v but cause present=%v", show, has)
		}
	}
}

func TestAppendNum(t *testing.T) {
	cases := []struct {
		v      float64
		format string
		int    bool
		want   string
	}{
		{3.14159, "", false, "3.14"},
		{3.6, "", true, "4"},
		{3.14159, "%.3f", false, "3.142"},
		{255, "%x", true, "ff"},
		{42, "%5d", true, "   42"},
		{0.5, "%s", false, "0.5"},
	}
	for _, c := range cases {
		if got := string(appendNum(nil, c.v, c.format, c.int)); got != c.want {
			t.Errorf("appendNum(%v, %q) = %q, want %q", c.v, c.format, got, c.want)
		}
	}
}
