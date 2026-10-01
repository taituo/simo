// Package record defines the fixed-size event record the engine emits.
//
// The engine never builds strings. It writes 64-byte records, and the
// renderer turns them into log lines only when something reads them.
package record

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
)

// Size is the encoded size of a record in bytes.
const Size = 64

// Flags for pipeline artefacts.
const (
	FlagLate      = 1 << 0
	FlagDuplicate = 1 << 1
)

// Record is one event. Its layout matches the planned GPU buffer.
type Record struct {
	TS       int64     // true simulated time, ns since the world's start
	Device   uint32    // device index
	Template uint16    // global event template id
	Level    uint8     // severity
	Flags    uint8     // pipeline artefacts
	Trace    uint64    // trace or correlation id, 0 if none
	Span     uint32    // span id
	Parent   uint32    // parent span id
	Slots    [5]uint32 // raw random draws, or state values, for the template's slots
	Cause    uint32    // ground-truth fault id, 0 if background; hidden from agents
	Skew     int32     // device clock offset in ms; device time = TS + Skew
	_        uint32    // padding to 64 bytes
}

// Encode writes r into b, which must be at least Size bytes, little-endian.
func (r *Record) Encode(b []byte) {
	le := binary.LittleEndian
	le.PutUint64(b[0:], uint64(r.TS))
	le.PutUint32(b[8:], r.Device)
	le.PutUint16(b[12:], r.Template)
	b[14] = r.Level
	b[15] = r.Flags
	le.PutUint64(b[16:], r.Trace)
	le.PutUint32(b[24:], r.Span)
	le.PutUint32(b[28:], r.Parent)
	for i, s := range r.Slots {
		le.PutUint32(b[32+4*i:], s)
	}
	le.PutUint32(b[52:], r.Cause)
	le.PutUint32(b[56:], uint32(r.Skew))
	le.PutUint32(b[60:], 0)
}

// Decode reads a record from b.
func Decode(b []byte) Record {
	le := binary.LittleEndian
	r := Record{
		TS:       int64(le.Uint64(b[0:])),
		Device:   le.Uint32(b[8:]),
		Template: le.Uint16(b[12:]),
		Level:    b[14],
		Flags:    b[15],
		Trace:    le.Uint64(b[16:]),
		Span:     le.Uint32(b[24:]),
		Parent:   le.Uint32(b[28:]),
		Cause:    le.Uint32(b[52:]),
		Skew:     int32(le.Uint32(b[56:])),
	}
	for i := range r.Slots {
		r.Slots[i] = le.Uint32(b[32+4*i:])
	}
	return r
}

// Writer writes encoded records.
type Writer struct {
	w   *bufio.Writer
	buf [Size]byte
	n   int64
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 1<<20)}
}

// Write appends records.
func (w *Writer) Write(recs []Record) error {
	for i := range recs {
		recs[i].Encode(w.buf[:])
		if _, err := w.w.Write(w.buf[:]); err != nil {
			return err
		}
	}
	w.n += int64(len(recs))
	return nil
}

// Count returns the number of records written.
func (w *Writer) Count() int64 { return w.n }

// Flush flushes buffered data.
func (w *Writer) Flush() error { return w.w.Flush() }

// Reader reads encoded records.
type Reader struct {
	r   *bufio.Reader
	buf [Size]byte
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 1<<20)}
}

// Next returns the next record, or io.EOF.
func (r *Reader) Next() (Record, error) {
	if _, err := io.ReadFull(r.r, r.buf[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Record{}, errors.New("record stream truncated")
		}
		return Record{}, err
	}
	return Decode(r.buf[:]), nil
}
