package sim

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Candidate is one way to bet on one side of one race.
type Candidate struct {
	Race    int     // index into races
	AntiGOP bool    // true: pays when the anti-GOP side wins
	Exch    string  // SUSQ exchange to trade
	Buy     string  // "yes" or "no" on that exchange
	Cost    float64 // entry price per share, including slippage
	Label   string
}

// Candidates builds, per race and side, the cheaper of the two equivalent
// contracts (anti-GOP YES vs GOP NO, and GOP YES vs anti-GOP NO). Live books
// are used when present, else snapshot mids plus halfSpread. impact is added
// for moving the book while you build.
//
// Sides the market gives less than minP are left out. Converting a 6% price
// into "9 points behind" lets a modeled wave flip races that are really 25
// points, because tail prices are mostly longshot bias and price floors,
// not real chances. Those races are still simulated; you just can't bet
// their long side.
func Candidates(races []Race, halfSpread, impact, minP float64) []Candidate {
	var out []Candidate
	ask := func(bid, a, px float64) float64 { // cost to buy YES
		if a > 0 {
			return a
		}
		if px > 0 {
			return px + halfSpread
		}
		return 0
	}
	noAsk := func(bid, a, px float64) float64 { // cost to buy NO = 1 - YES bid
		if bid > 0 {
			return 1 - bid
		}
		if px > 0 {
			return 1 - px + halfSpread
		}
		return 0
	}
	for i, r := range races {
		// No usable price means no baseline: a blank P would read as 0% and
		// make the favorite look like a near-certain loser. Out of play
		// until it has a price or you set p_fair.
		if r.P() <= 0 {
			continue
		}
		type opt struct {
			exch, buy string
			cost      float64
		}
		pick := func(a, b opt) opt {
			switch {
			case a.cost <= 0:
				return b
			case b.cost <= 0:
				return a
			case b.cost < a.cost:
				return b
			}
			return a
		}
		// A GOP NO also pays if an Independent beats the Democrat, so in
		// three-way races the two aren't identical. Both still pay whenever
		// the anti-GOP side wins, which is what the model scores.
		anti := pick(opt{r.AntiID, "yes", ask(r.AntiBid, r.AntiAsk, r.AntiPx)},
			opt{r.GOPID, "no", noAsk(r.GOPBid, r.GOPAsk, r.GOPPx)})
		gop := pick(opt{r.GOPID, "yes", ask(r.GOPBid, r.GOPAsk, r.GOPPx)},
			opt{r.AntiID, "no", noAsk(r.AntiBid, r.AntiAsk, r.AntiPx)})
		for _, x := range []struct {
			o    opt
			anti bool
		}{{anti, true}, {gop, false}} {
			c := x.o.cost + impact
			if x.o.cost <= 0 || c >= 0.99 {
				continue
			}
			if mp := r.P(); (x.anti && mp < minP) || (!x.anti && 1-mp < minP) {
				continue
			}
			side := "GOP"
			if x.anti {
				side = map[string]string{"D": "Dem", "I": "Ind"}[r.AntiParty]
			}
			out = append(out, Candidate{Race: i, AntiGOP: x.anti, Exch: x.o.exch, Buy: x.o.buy,
				Cost: math.Round(c*1000) / 1000, Label: r.Key + " " + side})
		}
	}
	return out
}

// OptConfig bounds the search.
type OptConfig struct {
	Bankroll    float64 // current portfolio value
	Target      float64 // >>> YOU: the final balance you think wins 1st
	Reserve     float64 // fraction kept as cash (election-night rolls)
	MaxPerBet   float64 // cap per contract, SUSQies (depth-limited)
	Chunk       float64 // search step, SUSQies
	MaxSteps    int
	OnlyAntiGOP bool // restrict to anti-GOP side
}

type Holding struct {
	Cand     Candidate
	Notional float64
}

type Portfolio struct {
	Holdings []Holding
	Cash     float64
}

func (p Portfolio) Staked() float64 {
	s := 0.0
	for _, h := range p.Holdings {
		s += h.Notional
	}
	return s
}

// Balances returns final balance per simulation.
func Balances(p Portfolio, cands []Candidate, out Outcomes) []float64 {
	n := len(out[0])
	b := make([]float64, n)
	for s := range b {
		b[s] = p.Cash
	}
	for _, h := range p.Holdings {
		sh := h.Notional / h.Cand.Cost
		w := out[h.Cand.Race]
		for s := 0; s < n; s++ {
			if w[s] == h.Cand.AntiGOP {
				b[s] += sh
			}
		}
	}
	return b
}

type Stats struct {
	PTarget, Mean, Median, P10, P90, PLoseHalf float64
}

func Summarize(b []float64, target, bankroll float64) Stats {
	x := append([]float64(nil), b...)
	sort.Float64s(x)
	st := Stats{Median: x[len(x)/2], P10: x[len(x)/10], P90: x[len(x)*9/10]}
	for _, v := range x {
		st.Mean += v
		if v >= target {
			st.PTarget++
		}
		if v < bankroll/2 {
			st.PLoseHalf++
		}
	}
	n := float64(len(x))
	st.Mean /= n
	st.PTarget /= n
	st.PLoseHalf /= n
	return st
}

// Optimize searches for the portfolio that maximizes P(balance >= target).
//
// The hard objective is flat (one small bet never reaches a 5x target by
// itself), so the search climbs a smoothed version, a sigmoid of
// log(balance) against log(target), annealed toward the hard threshold.
// Moves are add a chunk, remove a chunk, or swap a chunk between two
// contracts. Swaps matter: moving money from a safe bet to a longshot is
// often only an improvement as one step. The search runs from several
// starting portfolios (empty plus each benchmark) and keeps the best.
func Optimize(cands []Candidate, out Outcomes, cfg OptConfig, starts ...Portfolio) Portfolio {
	n := len(out[0])
	wins := make([][]bool, len(cands))
	for j, c := range cands {
		w := make([]bool, n)
		src := out[c.Race]
		for s := 0; s < n; s++ {
			w[s] = src[s] == c.AntiGOP
		}
		wins[j] = w
	}
	idx := map[string]int{}
	for j, c := range cands {
		idx[c.Exch+c.Buy] = j
	}
	best, bestP := Portfolio{Cash: cfg.Bankroll}, -1.0
	for _, st := range append([]Portfolio{{Cash: cfg.Bankroll}}, starts...) {
		o := newOpt(cands, wins, cfg)
		for _, h := range st.Holdings {
			if j, ok := idx[h.Cand.Exch+h.Cand.Buy]; ok {
				amt := math.Min(h.Notional, cfg.MaxPerBet)
				amt = math.Floor(amt/cfg.Chunk) * cfg.Chunk
				if amt > 0 && o.staked+amt <= o.budget+1e-6 {
					o.apply(j, amt)
				}
			}
		}
		o.search()
		p := o.portfolio()
		if pt := Summarize(o.bal, cfg.Target, cfg.Bankroll).PTarget; pt > bestP {
			best, bestP = p, pt
		}
	}
	return best
}

type optState struct {
	cands  []Candidate
	wins   [][]bool
	cfg    OptConfig
	bal    []float64
	held   []float64
	cash   float64
	staked float64
	budget float64
	lt     float64
	steps  int
	base   []float64 // sig(bal[s]) at the current tau
	tau    float64
}

func newOpt(cands []Candidate, wins [][]bool, cfg OptConfig) *optState {
	n := len(wins[0])
	o := &optState{cands: cands, wins: wins, cfg: cfg, bal: make([]float64, n), held: make([]float64, len(cands)),
		cash: cfg.Bankroll, budget: cfg.Bankroll * (1 - cfg.Reserve), lt: math.Log(cfg.Target)}
	for s := range o.bal {
		o.bal[s] = cfg.Bankroll
	}
	return o
}

func (o *optState) apply(j int, amt float64) {
	sh := amt / o.cands[j].Cost
	w := o.wins[j]
	for s := range o.bal {
		o.bal[s] -= amt
		if w[s] {
			o.bal[s] += sh
		}
	}
	o.base = nil // stale
	o.held[j] += amt
	o.cash -= amt
	o.staked += amt
}

// gain of moving one chunk: add to k (k>=0) and/or remove from j (j>=0).
func (o *optState) gain(tau float64, add, rem int) float64 {
	c := o.cfg.Chunk
	var shA, shR float64
	var wA, wR []bool
	if add >= 0 {
		shA, wA = c/o.cands[add].Cost, o.wins[add]
	}
	if rem >= 0 {
		shR, wR = c/o.cands[rem].Cost, o.wins[rem]
	}
	t := 0.0
	for s, b := range o.bal {
		v := b
		if add >= 0 {
			v -= c
			if wA[s] {
				v += shA
			}
		}
		if rem >= 0 {
			v += c
			if wR[s] {
				v -= shR
			}
		}
		t += sig(v, o.lt, tau) - o.base[s]
	}
	return t / float64(len(o.bal))
}

func sig(v, lt, tau float64) float64 {
	if v <= 0 {
		return 0
	}
	return 1 / (1 + math.Exp(-(math.Log(v)-lt)/tau))
}

func (o *optState) canAdd(k int) bool {
	c := o.cfg.Chunk
	if o.cfg.OnlyAntiGOP && !o.cands[k].AntiGOP {
		return false
	}
	return o.held[k]+c <= o.cfg.MaxPerBet+1e-6
}

func (o *optState) refreshBase(tau float64) {
	if o.base != nil && o.tau == tau {
		return
	}
	if o.base == nil {
		o.base = make([]float64, len(o.bal))
	}
	for s, b := range o.bal {
		o.base[s] = sig(b, o.lt, tau)
	}
	o.tau = tau
}

// gains scores many moves in parallel.
func (o *optState) gains(tau float64, moves [][2]int) []float64 {
	out := make([]float64, len(moves))
	workers := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	next := int64(-1)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1))
				if i >= len(moves) {
					return
				}
				out[i] = o.gain(tau, moves[i][0], moves[i][1])
			}
		}()
	}
	wg.Wait()
	return out
}

func (o *optState) search() {
	c := o.cfg.Chunk
	for _, tau := range []float64{0.6, 0.3, 0.15, 0.08, 0.04} {
		for o.steps < o.cfg.MaxSteps {
			o.refreshBase(tau)
			bestA, bestR, bestG := -1, -1, 1e-9
			type cg struct {
				k int
				g float64
			}
			var moves [][2]int
			for k := range o.cands {
				if o.canAdd(k) {
					moves = append(moves, [2]int{k, -1})
				}
			}
			nAdds := len(moves)
			for j := range o.cands {
				if o.held[j] >= c-1e-6 {
					moves = append(moves, [2]int{-1, j})
				}
			}
			gs := o.gains(tau, moves)
			var adds []cg
			for i, m := range moves {
				g := gs[i]
				if i < nAdds {
					adds = append(adds, cg{m[0], g})
					if o.staked+c > o.budget+1e-6 {
						continue
					}
				}
				if g > bestG {
					bestA, bestR, bestG = m[0], m[1], g
				}
			}
			if bestA < 0 && bestR < 0 {
				// No single move helps: try swaps into the most promising adds.
				sort.Slice(adds, func(a, b int) bool { return adds[a].g > adds[b].g })
				if len(adds) > 15 {
					adds = adds[:15]
				}
				var swaps [][2]int
				for j := range o.cands {
					if o.held[j] < c-1e-6 {
						continue
					}
					for _, a := range adds {
						if a.k != j {
							swaps = append(swaps, [2]int{a.k, j})
						}
					}
				}
				for i, g := range o.gains(tau, swaps) {
					if g > bestG {
						bestA, bestR, bestG = swaps[i][0], swaps[i][1], g
					}
				}
			}
			if bestA < 0 && bestR < 0 {
				break
			}
			if bestR >= 0 {
				o.apply(bestR, -c)
			}
			if bestA >= 0 {
				o.apply(bestA, c)
			}
			o.steps++
		}
	}
}

func (o *optState) portfolio() Portfolio {
	p := Portfolio{Cash: o.cash}
	for j, h := range o.held {
		if h > 1e-6 {
			p.Holdings = append(p.Holdings, Holding{Cand: o.cands[j], Notional: h})
		}
	}
	sort.Slice(p.Holdings, func(a, b int) bool { return p.Holdings[a].Notional > p.Holdings[b].Notional })
	return p
}

// Benchmarks are simple portfolios to compare the optimizer against.
func Benchmarks(cands []Candidate, races []Race, cfg OptConfig) map[string]Portfolio {
	band := func(lo, hi float64) Portfolio {
		var picks []Candidate
		for _, c := range cands {
			if c.AntiGOP && c.Cost >= lo && c.Cost < hi {
				picks = append(picks, c)
			}
		}
		p := Portfolio{Cash: cfg.Bankroll}
		if len(picks) == 0 {
			return p
		}
		each := math.Min(cfg.Bankroll*(1-cfg.Reserve)/float64(len(picks)), cfg.MaxPerBet)
		for _, c := range picks {
			p.Holdings = append(p.Holdings, Holding{Cand: c, Notional: each})
			p.Cash -= each
		}
		return p
	}
	return map[string]Portfolio{
		"all cash":                {Cash: cfg.Bankroll},
		"Dem coin flips (45-65c)": band(0.45, 0.65),
		"Dem reach (25-45c)":      band(0.25, 0.45),
		"Dem deep tails (<25c)":   band(0.0, 0.25),
	}
}

func (h Holding) String() string {
	return fmt.Sprintf("%-14s buy %-3s @%.3f  %6.0f", h.Cand.Label, h.Cand.Buy, h.Cand.Cost, h.Notional)
}

// Unpriced lists races with no usable price (out of play).
func Unpriced(races []Race) []string {
	var out []string
	for _, r := range races {
		if r.P() <= 0 {
			out = append(out, r.Key)
		}
	}
	return out
}

// Exclude drops candidates whose race matches a token: a 2-letter state code
// (FL) excludes every race in that state; anything else must match a race
// key exactly (MS-SEN). Excluded races are still simulated, just not bet.
func Exclude(cands []Candidate, races []Race, tokens []string) ([]Candidate, []string) {
	if len(tokens) == 0 {
		return cands, nil
	}
	states, keys := map[string]bool{}, map[string]bool{}
	for _, t := range tokens {
		t = strings.ToUpper(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if len(t) == 2 {
			states[t] = true
		} else {
			keys[t] = true
		}
	}
	var out []Candidate
	seen := map[string]bool{}
	var dropped []string
	for _, c := range cands {
		r := races[c.Race]
		if states[r.State] || keys[r.Key] {
			if !seen[r.Key] {
				seen[r.Key] = true
				dropped = append(dropped, r.Key)
			}
			continue
		}
		out = append(out, c)
	}
	sort.Strings(dropped)
	return out, dropped
}
