package tmpl

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tp, err := Parse("txn {txn:hex(16)} by {cashier:vocab} amount={eur:lognormal(3.0, 0.9)|%.2f} EUR {{ok}} {seq}")
	if err != nil {
		t.Fatal(err)
	}
	wantParts := []string{"txn ", " by ", " amount=", " EUR {ok} ", ""}
	if !reflect.DeepEqual(tp.Parts, wantParts) {
		t.Errorf("parts = %q, want %q", tp.Parts, wantParts)
	}
	want := []Slot{
		{Name: "txn", Gen: "hex", Args: []string{"16"}},
		{Name: "cashier", Gen: "vocab"},
		{Name: "eur", Gen: "lognormal", Args: []string{"3.0", "0.9"}, Format: "%.2f"},
		{Name: "seq", Gen: "seq"},
	}
	if !reflect.DeepEqual(tp.Slots, want) {
		t.Errorf("slots = %+v, want %+v", tp.Slots, want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"a {b", "a } b", "{1x}", "{a}{b}{c}{d}{e}{f}", "{a:b(}"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", bad)
		}
	}
}
