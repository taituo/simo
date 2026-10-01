// Package rng implements the counter-based random numbers simo is built on.
//
// Every random number is computed, not drawn from a running generator:
//
//	words = Philox4x32-10(key, counter)
//
// The key identifies an entity (usually a device) and the counter identifies
// what is being drawn: (stream, tick, index). Any draw can therefore be
// computed on its own, in any order, on any thread, which is what makes the
// engine reproducible, parallel and randomly addressable.
//
// Philox is from Salmon et al., "Parallel Random Numbers: As Easy as 1, 2, 3"
// (SC 2011). The implementation is checked against the Random123 known-answer
// vectors in philox_test.go.
package rng

// Philox4x32 round and key-schedule constants.
const (
	philoxM0 = 0xD2511F53
	philoxM1 = 0xCD9E8D57
	philoxW0 = 0x9E3779B9 // golden ratio
	philoxW1 = 0xBB67AE85 // sqrt(3) - 1
)

// Key is a Philox4x32 key.
type Key [2]uint32

// Counter is a Philox4x32 counter.
type Counter [4]uint32

// Philox4x32 returns four random words for counter c under key k, using the
// standard 10 rounds.
func Philox4x32(c Counter, k Key) [4]uint32 {
	c0, c1, c2, c3 := c[0], c[1], c[2], c[3]
	k0, k1 := k[0], k[1]
	for i := 0; i < 10; i++ {
		if i > 0 {
			k0 += philoxW0
			k1 += philoxW1
		}
		p0 := uint64(philoxM0) * uint64(c0)
		p1 := uint64(philoxM1) * uint64(c2)
		c0, c1, c2, c3 = uint32(p1>>32)^c1^k0, uint32(p1), uint32(p0>>32)^c3^k1, uint32(p0)
	}
	return [4]uint32{c0, c1, c2, c3}
}

// SplitMix64 is a fast 64-bit mixing function, used to derive keys and to
// stretch one 32-bit draw into more bits when rendering.
func SplitMix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// EntityKey derives the Philox key of one entity (a device, a site, a fleet
// entry) from the world's master seed.
func EntityKey(master, entity uint64) Key {
	h := SplitMix64(master ^ SplitMix64(entity+1))
	return Key{uint32(h), uint32(h >> 32)}
}

// Draw returns four random words for (stream, tick, index) under key k.
func Draw(k Key, stream uint32, tick int64, idx uint32) [4]uint32 {
	t := uint64(tick)
	return Philox4x32(Counter{stream, uint32(t), uint32(t >> 32), idx}, k)
}

// Unit maps a 32-bit word to the open interval (0, 1). It never returns 0 or
// 1, so it is safe to pass to logarithms and inverse CDFs.
func Unit(u uint32) float64 {
	return (float64(u) + 0.5) * (1.0 / 4294967296.0)
}

// Seq hands out a sequence of words from one (key, stream, tick), for
// samplers that need a variable number of uniforms. It is a value type so it
// can live on the stack.
type Seq struct {
	key    Key
	stream uint32
	tick   int64
	idx    uint32
	buf    [4]uint32
	left   int
}

// NewSeq starts a sequence at counter index idx.
func NewSeq(k Key, stream uint32, tick int64, idx uint32) Seq {
	return Seq{key: k, stream: stream, tick: tick, idx: idx}
}

// Uint32 returns the next word.
func (s *Seq) Uint32() uint32 {
	if s.left == 0 {
		s.buf = Draw(s.key, s.stream, s.tick, s.idx)
		s.idx++
		s.left = 4
	}
	v := s.buf[4-s.left]
	s.left--
	return v
}

// Float64 returns the next uniform in (0, 1).
func (s *Seq) Float64() float64 {
	return Unit(s.Uint32())
}
