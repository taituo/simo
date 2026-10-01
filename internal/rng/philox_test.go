package rng

import "testing"

// Known-answer vectors from Random123's tests/kat_vectors (philox4x32 10).
func TestPhiloxKnownAnswers(t *testing.T) {
	cases := []struct {
		c    Counter
		k    Key
		want [4]uint32
	}{
		{Counter{0, 0, 0, 0}, Key{0, 0}, [4]uint32{0x6627e8d5, 0xe169c58d, 0xbc57ac4c, 0x9b00dbd8}},
		{Counter{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}, Key{0xffffffff, 0xffffffff}, [4]uint32{0x408f276d, 0x41c83b0e, 0xa20bc7c6, 0x6d5451fd}},
		{Counter{0x243f6a88, 0x85a308d3, 0x13198a2e, 0x03707344}, Key{0xa4093822, 0x299f31d0}, [4]uint32{0xd16cfe09, 0x94fdcceb, 0x5001e420, 0x24126ea1}},
	}
	for i, c := range cases {
		if got := Philox4x32(c.c, c.k); got != c.want {
			t.Errorf("vector %d: got %08x, want %08x", i, got, c.want)
		}
	}
}

func TestUnitIsOpenInterval(t *testing.T) {
	if u := Unit(0); u <= 0 {
		t.Errorf("Unit(0) = %v, want > 0", u)
	}
	if u := Unit(0xffffffff); u >= 1 {
		t.Errorf("Unit(max) = %v, want < 1", u)
	}
}

func TestSeqMatchesDraw(t *testing.T) {
	k := EntityKey(42, 7)
	s := NewSeq(k, 3, 99, 0)
	a := Draw(k, 3, 99, 0)
	b := Draw(k, 3, 99, 1)
	want := append(a[:], b[:]...)
	for i, w := range want {
		if got := s.Uint32(); got != w {
			t.Fatalf("word %d: got %08x, want %08x", i, got, w)
		}
	}
}

func TestEntityKeysDiffer(t *testing.T) {
	seen := map[Key]bool{}
	for e := uint64(0); e < 10000; e++ {
		k := EntityKey(1, e)
		if seen[k] {
			t.Fatalf("duplicate key for entity %d", e)
		}
		seen[k] = true
	}
}
