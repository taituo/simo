// Package dist turns uniform draws into the distributions simo's models use.
//
// Every sampler takes its randomness explicitly (a uniform or an rng.Seq), so
// results depend only on the counter-based draws and are reproducible.
//
// Determinism note: these samplers use Go's math package. Results are
// bit-identical on the same architecture; the GPU phase replaces log/exp with
// shared polynomial versions so CPU and GPU agree (see docs/design.md,
// "Determinism").
package dist

import (
	"fmt"
	"math"

	"github.com/taituo/simo/internal/rng"
)

// Poisson samples a Poisson count with the given mean. Small means use
// inversion from one uniform; large means use Hörmann's PTRS transformed
// rejection, which needs about two uniforms per sample.
func Poisson(mean float64, s *rng.Seq) int {
	switch {
	case mean <= 0 || math.IsNaN(mean):
		return 0
	case mean < 10:
		return poissonInversion(mean, s.Float64())
	default:
		return poissonPTRS(mean, s)
	}
}

func poissonInversion(mean, u float64) int {
	p := math.Exp(-mean)
	if u <= p { // fast path: most ticks emit nothing
		return 0
	}
	cdf := p
	k := 0
	for u > cdf && k < 1000 {
		k++
		p *= mean / float64(k)
		cdf += p
	}
	return k
}

// poissonPTRS is the transformed rejection method with squeeze from
// W. Hörmann, "The transformed rejection method for generating Poisson
// random variables" (1993), as used by NumPy.
func poissonPTRS(mean float64, s *rng.Seq) int {
	slam := math.Sqrt(mean)
	loglam := math.Log(mean)
	b := 0.931 + 2.53*slam
	a := -0.059 + 0.02483*b
	invalpha := 1.1239 + 1.1328/(b-3.4)
	vr := 0.9277 - 3.6224/(b-2)
	for {
		u := s.Float64() - 0.5
		v := s.Float64()
		us := 0.5 - math.Abs(u)
		k := math.Floor((2*a/us+b)*u + mean + 0.43)
		if us >= 0.07 && v <= vr {
			return int(k)
		}
		if k < 0 || (us < 0.013 && v > us) {
			continue
		}
		lg, _ := math.Lgamma(k + 1)
		if math.Log(v)+math.Log(invalpha)-math.Log(a/(us*us)+b) <= -mean+k*loglam-lg {
			return int(k)
		}
	}
}

// NormalInv is the inverse standard normal CDF (Acklam's rational
// approximation, relative error below 1.2e-9). u must be in (0, 1).
func NormalInv(u float64) float64 {
	const (
		a1, a2, a3 = -3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02
		a4, a5, a6 = 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00
		b1, b2, b3 = -5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02
		b4, b5     = 6.680131188771972e+01, -1.328068155288572e+01
		c1, c2, c3 = -7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00
		c4, c5, c6 = -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00
		d1, d2, d3 = 7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00
		d4         = 3.754408661907416e+00
		low        = 0.02425
	)
	switch {
	case u < low:
		q := math.Sqrt(-2 * math.Log(u))
		return (((((c1*q+c2)*q+c3)*q+c4)*q+c5)*q + c6) / ((((d1*q+d2)*q+d3)*q+d4)*q + 1)
	case u > 1-low:
		q := math.Sqrt(-2 * math.Log(1-u))
		return -(((((c1*q+c2)*q+c3)*q+c4)*q+c5)*q + c6) / ((((d1*q+d2)*q+d3)*q+d4)*q + 1)
	default:
		q := u - 0.5
		r := q * q
		return (((((a1*r+a2)*r+a3)*r+a4)*r+a5)*r + a6) * q / (((((b1*r+b2)*r+b3)*r+b4)*r+b5)*r + 1)
	}
}

// Kind names a distribution family.
type Kind string

// Supported distribution families.
const (
	Const     Kind = "const"
	Uniform   Kind = "uniform"
	Normal    Kind = "normal"
	LogNormal Kind = "lognormal"
	Exp       Kind = "exp"
)

// Dist is a parameterised distribution, as written in seeds:
// uniform(a, b), normal(mean, sd), lognormal(mu, sigma), exp(mean) or a
// constant.
type Dist struct {
	Kind Kind
	A, B float64
}

// Sample returns a value for the uniform u in (0, 1).
func (d Dist) Sample(u float64) float64 {
	switch d.Kind {
	case Uniform:
		return d.A + u*(d.B-d.A)
	case Normal:
		return d.A + d.B*NormalInv(u)
	case LogNormal:
		return math.Exp(d.A + d.B*NormalInv(u))
	case Exp:
		return -d.A * math.Log(u)
	default:
		return d.A
	}
}

// SampleInt returns an integer value. uniform(a, b) is inclusive of both
// ends; other families are rounded.
func (d Dist) SampleInt(u float64) int {
	if d.Kind == Uniform {
		lo, hi := math.Round(d.A), math.Round(d.B)
		return int(lo + math.Floor(u*(hi-lo+1)))
	}
	return int(math.Round(d.Sample(u)))
}

// Mean returns the distribution's mean.
func (d Dist) Mean() float64 {
	switch d.Kind {
	case Uniform:
		return (d.A + d.B) / 2
	case LogNormal:
		return math.Exp(d.A + d.B*d.B/2)
	default:
		return d.A
	}
}

func (d Dist) String() string {
	switch d.Kind {
	case Const:
		return fmt.Sprint(d.A)
	case Exp:
		return fmt.Sprintf("exp(%g)", d.A)
	default:
		return fmt.Sprintf("%s(%g, %g)", d.Kind, d.A, d.B)
	}
}

// Zipf picks ranks 0..n-1 with probability proportional to 1/(rank+1)^s.
// s = 0 gives a uniform pick.
type Zipf struct {
	cdf []float64
}

// NewZipf builds the table for n items with exponent s.
func NewZipf(n int, s float64) Zipf {
	cdf := make([]float64, n)
	total := 0.0
	for i := range cdf {
		total += 1 / math.Pow(float64(i+1), s)
		cdf[i] = total
	}
	for i := range cdf {
		cdf[i] /= total
	}
	return Zipf{cdf: cdf}
}

// Pick returns the rank for the uniform u.
func (z Zipf) Pick(u float64) int {
	lo, hi := 0, len(z.cdf)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if u <= z.cdf[mid] {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}
