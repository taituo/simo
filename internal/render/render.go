// Package render turns records into log lines. Templates are compiled once
// into literal parts and slot functions; rendering a line appends to a byte
// slice without per-line allocation.
//
// Random slots hold raw 32-bit draws; the slot's generator shapes them here
// (inverse CDFs, Zipf lookups, hex encoding), so the engine stays pure
// integer work and every backend renders the same text.
package render

import (
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/taituo/simo/internal/dist"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/record"
	"github.com/taituo/simo/internal/rng"
	"github.com/taituo/simo/internal/spec"
	"github.com/taituo/simo/internal/tmpl"
)

// Format is an output format.
type Format int

// Output formats.
const (
	Text   Format = iota // 2026-10-05T14:03:22.123Z host LEVEL message
	JSON                 // one JSON object per line
	Syslog               // RFC 5424
)

// ParseFormat parses a format name.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(s) {
	case "text", "plain", "":
		return Text, nil
	case "json", "jsonl":
		return JSON, nil
	case "syslog", "rfc5424":
		return Syslog, nil
	}
	return 0, fmt.Errorf("unknown format %q (want text, jsonl or syslog)", s)
}

// syslogSeverity maps simo levels (DEBUG..CRIT) to RFC 5424 severities.
var syslogSeverity = []int{7, 6, 5, 4, 3, 2}

// facilityLocal0 is the syslog facility used for all lines.
const facilityLocal0 = 16

type slotFunc func(dst []byte, v uint32) []byte

// Renderer renders records of one world.
type Renderer struct {
	w       *ir.World
	slots   [][]slotFunc
	startNs int64
	// ShowCause adds the ground-truth cause to JSON lines. Keep it off for
	// anything an agent under test will read.
	ShowCause bool
}

// New compiles the world's templates.
func New(w *ir.World) (*Renderer, error) {
	r := &Renderer{w: w, startNs: w.Start.UnixNano()}
	for _, ev := range w.Events {
		cls := &w.Classes[ev.Class]
		var fns []slotFunc
		for _, sl := range ev.Tmpl.Slots {
			f, err := compileSlot(w, cls, sl)
			if err != nil {
				return nil, fmt.Errorf("event %s: {%s}: %w", ev.ID, sl.Name, err)
			}
			fns = append(fns, f)
		}
		r.slots = append(r.slots, fns)
	}
	return r, nil
}

// Message appends the rendered message of rec to dst.
func (r *Renderer) Message(dst []byte, rec *record.Record) []byte {
	t := r.w.Events[rec.Template].Tmpl
	dst = append(dst, t.Parts[0]...)
	for i, f := range r.slots[rec.Template] {
		dst = f(dst, rec.Slots[i])
		dst = append(dst, t.Parts[i+1]...)
	}
	return dst
}

// Time returns the device time of rec (true time plus clock skew).
func (r *Renderer) Time(rec *record.Record) time.Time {
	return time.Unix(0, r.startNs+rec.TS+int64(rec.Skew)*int64(time.Millisecond)).UTC()
}

const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// Line appends one full line (without newline) in format f.
func (r *Renderer) Line(dst []byte, rec *record.Record, f Format) []byte {
	ev := &r.w.Events[rec.Template]
	dev := &r.w.Devices[rec.Device]
	ts := r.Time(rec)
	switch f {
	case JSON:
		dst = append(dst, `{"ts":"`...)
		dst = ts.AppendFormat(dst, tsLayout)
		dst = append(dst, `","host":`...)
		dst = appendJSONString(dst, dev.Name)
		dst = append(dst, `,"class":`...)
		dst = appendJSONString(dst, r.w.Classes[dev.Class].Name)
		dst = append(dst, `,"level":"`...)
		dst = append(dst, spec.Levels[rec.Level]...)
		dst = append(dst, `","event":"`...)
		dst = append(dst, ev.ID...)
		dst = append(dst, `","msg":`...)
		start := len(dst)
		dst = r.Message(dst, rec)
		msg := string(dst[start:])
		dst = appendJSONString(dst[:start], msg)
		if r.ShowCause {
			dst = append(dst, `,"cause":`...)
			dst = strconv.AppendUint(dst, uint64(rec.Cause), 10)
		}
		dst = append(dst, '}')
	case Syslog:
		dst = append(dst, '<')
		dst = strconv.AppendInt(dst, int64(facilityLocal0*8+syslogSeverity[rec.Level]), 10)
		dst = append(dst, ">1 "...)
		dst = ts.AppendFormat(dst, tsLayout)
		dst = append(dst, ' ')
		dst = append(dst, dev.Name...)
		dst = append(dst, ' ')
		dst = append(dst, r.w.Classes[dev.Class].Name...)
		dst = append(dst, " - "...)
		dst = append(dst, ev.ID...)
		dst = append(dst, " - "...)
		dst = r.Message(dst, rec)
	default:
		dst = ts.AppendFormat(dst, tsLayout)
		dst = append(dst, ' ')
		dst = append(dst, dev.Name...)
		dst = append(dst, ' ')
		dst = append(dst, spec.Levels[rec.Level]...)
		dst = append(dst, ' ')
		dst = r.Message(dst, rec)
	}
	return dst
}

func appendJSONString(dst []byte, s string) []byte {
	const hexDigits = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"')
}

func compileSlot(w *ir.World, cls *ir.Class, sl tmpl.Slot) (slotFunc, error) {
	nums := make([]float64, len(sl.Args))
	for i, a := range sl.Args {
		nums[i], _ = strconv.ParseFloat(a, 64)
	}
	switch sl.Gen {
	case "hex":
		n := int(nums[0])
		return func(dst []byte, v uint32) []byte {
			h := uint64(v)
			for i := 0; i < n; i += 16 {
				h = rng.SplitMix64(h)
				var buf [16]byte
				x := h
				for j := 15; j >= 0; j-- {
					buf[j] = "0123456789abcdef"[x&0xF]
					x >>= 4
				}
				dst = append(dst, buf[:min(16, n-i)]...)
			}
			return dst
		}, nil
	case "int":
		lo, hi := math.Round(nums[0]), math.Round(nums[1])
		return func(dst []byte, v uint32) []byte {
			return appendNum(dst, lo+math.Floor(rng.Unit(v)*(hi-lo+1)), sl.Format, true)
		}, nil
	case "uniform", "normal", "lognormal", "exp":
		d := dist.Dist{Kind: dist.Kind(sl.Gen), A: nums[0]}
		if len(nums) > 1 {
			d.B = nums[1]
		}
		return func(dst []byte, v uint32) []byte {
			return appendNum(dst, d.Sample(rng.Unit(v)), sl.Format, false)
		}, nil
	case "choice":
		words := sl.Args
		return func(dst []byte, v uint32) []byte {
			return append(dst, words[int(rng.Unit(v)*float64(len(words)))]...)
		}, nil
	case "vocab":
		name := sl.Name
		if len(sl.Args) == 1 {
			name = sl.Args[0]
		}
		voc, ok := w.Vocab[name]
		if !ok {
			return nil, fmt.Errorf("unknown vocab %q", name)
		}
		return func(dst []byte, v uint32) []byte {
			return append(dst, voc.Words[voc.Zipf.Pick(rng.Unit(v))]...)
		}, nil
	case "ip":
		p, err := netip.ParsePrefix(sl.Args[0])
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("bad CIDR %q", sl.Args[0])
		}
		p = p.Masked()
		a4 := p.Addr().As4()
		base := uint32(a4[0])<<24 | uint32(a4[1])<<16 | uint32(a4[2])<<8 | uint32(a4[3])
		hostBits := 32 - p.Bits()
		return func(dst []byte, v uint32) []byte {
			addr := base
			switch {
			case hostBits >= 2:
				size := uint64(1) << hostBits
				addr = base + 1 + uint32(uint64(v)%(size-2)) // skip network and broadcast
			case hostBits == 1:
				addr = base + v&1
			}
			return netip.AddrFrom4([4]byte{byte(addr >> 24), byte(addr >> 16), byte(addr >> 8), byte(addr)}).AppendTo(dst)
		}, nil
	case "uuid":
		return func(dst []byte, v uint32) []byte {
			a := rng.SplitMix64(uint64(v))
			b := rng.SplitMix64(a)
			a = a&^0xF000 | 0x4000           // version 4
			b = b&^(0xC000<<48) | 0x8000<<48 // RFC 4122 variant
			return fmt.Appendf(dst, "%08x-%04x-%04x-%04x-%012x", a>>32, (a>>16)&0xFFFF, a&0xFFFF, b>>48, b&0xFFFFFFFFFFFF)
		}, nil
	case "metric":
		return func(dst []byte, v uint32) []byte {
			return appendNum(dst, float64(math.Float32frombits(v)), sl.Format, false)
		}, nil
	case "seq":
		return func(dst []byte, v uint32) []byte {
			return appendNum(dst, float64(v), sl.Format, true)
		}, nil
	}
	return nil, fmt.Errorf("unknown generator %q", sl.Gen)
}

// appendNum formats v. Without a format, integers print as %d and other
// numbers with two decimals. %d and %.Nf take a fast path.
func appendNum(dst []byte, v float64, format string, integer bool) []byte {
	switch {
	case format == "" && integer, format == "%d":
		return strconv.AppendInt(dst, int64(math.Round(v)), 10)
	case format == "":
		return strconv.AppendFloat(dst, v, 'f', 2, 64)
	case strings.HasPrefix(format, "%.") && strings.HasSuffix(format, "f"):
		if prec, err := strconv.Atoi(format[2 : len(format)-1]); err == nil {
			return strconv.AppendFloat(dst, v, 'f', prec, 64)
		}
	}
	switch format[len(format)-1] {
	case 'd', 'x', 'X', 'o', 'b', 'c':
		return fmt.Appendf(dst, format, int64(math.Round(v)))
	case 's', 'v':
		if integer {
			return strconv.AppendInt(dst, int64(math.Round(v)), 10)
		}
		return strconv.AppendFloat(dst, v, 'g', -1, 64)
	}
	return fmt.Appendf(dst, format, v)
}
