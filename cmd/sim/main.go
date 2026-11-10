// Command sim finds the stack that maximizes P(final balance >= target).
//
//	./sim -build markets_dump.csv            # once: map every race to exchange IDs -> races.csv
//	./sim -shift 2 -target 3                 # optimize, stress-test, write targets.csv
//	./sim -races races.seed.csv -offline ... # today, before you have IDs or keys
//
// Every input you own is marked ">>> YOU" in YOU_FILL_IN.md.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"sigcup/internal/sim"
	"sigcup/internal/susq"
)

func main() {
	build := flag.String("build", "", "build races.csv from this markets_dump.csv and exit")
	racesPath := flag.String("races", "races.csv", "race file (races.seed.csv works offline)")
	seedPath := flag.String("seed", "races.seed.csv", "build: fill blank prices from this board (\"\" = off)")
	offline := flag.Bool("offline", false, "use prices in the race file instead of live books")

	// >>> YOU: your view and the finish line.
	shift := flag.Float64("shift", 0, ">>> YOU: points better for Dems than markets price (negative = better for GOP)")
	target := flag.Float64("target", 3, ">>> YOU: final balance that wins 1st, as a multiple of bankroll")
	reserve := flag.Float64("reserve", 0.3, ">>> YOU: fraction kept in cash for election night")
	maxBet := flag.Float64("max-bet", 5000, ">>> YOU: cap per contract in SUSQies (set from depth.csv)")
	onlyAnti := flag.Bool("only-anti", false, "only bet the anti-GOP side")
	exclude := flag.String("exclude", "", "never bet these: state codes (FL) or race keys (MS-SEN), comma-separated")
	minP := flag.Float64("min-p", 0.10, "never bet a side the market gives less than this (tail prices are mostly longshot bias)")

	// Model knobs. Defaults are reasonable; see YOU_FILL_IN.md before changing.
	natSigma := flag.Float64("nat-sigma", 3.5, "national polling-miss sd (points)")
	natDF := flag.Float64("nat-df", 5, "fat tails on the national term (t dof, 0 = normal)")
	stateSD := flag.Float64("state-sd", 2.0, "shared noise within a state")
	localSD := flag.Float64("local-sd", 4.5, "candidate/district noise")
	controlSD := flag.Float64("control-sd", 1.5, "local noise for chamber-control markets")

	// Execution assumptions.
	halfSpread := flag.Float64("half-spread", 0.04, "offline: assumed half spread over the mid")
	impact := flag.Float64("impact", 0.01, "extra cost for moving thin books while building")
	room := flag.Float64("room", 0.02, "targets.csv max_price = assumed cost + room (never above your model prob)")

	bankroll := flag.Float64("bankroll", 0, "portfolio value (0 = live from the API, or 100000 offline)")
	chunk := flag.Float64("chunk", 0, "search step in SUSQies (0 = 2% of bankroll)")
	sims := flag.Int("sims", 8000, "simulations used by the search")
	evalSims := flag.Int("eval-sims", 50000, "fresh simulations used to report results")
	stress := flag.String("stress", "-3,-1.5,0,1.5,3,4.5", "shifts to re-test the chosen stack under")
	outPath := flag.String("out", "targets.csv", "accumulate targets for the bot")
	reportPath := flag.String("report", "sim_report.txt", "human-readable report")
	flag.Parse()

	loadDotEnv(".env")
	ctx := context.Background()

	if *build != "" {
		buildRaces(ctx, *build, *racesPath, *seedPath)
		return
	}

	races, err := sim.LoadRaces(*racesPath)
	if err != nil {
		if os.IsNotExist(err) && *racesPath == "races.csv" {
			fatal("races.csv not found. Run ./sim -build markets_dump.csv, or use -races races.seed.csv -offline today.")
		}
		fatal(err.Error())
	}

	bank := *bankroll
	if !*offline {
		s, tID, value := client(ctx)
		if bank == 0 {
			bank = value
		}
		fillBooks(ctx, s, tID, races)
	}
	if bank == 0 {
		bank = 100000
	}
	if *chunk == 0 {
		// About 2% of bankroll, but dividing max-bet evenly so bets can reach the cap.
		k := math.Ceil(*maxBet / (bank * 0.02))
		*chunk = math.Max(100, *maxBet/k)
	}

	p := sim.Params{Shift: *shift, NatSigma: *natSigma, NatDF: *natDF, StateSD: *stateSD, LocalSD: *localSD,
		ControlSD: *controlSD, Sims: *sims, Seed: 1}
	cfg := sim.OptConfig{Bankroll: bank, Target: bank * *target, Reserve: *reserve, MaxPerBet: *maxBet,
		Chunk: *chunk, MaxSteps: 400, OnlyAntiGOP: *onlyAnti}

	cands := sim.Candidates(races, *halfSpread, *impact, *minP)
	var dropped []string
	if *exclude != "" {
		cands, dropped = sim.Exclude(cands, races, strings.Split(*exclude, ","))
	}
	fmt.Fprintf(os.Stderr, "%d races, %d candidate bets. Searching %d sims (this takes a few minutes)...\n",
		len(races), len(cands), *sims)
	t0 := time.Now()
	out := sim.Simulate(races, p)
	bench := sim.Benchmarks(cands, races, cfg)
	var starts []sim.Portfolio
	for _, b := range bench {
		starts = append(starts, b)
	}
	port := sim.Optimize(cands, out, cfg, starts...)
	fmt.Fprintf(os.Stderr, "search took %s\n", time.Since(t0).Round(time.Second))

	// Judge everything on fresh simulations.
	pe := p
	pe.Sims, pe.Seed = *evalSims, 7
	eval := sim.Simulate(races, pe)

	f, err := os.Create(*reportPath)
	if err != nil {
		fatal(err.Error())
	}
	defer f.Close()
	w := io.MultiWriter(os.Stdout, f)
	if len(dropped) > 0 {
		fmt.Fprintf(w, "Excluded by you: %s\n", strings.Join(dropped, " "))
	}
	report(w, races, cands, port, bench, eval, p, pe, cfg, *stress, *offline)

	n, missing := writeTargets(*outPath, races, port, p, *room)
	fmt.Fprintf(w, "\nWrote %d targets to %s.\n", n, *outPath)
	if missing > 0 {
		fmt.Fprintf(w, "WARNING: %d targets have no exchange ID (offline seed file). Rebuild races.csv from your\n"+
			"markets dump and rerun before giving %s to the bot. The bot rejects rows without IDs.\n", missing, *outPath)
	}
}

func report(w io.Writer, races []sim.Race, cands []sim.Candidate, port sim.Portfolio, bench map[string]sim.Portfolio,
	eval sim.Outcomes, p, pe sim.Params, cfg sim.OptConfig, stress string, offline bool) {
	src := "live books"
	if offline {
		src = "prices in the race file (offline)"
	}
	fmt.Fprintf(w, "SIM REPORT  %s\n", time.Now().Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "Prices: %s. Bankroll %.0f. Target %.0f (%.1fx). Reserve %.0f%%. Max per bet %.0f.\n",
		src, cfg.Bankroll, cfg.Target, cfg.Target/cfg.Bankroll, cfg.Reserve*100, cfg.MaxPerBet)
	fmt.Fprintf(w, "Your view: national environment %+.1f pts vs market (national sd %.1f, t dof %.0f).\n",
		p.Shift, p.NatSigma, p.NatDF)
	if u := sim.Unpriced(races); len(u) > 0 {
		fmt.Fprintf(w, "Out of play, no price (set p_fair in races.csv to include): %s\n", strings.Join(u, " "))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "RESULTS on fresh simulations")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "portfolio\tP(hit target)\tmedian\tp10\tp90\tP(lose half)")
	row := func(name string, pf sim.Portfolio) {
		st := sim.Summarize(sim.Balances(pf, cands, eval), cfg.Target, cfg.Bankroll)
		fmt.Fprintf(tw, "%s\t%.1f%%\t%.0f\t%.0f\t%.0f\t%.1f%%\n", name, st.PTarget*100, st.Median, st.P10, st.P90, st.PLoseHalf*100)
	}
	row("OPTIMIZED", port)
	names := make([]string, 0, len(bench))
	for k := range bench {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		row(k, bench[k])
	}
	tw.Flush()

	fmt.Fprintln(w, "\nSTRESS: the same stack if the true environment differs from your view")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "true shift\tP(hit target)\tmedian\tP(lose half)")
	for _, x := range strings.Split(stress, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			continue
		}
		ps := pe
		ps.Shift, ps.Sims, ps.Seed = v, 20000, 11
		st := sim.Summarize(sim.Balances(port, cands, sim.Simulate(races, ps)), cfg.Target, cfg.Bankroll)
		fmt.Fprintf(tw, "%+.1f\t%.1f%%\t%.0f\t%.1f%%\n", v, st.PTarget*100, st.Median, st.PLoseHalf*100)
	}
	tw.Flush()

	fmt.Fprintf(w, "\nSTACK: %d bets, %.0f staked, %.0f cash\n", len(port.Holdings), port.Staked(), port.Cash)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "bet\texchange\tbuy\tcost\tnotional\tshares\tmarket P\tyour P\tpays")
	for _, h := range port.Holdings {
		r := races[h.Cand.Race]
		mp, yp := r.P(), sim.ModelProb(r, p)
		if !h.Cand.AntiGOP {
			mp, yp = 1-mp, 1-yp
		}
		id := h.Cand.Exch
		if id == "" {
			id = "(no id)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.3f\t%.0f\t%.0f\t%.2f\t%.2f\t%.1fx\n", h.Cand.Label, id, h.Cand.Buy, h.Cand.Cost,
			h.Notional, h.Notional/h.Cand.Cost, mp, yp, 1/h.Cand.Cost)
	}
	tw.Flush()

	fmt.Fprintln(w, "\nBIGGEST GAPS between your view and the price (anti-GOP side, cost to enter)")
	type gap struct {
		label           string
		cost, mp, yp, e float64
	}
	var gs []gap
	for _, c := range cands {
		if !c.AntiGOP {
			continue
		}
		r := races[c.Race]
		yp := sim.ModelProb(r, p)
		gs = append(gs, gap{c.Label, c.Cost, r.P(), yp, yp - c.Cost})
	}
	sort.Slice(gs, func(a, b int) bool { return gs[a].e > gs[b].e })
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "race\tcost\tmarket P\tyour P\tedge")
	for i, g := range gs {
		if i >= 15 {
			break
		}
		fmt.Fprintf(tw, "%s\t%.3f\t%.2f\t%.2f\t%+.2f\n", g.label, g.cost, g.mp, g.yp, g.e)
	}
	tw.Flush()
	fmt.Fprintln(w, "\nNot modeled: election-night rolling of the reserve, what other players hold, and")
	fmt.Fprintln(w, "chamber control computed seat by seat (control markets ride the national term instead).")
}

// writeTargets turns the stack into accumulate targets. max_price is the
// price the sim assumed plus room, but never above your own probability.
func writeTargets(path string, races []sim.Race, port sim.Portfolio, p sim.Params, room float64) (n, missing int) {
	f, err := os.Create(path)
	if err != nil {
		fatal(err.Error())
	}
	defer f.Close()
	fmt.Fprintf(f, "# Generated by sim %s, shift %+.1f.\n", time.Now().Format("2006-01-02 15:04"), p.Shift)
	fmt.Fprintln(f, "# The bot builds each position with resting bids at or below max_price (price of the side")
	fmt.Fprintln(f, "# you buy). >>> YOU: review every row. take_below (optional): take the ask outright at or")
	fmt.Fprintln(f, "# below this price. Delete a row to drop it; edit max_price or target_shares freely.")
	w := csv.NewWriter(f)
	w.Write([]string{"exchange_id", "buy", "max_price", "target_shares", "take_below", "note"})
	for _, h := range port.Holdings {
		r := races[h.Cand.Race]
		yp := sim.ModelProb(r, p)
		if !h.Cand.AntiGOP {
			yp = 1 - yp
		}
		mx := math.Min(h.Cand.Cost+room, yp-0.01)
		mx = math.Floor(mx/0.005+1e-9) * 0.005
		if mx < h.Cand.Cost-0.03 {
			mx = math.Floor(h.Cand.Cost/0.005+1e-9) * 0.005
		}
		shares := int(h.Notional / h.Cand.Cost)
		if h.Cand.Exch == "" {
			missing++
		}
		w.Write([]string{h.Cand.Exch, h.Cand.Buy, fmt.Sprintf("%.3f", mx), strconv.Itoa(shares), "",
			fmt.Sprintf("%s, sim cost %.3f, your P %.2f", h.Cand.Label, h.Cand.Cost, yp)})
		n++
	}
	w.Flush()
	return n, missing
}

func client(ctx context.Context) (*susq.Client, string, float64) {
	key, slug := os.Getenv("SUSQ_API_KEY"), os.Getenv("SUSQ_TOURNAMENT_SLUG")
	if key == "" || slug == "" {
		fatal("SUSQ_API_KEY / SUSQ_TOURNAMENT_SLUG not set. Use -offline to run on file prices.")
	}
	s := susq.New(os.Getenv("SUSQ_BASE_URL"), key)
	t, err := s.Tournament(ctx, slug)
	if err != nil {
		fatal(err.Error())
	}
	s.TournamentID = t.ID
	value := t.MyBalance
	if pos, err := s.Positions(ctx, slug); err == nil {
		value += pos.Summary.TotalMarketValue
	}
	return s, t.ID, value
}

func fillBooks(ctx context.Context, s *susq.Client, _ string, races []sim.Race) {
	var ids []string
	for _, r := range races {
		if r.AntiID != "" {
			ids = append(ids, r.AntiID)
		}
		if r.GOPID != "" {
			ids = append(ids, r.GOPID)
		}
	}
	if len(ids) == 0 {
		fatal("race file has no exchange IDs; run -build first or use -offline")
	}
	q, err := s.Prices(ctx, ids)
	if err != nil {
		fatal(err.Error())
	}
	v := func(x *float64) float64 {
		if x == nil {
			return 0
		}
		return *x
	}
	for i := range races {
		if a, ok := q[races[i].AntiID]; ok {
			races[i].AntiBid, races[i].AntiAsk = v(a.BestBid), v(a.BestAsk)
		}
		if g, ok := q[races[i].GOPID]; ok {
			races[i].GOPBid, races[i].GOPAsk = v(g.BestBid), v(g.BestAsk)
		}
	}
}

// buildRaces reads markets_dump.csv (from ./sigcup -mode markets), prices
// each contract live when possible, and writes races.csv.
func buildRaces(ctx context.Context, dump, out, seedPath string) {
	f, err := os.Open(dump)
	if err != nil {
		fatal(err.Error())
	}
	recs, err := csv.NewReader(f).ReadAll()
	f.Close()
	if err != nil {
		fatal(err.Error())
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	for _, need := range []string{"market_id", "exchange_id", "title", "option", "latest"} {
		if _, ok := col[need]; !ok {
			fatal(dump + " is missing column " + need + "; regenerate it with ./sigcup -mode markets")
		}
	}
	// One contract per market: a binary market's YES exchange. If a market
	// lists several exchanges, prefer the one whose option is "Yes".
	byMarket := map[string][]string{}
	var order []string
	for _, r := range recs[1:] {
		m := r[col["market_id"]]
		if _, ok := byMarket[m]; !ok {
			order = append(order, m)
		}
		byMarket[m] = append(byMarket[m], strings.Join(r, "\x00"))
	}
	var cs []sim.Contract
	multi := 0
	for _, m := range order {
		rows := byMarket[m]
		pick := strings.Split(rows[0], "\x00")
		if len(rows) > 1 {
			multi++
			for _, raw := range rows {
				r := strings.Split(raw, "\x00")
				if strings.EqualFold(r[col["option"]], "yes") {
					pick = r
				}
			}
		}
		px, _ := strconv.ParseFloat(pick[col["latest"]], 64)
		cs = append(cs, sim.Contract{ExchangeID: pick[col["exchange_id"]], Title: pick[col["title"]], Px: px})
	}
	if os.Getenv("SUSQ_API_KEY") != "" && os.Getenv("SUSQ_TOURNAMENT_SLUG") != "" {
		s, _, _ := client(ctx)
		ids := make([]string, len(cs))
		for i, c := range cs {
			ids[i] = c.ExchangeID
		}
		if q, err := s.Prices(ctx, ids); err == nil {
			for i := range cs {
				if x, ok := q[cs[i].ExchangeID]; ok && x.BestBid != nil && x.BestAsk != nil {
					cs[i].Px = (*x.BestBid + *x.BestAsk) / 2
				}
			}
		}
	}
	races, skipped := sim.BuildRaces(cs)
	filled := 0
	if seedPath != "" {
		if seed, err := sim.LoadRaces(seedPath); err == nil {
			filled = sim.FillFromSeed(races, seed)
		}
	}
	if err := sim.SaveRaces(out, races); err != nil {
		fatal(err.Error())
	}
	fmt.Printf("%s: %d races from %d contracts.\n", out, len(races), len(cs))
	if filled > 0 {
		fmt.Printf("%d races had no two-sided book or trade yet; their blank prices came from %s.\n", filled, seedPath)
	}
	if u := sim.Unpriced(races); len(u) > 0 {
		fmt.Printf("%d races still have no price and are out of play: %s\n", len(u), strings.Join(u, " "))
	}
	if multi > 0 {
		fmt.Printf("NOTE: %d markets had more than one exchange; used the \"Yes\" option. Spot-check one.\n", multi)
	}
	for _, s := range skipped {
		fmt.Println("  skipped " + s)
	}
	fmt.Println(">>> YOU: check the Independent rows (NE-SEN, MT-SEN, RI-GOV) against their Info tabs.")
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || os.Getenv(strings.TrimSpace(k)) != "" {
			continue
		}
		os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`))
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "sim:", msg)
	os.Exit(1)
}
