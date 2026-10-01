// Package tmpl parses log message templates such as
//
//	txn {txn:hex(16)} by {cashier:vocab} amount={eur:lognormal(3.0,0.9)|%.2f} EUR
//
// A placeholder is {name:generator(args)|format}. The generator and format are
// optional: {seq} uses the generator "seq". Write {{ for a literal brace.
// A template has at most MaxSlots placeholders, one per record slot.
package tmpl

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxSlots is the number of slots in a record.
const MaxSlots = 5

// Slot is one placeholder.
type Slot struct {
	Name   string
	Gen    string
	Args   []string
	Format string
}

// Template is a parsed message: literal parts interleaved with slots.
// Parts has len(Slots)+1 entries; the message is
// Parts[0] Slot[0] Parts[1] Slot[1] ... Parts[n].
type Template struct {
	Text  string
	Parts []string
	Slots []Slot
}

var placeholderRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?::([A-Za-z_][A-Za-z0-9_]*)(?:\((.*)\))?)?(?:\|(%[-+ #0-9.]*[a-zA-Z]))?$`)

// Parse parses template text.
func Parse(text string) (*Template, error) {
	t := &Template{Text: text}
	var lit strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '{' && i+1 < len(text) && text[i+1] == '{':
			lit.WriteByte('{')
			i++
		case c == '}' && i+1 < len(text) && text[i+1] == '}':
			lit.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(text[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unclosed placeholder at byte %d", i)
			}
			body := strings.TrimSpace(text[i+1 : i+end])
			m := placeholderRe.FindStringSubmatch(body)
			if m == nil {
				return nil, fmt.Errorf("bad placeholder {%s}: want {name:generator(args)|format}", body)
			}
			s := Slot{Name: m[1], Gen: m[2], Format: m[4]}
			if s.Gen == "" {
				s.Gen = s.Name
			}
			if strings.TrimSpace(m[3]) != "" {
				for _, a := range strings.Split(m[3], ",") {
					s.Args = append(s.Args, strings.TrimSpace(a))
				}
			}
			t.Parts = append(t.Parts, lit.String())
			lit.Reset()
			t.Slots = append(t.Slots, s)
			i += end
		case c == '}':
			return nil, fmt.Errorf("unmatched } at byte %d (write }} for a literal brace)", i)
		default:
			lit.WriteByte(c)
		}
	}
	t.Parts = append(t.Parts, lit.String())
	if len(t.Slots) > MaxSlots {
		return nil, fmt.Errorf("%d placeholders, at most %d are allowed", len(t.Slots), MaxSlots)
	}
	return t, nil
}

// GenInfo describes a slot generator.
type GenInfo struct {
	MinArgs, MaxArgs int  // MaxArgs < 0 = unlimited
	State            bool // value comes from device state, not a random draw
	Doc              string
}

// Generators lists the slot generators renderers implement.
var Generators = map[string]GenInfo{
	"hex":       {1, 1, false, "hex(n): n random hex characters"},
	"int":       {2, 2, false, "int(a, b): integer in [a, b]"},
	"uniform":   {2, 2, false, "uniform(a, b): number in [a, b)"},
	"normal":    {2, 2, false, "normal(mean, sd)"},
	"lognormal": {2, 2, false, "lognormal(mu, sigma): heavy-tailed, e.g. latencies"},
	"exp":       {1, 1, false, "exp(mean)"},
	"choice":    {1, -1, false, "choice(a, b, ...): one of the words"},
	"vocab":     {0, 1, false, "vocab or vocab(name): a word from a vocab list"},
	"ip":        {1, 1, false, "ip(cidr): an IPv4 address in the range"},
	"uuid":      {0, 0, false, "uuid: a random UUID"},
	"metric":    {1, 1, true, "metric(name): the device's current metric value"},
	"seq":       {0, 0, true, "seq: a per-device counter for this event"},
}
