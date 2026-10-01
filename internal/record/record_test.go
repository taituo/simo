package record

import (
	"bytes"
	"io"
	"testing"
	"unsafe"
)

func TestSizeMatchesLayout(t *testing.T) {
	if got := unsafe.Sizeof(Record{}); got != Size {
		t.Fatalf("Record is %d bytes in memory, want %d", got, Size)
	}
}

func TestRoundTrip(t *testing.T) {
	in := []Record{
		{TS: 123456789, Device: 7, Template: 3, Level: 4, Flags: FlagLate, Trace: 1 << 40, Span: 9, Parent: 8, Slots: [5]uint32{1, 2, 3, 4, 5}, Cause: 2, Skew: -1500},
		{TS: -1, Device: 1<<32 - 1},
	}
	var b bytes.Buffer
	w := NewWriter(&b)
	if err := w.Write(in); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if b.Len() != len(in)*Size {
		t.Fatalf("wrote %d bytes, want %d", b.Len(), len(in)*Size)
	}
	r := NewReader(&b)
	for i, want := range in {
		got, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("record %d: got %+v, want %+v", i, got, want)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("after last record: err = %v, want io.EOF", err)
	}
}
