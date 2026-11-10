package sim

import (
	"math"
	"math/rand"
)

// Params describe the national-swing model. Every race's Dem-side margin is
//
//	baseline + national + state + local
//
// where the baseline is set so that with Shift = 0 the model reproduces the
// market probability of every race exactly. Shift is your view: how many
// points better (positive) or worse for Democrats the national environment
// is than the markets think. The shared national term is what makes races
// move together.
type Params struct {
	Shift     float64 // >>> YOU: points toward Dems vs market consensus
	NatSigma  float64 // national polling-miss sd, points
	NatDF     float64 // t degrees of freedom for the national term (fat tails); 0 = normal
	StateSD   float64 // shared within a state (Senate + Gov + House seats move together)
	LocalSD   float64 // candidate/district noise for sen/gov/house
	ControlSD float64 // local noise for chamber-control markets (mostly national)
	Sims      int
	Seed      int64
}

func DefaultParams() Params {
	return Params{Shift: 0, NatSigma: 3.5, NatDF: 5, StateSD: 2.0, LocalSD: 4.5, ControlSD: 1.5, Sims: 20000, Seed: 1}
}

func (p Params) localSD(kind string) float64 {
	if kind == "control" {
		return p.ControlSD
	}
	return p.LocalSD
}

// totalSD is the sd of a race's margin around its baseline.
func (p Params) totalSD(kind string) float64 {
	return math.Sqrt(p.NatSigma*p.NatSigma + p.StateSD*p.StateSD + p.localSD(kind)*p.localSD(kind))
}

func probit(q float64) float64 {
	q = math.Min(0.995, math.Max(0.005, q))
	return math.Sqrt2 * math.Erfinv(2*q-1)
}

func normCDF(x float64) float64 { return 0.5 * (1 + math.Erf(x/math.Sqrt2)) }

// ModelProb is P(anti-GOP wins) for a race under the params (closed form,
// normal approximation of the national term). Used for reporting edges.
func ModelProb(r Race, p Params) float64 {
	sd := p.totalSD(r.Kind)
	return normCDF((sd*probit(r.P()) + p.Shift) / sd)
}

// Outcomes[r][s] is true when race r's anti-GOP side wins in simulation s.
type Outcomes [][]bool

func Simulate(races []Race, p Params) Outcomes {
	rng := rand.New(rand.NewSource(p.Seed))
	out := make(Outcomes, len(races))
	for i := range out {
		out[i] = make([]bool, p.Sims)
	}
	base := make([]float64, len(races))
	for i, r := range races {
		base[i] = p.totalSD(r.Kind) * probit(r.P())
	}
	stateIdx := map[string]int{}
	for _, r := range races {
		if _, ok := stateIdx[r.State]; !ok {
			stateIdx[r.State] = len(stateIdx)
		}
	}
	stateDraw := make([]float64, len(stateIdx))
	for s := 0; s < p.Sims; s++ {
		nat := p.Shift + p.NatSigma*unitT(rng, p.NatDF)
		for k := range stateDraw {
			stateDraw[k] = p.StateSD * rng.NormFloat64()
		}
		for i, r := range races {
			m := base[i] + nat + stateDraw[stateIdx[r.State]] + p.localSD(r.Kind)*rng.NormFloat64()
			out[i][s] = m > 0
		}
	}
	return out
}

// unitT draws a Student-t scaled to unit variance (normal when df <= 0).
func unitT(rng *rand.Rand, df float64) float64 {
	z := rng.NormFloat64()
	if df <= 2 {
		return z
	}
	chi := 0.0
	for k := 0; k < int(df); k++ {
		g := rng.NormFloat64()
		chi += g * g
	}
	return z / math.Sqrt(chi/df) * math.Sqrt((df-2)/df)
}
