package sim

import (
	"math"
	"testing"
)

func seedRaces(t *testing.T) []Race {
	r, err := LoadRaces("../../races.seed.csv")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// With Shift = 0 the model must reproduce every market probability, or the
// whole stack is built on a bias you didn't choose.
func TestCalibratedAtZeroShift(t *testing.T) {
	races := seedRaces(t)
	p := DefaultParams()
	p.NatDF = 0
	p.Sims = 40000
	out := Simulate(races, p)
	worst := 0.0
	for i, r := range races {
		w := 0
		for _, x := range out[i] {
			if x {
				w++
			}
		}
		got := float64(w) / float64(p.Sims)
		worst = math.Max(worst, math.Abs(got-r.P()))
	}
	if worst > 0.012 {
		t.Fatalf("calibration off by %.3f", worst)
	}
	// Fat tails keep medians but move the tails a little.
	p.NatDF = 5
	out = Simulate(races, p)
	for i, r := range races {
		w := 0
		for _, x := range out[i] {
			if x {
				w++
			}
		}
		if d := math.Abs(float64(w)/float64(p.Sims) - r.P()); d > 0.03 {
			t.Fatalf("%s off by %.3f with t tails", r.Key, d)
		}
	}
}

func TestShiftAndCorrelation(t *testing.T) {
	races := seedRaces(t)
	p := DefaultParams()
	p.Shift = 3
	byKey := map[string]int{}
	for i, r := range races {
		byKey[r.Key] = i
	}
	tx := races[byKey["TX-SEN"]]
	if m := ModelProb(tx, p); m <= tx.P() {
		t.Fatalf("a D shift must raise TX-SEN, got %.3f vs %.3f", m, tx.P())
	}
	// Races share a national term, so reach seats flip together.
	p.Shift = 0
	out := Simulate(races, p)
	a, b := out[byKey["VA-01"]], out[byKey["PA-01"]]
	both, na, nb := 0, 0, 0
	for s := range a {
		if a[s] {
			na++
		}
		if b[s] {
			nb++
		}
		if a[s] && b[s] {
			both++
		}
	}
	n := float64(len(a))
	if float64(both)/n <= float64(na)/n*float64(nb)/n*1.2 {
		t.Fatal("reach seats should be positively correlated")
	}
}

func TestParseTitle(t *testing.T) {
	cases := map[string]string{
		"Will the Democratic Party win the Texas Senate?":           "TX-SEN sen TX D",
		"Will the Republican Party win the TX-23 House race?":       "TX-23 house TX R",
		"Will the Independent Party win the Nebraska Senate?":       "NE-SEN sen NE I",
		"Will the Democratic Party win the New Hampshire Governor?": "NH-GOV gov NH D",
		"Will the Republican Party win the U.S. House?":             "US-HOUSE control US R",
	}
	for title, want := range cases {
		k, kind, st, party, ok := ParseTitle(title)
		if got := k + " " + kind + " " + st + " " + party; !ok || got != want {
			t.Errorf("%q -> %q, want %q", title, got, want)
		}
	}
	if _, _, _, _, ok := ParseTitle("Will Trump tweet?"); ok {
		t.Error("should not parse")
	}
}

func TestBuildRacesPicksIndependent(t *testing.T) {
	cs := []Contract{
		{"1", "Will the Republican Party win the Nebraska Senate?", 0.73},
		{"2", "Will the Independent Party win the Nebraska Senate?", 0.28},
		{"3", "Will the Democratic Party win the Nebraska Senate?", 0.04},
		{"4", "Will the Democratic Party win the Texas Senate?", 0.62},
		{"5", "Will the Republican Party win the Texas Senate?", 0.39},
		{"6", "Will the Democratic Party win the VA-07 House race?", 0.93},
	}
	races, skipped := BuildRaces(cs)
	if len(races) != 2 || len(skipped) != 1 {
		t.Fatalf("got %d races, skipped %v", len(races), skipped)
	}
	if races[0].Key != "NE-SEN" || races[0].AntiID != "2" || races[0].AntiParty != "I" {
		t.Fatalf("NE should use the Independent: %+v", races[0])
	}
}

func TestCandidatesPickCheaperSide(t *testing.T) {
	r := []Race{{Key: "X", Kind: "house", State: "XX", AntiParty: "D", AntiID: "a", GOPID: "g",
		AntiBid: 0.30, AntiAsk: 0.36, GOPBid: 0.68, GOPAsk: 0.72}}
	cs := Candidates(r, 0.04, 0, 0.10)
	// Dem YES at 0.36 vs GOP NO at 1-0.68 = 0.32: buy GOP NO.
	if cs[0].Exch != "g" || cs[0].Buy != "no" || math.Abs(cs[0].Cost-0.32) > 1e-9 {
		t.Fatalf("anti side: %+v", cs[0])
	}
	// GOP YES at 0.72 vs Dem NO at 1-0.30 = 0.70: buy Dem NO.
	if cs[1].Exch != "a" || cs[1].Buy != "no" || math.Abs(cs[1].Cost-0.70) > 1e-9 {
		t.Fatalf("gop side: %+v", cs[1])
	}
}

func TestOptimizerBeatsBenchmarks(t *testing.T) {
	races := seedRaces(t)
	p := DefaultParams()
	p.Shift = 2
	p.Sims = 6000
	out := Simulate(races, p)
	cands := Candidates(races, 0.04, 0.01, 0.10)
	cfg := OptConfig{Bankroll: 100000, Target: 250000, Reserve: 0.2, MaxPerBet: 10000, Chunk: 2000, MaxSteps: 200}
	var starts []Portfolio
	for _, b := range Benchmarks(cands, races, cfg) {
		starts = append(starts, b)
	}
	port := Optimize(cands, out, cfg, starts...)
	if port.Staked() > 80000+1 || port.Cash < 20000-1 {
		t.Fatalf("reserve violated: staked %.0f", port.Staked())
	}
	for _, h := range port.Holdings {
		if h.Notional > 10000+1 {
			t.Fatalf("per-bet cap violated: %v", h)
		}
	}
	p.Seed = 99 // judge on fresh sims
	out2 := Simulate(races, p)
	got := Summarize(Balances(port, cands, out2), cfg.Target, cfg.Bankroll).PTarget
	for name, b := range Benchmarks(cands, races, cfg) {
		bp := Summarize(Balances(b, cands, out2), cfg.Target, cfg.Bankroll).PTarget
		t.Logf("%-26s P(>=2.5x) %.3f", name, bp)
		if bp > got+0.01 {
			t.Errorf("optimizer (%.3f) lost to %s (%.3f)", got, name, bp)
		}
	}
	t.Logf("optimized P(>=2.5x) %.3f with %d holdings", got, len(port.Holdings))
}

func TestTailsExcluded(t *testing.T) {
	races := seedRaces(t)
	for _, c := range Candidates(races, 0.04, 0.01, 0.10) {
		r := races[c.Race]
		mp := r.P()
		if !c.AntiGOP {
			mp = 1 - mp
		}
		if mp < 0.10 {
			t.Fatalf("%s at market P %.2f should be out of play", c.Label, mp)
		}
	}
}

// A race with no price must never become a candidate: P = 0 would make the
// favorite look like a certain loser and its cheap side look like free money.
func TestUnpricedRaceOutOfPlay(t *testing.T) {
	r := []Race{
		{Key: "CA-GOV", Kind: "gov", State: "CA", AntiParty: "D", AntiID: "a", GOPID: "g", GOPAsk: 0.10},
		{Key: "TX-SEN", Kind: "sen", State: "TX", AntiParty: "D", AntiID: "b", GOPID: "h", AntiPx: 0.62, GOPPx: 0.39},
	}
	for _, c := range Candidates(r, 0.04, 0.01, 0.10) {
		if c.Race == 0 {
			t.Fatalf("unpriced race became a candidate: %+v", c)
		}
	}
	if u := Unpriced(r); len(u) != 1 || u[0] != "CA-GOV" {
		t.Fatalf("Unpriced = %v", u)
	}
}

func TestFillFromSeed(t *testing.T) {
	built := []Race{{Key: "RI-GOV"}, {Key: "DE-SEN", GOPPx: 0.12}, {Key: "TX-SEN", AntiPx: 0.625, GOPPx: 0.375}}
	seed := []Race{{Key: "RI-GOV", AntiPx: 0.93, GOPPx: 0.06}, {Key: "DE-SEN", AntiPx: 0.95, GOPPx: 0.12}, {Key: "TX-SEN", AntiPx: 0.62, GOPPx: 0.39}}
	n := FillFromSeed(built, seed)
	if n != 2 || built[0].AntiPx != 0.93 || built[1].AntiPx != 0.95 || built[1].GOPPx != 0.12 || built[2].AntiPx != 0.625 {
		t.Fatalf("filled %d: %+v", n, built)
	}
}

func TestExclude(t *testing.T) {
	races := seedRaces(t)
	cands, dropped := Exclude(Candidates(races, 0.04, 0.01, 0.10), races, []string{"fl", "MS-SEN"})
	for _, c := range cands {
		if r := races[c.Race]; r.State == "FL" || r.Key == "MS-SEN" {
			t.Fatalf("%s should be excluded", r.Key)
		}
	}
	if len(dropped) == 0 || dropped[0] != "FL-07" {
		t.Fatalf("dropped = %v", dropped)
	}
}
