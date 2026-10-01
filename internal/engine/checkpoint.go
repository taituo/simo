package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/taituo/simo/internal/ir"
)

// Checkpoint is the complete dynamic state of every device at the start of
// a tick, before that tick is stepped. Everything else the engine needs is a
// pure function of the compiled world and the tick, so a run resumed from a
// checkpoint produces exactly the output a run from tick 0 would.
type Checkpoint struct {
	Tick       int64
	Engine     string // ir.EngineVersion that wrote it
	SeedSHA256 string // world it belongs to

	State    []uint16
	Deadline []int64
	PendTick []int64
	Health   []float64
	Metric   []float64
	Seq      []uint32
}

func (s *sim) snapshot(tick int64) *Checkpoint {
	return &Checkpoint{
		Tick:       tick,
		Engine:     ir.EngineVersion,
		SeedSHA256: s.w.SeedSHA256,
		State:      append([]uint16(nil), s.state...),
		Deadline:   append([]int64(nil), s.deadline...),
		PendTick:   append([]int64(nil), s.pendTick...),
		Health:     append([]float64(nil), s.health...),
		Metric:     append([]float64(nil), s.metric...),
		Seq:        append([]uint32(nil), s.seq...),
	}
}

func (s *sim) load(cp *Checkpoint) error {
	switch {
	case cp.Engine != ir.EngineVersion:
		return fmt.Errorf("checkpoint was written by engine %s, this is %s", cp.Engine, ir.EngineVersion)
	case cp.SeedSHA256 != s.w.SeedSHA256:
		return errors.New("checkpoint belongs to a different seed")
	case len(cp.State) != len(s.state) || len(cp.Deadline) != len(s.deadline) || len(cp.PendTick) != len(s.pendTick) ||
		len(cp.Health) != len(s.health) || len(cp.Metric) != len(s.metric) || len(cp.Seq) != len(s.seq):
		return errors.New("checkpoint does not match the world's devices")
	case cp.Tick < 0 || cp.Tick > s.w.Ticks:
		return fmt.Errorf("checkpoint tick %d is outside the run", cp.Tick)
	}
	copy(s.state, cp.State)
	copy(s.deadline, cp.Deadline)
	copy(s.pendTick, cp.PendTick)
	copy(s.health, cp.Health)
	copy(s.metric, cp.Metric)
	copy(s.seq, cp.Seq)
	return nil
}

const checkpointMagic = "SIMOCKP1"

// MarshalBinary encodes the checkpoint: a magic header, then gzip of
// little-endian fields.
func (cp *Checkpoint) MarshalBinary() ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(checkpointMagic)
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	le := binary.LittleEndian
	for _, s := range []string{cp.Engine, cp.SeedSHA256} {
		if err := binary.Write(zw, le, uint16(len(s))); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(zw, s); err != nil {
			return nil, err
		}
	}
	header := []any{cp.Tick, uint32(len(cp.State)), uint32(len(cp.Metric)), uint32(len(cp.Seq))}
	for _, v := range append(header, cp.State, cp.Deadline, cp.PendTick, cp.Health, cp.Metric, cp.Seq) {
		if err := binary.Write(zw, le, v); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// UnmarshalCheckpoint decodes a checkpoint written by MarshalBinary.
func UnmarshalCheckpoint(b []byte) (*Checkpoint, error) {
	if !bytes.HasPrefix(b, []byte(checkpointMagic)) {
		return nil, errors.New("not a simo checkpoint")
	}
	zr, err := gzip.NewReader(bytes.NewReader(b[len(checkpointMagic):]))
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	var strs [2]string
	for i := range strs {
		var n uint16
		if err := binary.Read(zr, le, &n); err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(zr, buf); err != nil {
			return nil, err
		}
		strs[i] = string(buf)
	}
	cp := &Checkpoint{Engine: strs[0], SeedSHA256: strs[1]}
	var n, nm, ns uint32
	for _, v := range []any{&cp.Tick, &n, &nm, &ns} {
		if err := binary.Read(zr, le, v); err != nil {
			return nil, err
		}
	}
	const limit = 1 << 30
	if n > limit || nm > limit || ns > limit {
		return nil, errors.New("checkpoint is corrupt")
	}
	cp.State = make([]uint16, n)
	cp.Deadline = make([]int64, n)
	cp.PendTick = make([]int64, n)
	cp.Health = make([]float64, n)
	cp.Metric = make([]float64, nm)
	cp.Seq = make([]uint32, ns)
	for _, v := range []any{cp.State, cp.Deadline, cp.PendTick, cp.Health, cp.Metric, cp.Seq} {
		if err := binary.Read(zr, le, v); err != nil {
			return nil, fmt.Errorf("checkpoint is truncated: %w", err)
		}
	}
	return cp, nil
}
