package dist

import (
	"math"
	"testing"

	"github.com/taituo/simo/internal/rng"
)

func TestPoissonMeanAndVariance(t *testing.T) {
	for _, mean := range []float64{0.01, 0.5, 3, 9.9, 10, 42, 500} {
		const n = 200000
		k := rng.EntityKey(7, uint64(mean*1000))
		var sum, sumSq float64
		for i := int64(0); i < n; i++ {
			s := rng.NewSeq(k, 1, i, 0)
			x := float64(Poisson(mean, &s))
			sum += x
			sumSq += x * x
		}
		m := sum / n
		v := sumSq/n - m*m
		se := math.Sqrt(mean / n)
		if math.Abs(m-mean) > 5*se {
			t.Errorf("mean %v: sample mean %v (5 SE = %v)", mean, m, 5*se)
		}
		if math.Abs(v-mean)/mean > 0.05 {
			t.Errorf("mean %v: sample variance %v", mean, v)
		}
	}
}

func TestNormalInvQuantiles(t *testing.T) {
	cases := map[float64]float64{0.5: 0, 0.975: 1.959963985, 0.025: -1.959963985, 0.8413447461: 1, 1e-6: -4.753424309}
	for u, want := range cases {
		if got := NormalInv(u); math.Abs(got-want) > 1e-6 {
			t.Errorf("NormalInv(%v) = %v, want %v", u, got, want)
		}
	}
}

func TestUniformIntIsInclusive(t *testing.T) {
	d := Dist{Kind: Uniform, A: 4, B: 12}
	seen := map[int]bool{}
	for i := uint32(0); i < 100000; i++ {
		v := d.SampleInt(rng.Unit(i * 42949))
		if v < 4 || v > 12 {
			t.Fatalf("value %d outside [4, 12]", v)
		}
		seen[v] = true
	}
	if len(seen) != 9 {
		t.Errorf("saw %d distinct values, want 9", len(seen))
	}
}

func TestZipfFavoursLowRanks(t *testing.T) {
	z := NewZipf(10, 1.1)
	counts := make([]int, 10)
	for i := uint32(0); i < 100000; i++ {
		counts[z.Pick(rng.Unit(i*42949))]++
	}
	for i := 1; i < 10; i++ {
		if counts[i] > counts[i-1] {
			t.Errorf("rank %d (%d) more common than rank %d (%d)", i, counts[i], i-1, counts[i-1])
		}
	}
}
