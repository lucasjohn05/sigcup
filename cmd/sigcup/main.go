package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sigcup/internal/journal"
	"sigcup/internal/kalshi"
	"sigcup/internal/mapping"
	"sigcup/internal/risk"
	"sigcup/internal/sim"
	"sigcup/internal/strategy"
	"sigcup/internal/susq"
)

func main() {
	mode := flag.String("mode", "scan", "tournaments | markets | scan | dry | live | stage | fire | flatten")
	mapPath := flag.String("mapping", "mapping.csv", "SUSQ->Kalshi contract map")
	interval := flag.Duration("interval", 20*time.Second, "loop interval")
	edge := flag.Float64("edge", 0.05, "min distance through Kalshi fair value")
	kSpread := flag.Float64("kalshi-max-spread", 0.03, "ignore Kalshi books wider than this")
	arb := flag.Bool("arb", true, "relationship arbitrage")
	arbEdge := flag.Float64("arb-edge", 0.02, "min guaranteed profit per arb set")
	maxOrder := flag.Float64("max-order", 2000, "max SUSQies per order")
	maxEx := flag.Float64("max-exchange", 5000, "max cost basis per contract")
	maxDep := flag.Float64("max-deployed", 0.4, "max share of value in positions")
	dayLoss := flag.Float64("daily-loss", 0.10, "halt if value drops this much in a UTC day")
	minP := flag.Float64("min-price", 0.03, "never take below this price")
	maxP := flag.Float64("max-price", 0.97, "never take above this price")
	maxActions := flag.Int("max-actions", 5, "orders attempted per tick")
	ttl := flag.Duration("ttl", 15*time.Second, "order expiry; remainders are cancelled anyway")
	jpath := flag.String("journal", "sigcup.jsonl", "audit log")
	suspect := flag.Float64("suspect-gap", 0.15, "quarantine unverified mappings this far from Kalshi (0 = off)")
	repairTTL := flag.Duration("repair-ttl", 5*time.Second, "expiry for arb repair/unwind orders")
	unwindSlack := flag.Float64("unwind-slack", 0.03, "max per-share loss to flatten a stuck arb leg")
	digestHour := flag.Int("digest-hour", 12, "UTC hour for the daily digest (12 = 8am EDT), -1 = off")
	fvStop := flag.String("fv-stop", "2026-11-01T04:00:00Z", "no new fair-value trades after this RFC3339 time (\"\" = never)")
	lagThresh := flag.Float64("lag-thresh", 0.03, "Kalshi move that starts a SUSQ lag measurement (0 = off)")
	lagWait := flag.Duration("lag-max-wait", 6*time.Hour, "lag events still open after this count as unfollowed")
	depthPer := flag.Int("depth-per-tick", 3, "order books snapshotted per tick into -depth-file (0 = off)")
	depthFile := flag.String("depth-file", "depth.csv", "order book depth log")
	lbFile := flag.String("leaderboard-file", "leaderboard.csv", "hourly top-100 leaderboard snapshots")
	batchPath := flag.String("batch", "", "stage/fire: order batch CSV")
	confirm := flag.Bool("confirm", false, "fire: actually send (without it, fire only stages)")
	fireTTL := flag.Duration("fire-ttl", 10*time.Second, "fire: order expiry; remainders are cancelled")
	flatSlack := flag.Float64("flatten-slack", 0.02, "flatten: how far through the touch to price exits")
	outPath := flag.String("out", "flatten.csv", "flatten: output batch CSV")
	targetsPath := flag.String("targets", "targets.csv", "stack targets from ./sim (missing file = accumulate off; edits reload live)")
	fairPath := flag.String("fair", "fair.csv", "your fair values for contracts with no Kalshi market (optional)")
	fairEdge := flag.Float64("fair-edge", 0.08, "min edge vs fair.csv values")
	racesPath := flag.String("races", "races.csv", "race pairs for pair arbitrage (from ./sim -build); missing = off")
	accChunk := flag.Float64("acc-chunk", 1000, "max SUSQies per resting stack order")
	accTTL := flag.Duration("acc-ttl", 30*time.Minute, "resting stack orders expire after this")
	accEvery := flag.Int("acc-every", 3, "work the stack every N ticks (3 x 20s = once a minute)")
	accMax := flag.Float64("acc-max", 0.8, "stack at max prices may use at most this share of value; bigger files are scaled down")
	flag.Parse()

	loadDotEnv(".env")
	key := os.Getenv("SUSQ_API_KEY")
	slug := os.Getenv("SUSQ_TOURNAMENT_SLUG")
	if key == "" {
		fatal("SUSQ_API_KEY not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := susq.New(os.Getenv("SUSQ_BASE_URL"), key)

	switch *mode {
	case "tournaments":
		raw, err := s.TournamentsRaw(ctx)
		if err != nil {
			fatal(err.Error())
		}
		fmt.Println(string(raw))
		return
	case "markets":
		if slug == "" {
			fatal("SUSQ_TOURNAMENT_SLUG not set (run -mode tournaments)")
		}
		dumpMarkets(ctx, s, slug)
		return
	case "stage", "fire", "flatten":
		if slug == "" {
			fatal("SUSQ_TOURNAMENT_SLUG not set (run -mode tournaments)")
		}
		t, err := s.Tournament(ctx, slug)
		if err != nil {
			fatal(err.Error())
		}
		s.TournamentID = t.ID
		switch *mode {
		case "flatten":
			runFlatten(ctx, s, slug, *flatSlack, *outPath, *targetsPath)
		case "stage":
			runStage(ctx, s, *batchPath, t.MyBalance)
		case "fire":
			runFire(ctx, s, *batchPath, t.MyBalance, *confirm, *fireTTL, *jpath)
		}
		return
	case "scan", "dry", "live":
	default:
		fatal("unknown mode " + *mode)
	}
	if slug == "" {
		fatal("SUSQ_TOURNAMENT_SLUG not set (run -mode tournaments)")
	}

	rows, err := mapping.Load(*mapPath)
	if err != nil {
		fatal("mapping: " + err.Error())
	}
	if fr, err := mapping.LoadFair(*fairPath); err == nil {
		have := map[string]bool{}
		for _, r := range rows {
			have[r.ExchangeID] = true
		}
		for _, r := range fr {
			if have[r.ExchangeID] {
				fatal("fair.csv: exchange " + r.ExchangeID + " is also in mapping.csv; keep it in one file")
			}
			rows = append(rows, r)
		}
	} else if !os.IsNotExist(err) {
		fatal("fair.csv: " + err.Error())
	}
	var targets []strategy.Target
	var targetsMod time.Time
	if st, err := os.Stat(*targetsPath); err == nil {
		if targets, err = strategy.LoadTargets(*targetsPath); err != nil {
			fatal(err.Error())
		}
		targetsMod = st.ModTime()
	}
	j, err := journal.Open(*jpath, os.Getenv("ALERT_URL"))
	if err != nil {
		fatal(err.Error())
	}
	defer j.Close()
	j.SetHeartbeat(os.Getenv("HEARTBEAT_URL"))

	g := &risk.Guard{StopFile: "STOP", L: risk.Limits{
		MaxOrderNotional: *maxOrder, MaxExchangeNotional: *maxEx, MaxDeployedFrac: *maxDep,
		DailyLossFrac: *dayLoss, MinPrice: *minP, MaxPrice: *maxP}}

	var stopAt time.Time
	if *fvStop != "" {
		if stopAt, err = time.Parse(time.RFC3339, *fvStop); err != nil {
			fatal("-fv-stop: " + err.Error())
		}
	}
	eng := &strategy.Engine{S: s, K: kalshi.New(""), J: j, G: g, Map: rows, Cfg: strategy.Config{
		Mode: *mode, Slug: slug, Edge: *edge, MaxKalshiSpread: *kSpread, ArbEnabled: *arb,
		ArbEdge: *arbEdge, MaxActions: *maxActions, OrderTTL: *ttl,
		SuspectGap: *suspect, RepairTTL: *repairTTL, UnwindSlack: *unwindSlack,
		FVStopAt: stopAt, LagThresh: *lagThresh, LagMaxWait: *lagWait, DepthPerTick: *depthPer,
		FairEdge: *fairEdge, AccChunk: *accChunk, AccTTL: *accTTL, AccEvery: *accEvery, AccMaxFrac: *accMax}}
	eng.Targets = targets
	if *arb {
		if races, err := sim.LoadRaces(*racesPath); err == nil {
			eng.PairRels = pairRelationships(races)
		} else if !os.IsNotExist(err) {
			fatal("races.csv: " + err.Error())
		}
	}
	if *depthPer > 0 {
		f, empty, err := openAppend(*depthFile)
		if err != nil {
			fatal(err.Error())
		}
		defer f.Close()
		eng.DepthOut = strategy.NewDepthWriter(f, empty)
	}
	lb, lbEmpty, err := openAppend(*lbFile)
	if err != nil {
		fatal(err.Error())
	}
	defer lb.Close()
	lbw := newLeaderboardWriter(lb, lbEmpty)

	if *mode == "scan" {
		if err := eng.Tick(ctx); err != nil {
			fatal(err.Error())
		}
		return
	}

	live := *mode == "live"
	cancelAll := func(why string) {
		if !live {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.CancelAll(cctx); err != nil {
			j.Alert("cancel-all failed (" + why + "): " + err.Error())
			return
		}
		j.Event("cancel_all", map[string]any{"why": why})
	}

	// Clear anything a previous run left resting before trading again.
	if live {
		if t, err := s.Tournament(ctx, slug); err == nil {
			s.TournamentID = t.ID
			cancelAll("startup")
		}
	}

	j.Alert(fmt.Sprintf("started mode=%s mapped=%d stack targets=%d race pairs=%d", *mode, len(rows), len(targets), len(eng.PairRels)))
	dg := &digester{s: s, slug: slug, j: j, eng: eng, hour: *digestHour, known: "known_markets.txt"}
	errs := 0
	couldTrade := true
	lastScore, lastUniverse := time.Time{}, time.Time{}
	tk := time.NewTicker(*interval)
	defer tk.Stop()
	for {
		// Pick up edits to targets.csv without a restart.
		if st, err := os.Stat(*targetsPath); err == nil && st.ModTime() != targetsMod {
			targetsMod = st.ModTime()
			if ts, err := strategy.LoadTargets(*targetsPath); err != nil {
				j.Alert("targets.csv changed but didn't load, keeping the old stack: " + err.Error())
			} else {
				eng.Targets = ts
				j.Alert(fmt.Sprintf("stack reloaded: %d targets", len(ts)))
			}
		} else if os.IsNotExist(err) && len(eng.Targets) > 0 {
			eng.Targets = nil
			targetsMod = time.Time{}
			j.Alert("targets.csv removed: stack accumulation off (resting bids expire on their own)")
		}
		if time.Since(lastUniverse) > 6*time.Hour {
			if u, err := loadUniverse(ctx, s, slug); err == nil {
				eng.Universe = u
				lastUniverse = time.Now()
			} else {
				j.Event("universe_error", map[string]any{"err": err.Error()})
			}
		}
		if err := eng.Tick(ctx); err != nil {
			errs++
			j.Event("tick_error", map[string]any{"err": err.Error(), "consecutive": errs})
			if code, fatal := susq.IsFatal(err); fatal {
				if g.Halt("api " + code) {
					j.Alert("halted: " + code + ". Retrying won't fix this. Fix it (accept terms / new key), then restart.")
				}
			} else if errs == 5 {
				j.Alert("5 consecutive tick errors: " + err.Error())
			}
		} else {
			if errs >= 5 {
				j.Alert(fmt.Sprintf("recovered after %d errors", errs))
			}
			errs = 0
			j.Beat()
		}

		// Whenever trading gets blocked (STOP, daily loss, fatal), pull
		// anything resting so nothing fills while you're not looking.
		ok, why := g.CanTrade()
		if !ok && couldTrade {
			cancelAll(why)
		}
		couldTrade = ok

		if time.Since(lastScore) > time.Hour {
			if raw, err := s.SmartScore(ctx, slug); err == nil {
				j.Event("smart_score", map[string]any{"data": string(raw)})
			}
			if st, err := s.Leaderboard(ctx, slug, 100); err == nil {
				lbw.write(time.Now(), st)
			}
			lastScore = time.Now()
		}
		dg.maybeSend(ctx, time.Now())

		select {
		case <-ctx.Done():
			cancelAll("shutdown")
			j.Alert("stopped")
			return
		case <-tk.C:
		}
	}
}

// dumpMarkets writes every open tournament contract to markets_dump.csv so
// you can build mapping.csv without clicking through the UI.
func dumpMarkets(ctx context.Context, s *susq.Client, slug string) {
	t, err := s.Tournament(ctx, slug)
	if err != nil {
		fatal(err.Error())
	}
	s.TournamentID = t.ID
	ms, err := s.Markets(ctx)
	if err != nil {
		fatal(err.Error())
	}
	f, err := os.Create("markets_dump.csv")
	if err != nil {
		fatal(err.Error())
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"market_id", "exchange_id", "title", "option", "latest", "settlement", "composite", "categories", "settles_after_end"})
	n := 0
	var end time.Time
	if t.EndDate != nil {
		end = parseTime(*t.EndDate)
	}
	late := map[string]string{}
	for _, m := range ms {
		for _, x := range m.Exchanges {
			opt, lp, sd := "", "", ""
			if x.Option != nil {
				opt = *x.Option
			}
			if x.LatestPrice != nil {
				lp = fmt.Sprintf("%.3f", *x.LatestPrice)
			}
			if m.SettlementDate != nil {
				sd = *m.SettlementDate
			}
			after := ""
			if st := parseTime(sd); !st.IsZero() && !end.IsZero() && st.After(end) {
				after = "true"
				late[m.ID] = m.Title + " (" + sd + ")"
			}
			w.Write([]string{m.ID, x.ID, m.Title, opt, lp, sd, fmt.Sprint(m.IsComposite), strings.Join(m.Categories, "|"), after})
			n++
		}
	}
	w.Flush()
	fmt.Printf("%s: %d markets, %d contracts -> markets_dump.csv (balance %.0f)\n", t.Name, len(ms), n, t.MyBalance)
	if len(late) > 0 {
		// Runoffs, ranked-choice counts, slow states. Read these Info tabs
		// first: how they settle is where the uncrowded mispricing lives.
		fmt.Printf("\n%d markets settle after the tournament ends. Read their Info tabs:\n", len(late))
		for _, v := range late {
			fmt.Println("  " + v)
		}
	}
}

func parseTime(s string) time.Time {
	for _, f := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// openAppend opens path for appending and reports whether it was empty.
func openAppend(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, false, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false, err
	}
	return f, st.Size() == 0, nil
}

func loadUniverse(ctx context.Context, s *susq.Client, slug string) ([]strategy.Contract, error) {
	ms, err := s.Markets(ctx)
	if err != nil {
		return nil, err
	}
	var out []strategy.Contract
	for _, m := range ms {
		for _, x := range m.Exchanges {
			label := m.Title
			if x.Option != nil && *x.Option != "" {
				label += " | " + *x.Option
			}
			out = append(out, strategy.Contract{ID: x.ID, Label: label})
		}
	}
	return out, nil
}

type leaderboardWriter struct{ w *csv.Writer }

func newLeaderboardWriter(f *os.File, empty bool) *leaderboardWriter {
	w := csv.NewWriter(f)
	if empty {
		w.Write([]string{"ts", "rank", "profile_id", "username", "pnl", "trades", "volume", "roi", "my_rank", "total"})
		w.Flush()
	}
	return &leaderboardWriter{w: w}
}

// write appends one snapshot. Hourly snapshots let you infer what leaders
// hold later: match their pnl changes against price moves.
func (l *leaderboardWriter) write(now time.Time, st susq.Standing) {
	my := ""
	if st.MyRank != nil {
		my = fmt.Sprint(*st.MyRank)
	}
	ts := now.UTC().Format(time.RFC3339)
	for _, r := range st.Leaderboard {
		u := ""
		if r.Username != nil {
			u = *r.Username
		}
		l.w.Write([]string{ts, fmt.Sprint(r.Rank), r.ProfileID, u, fmt.Sprintf("%.2f", r.PnL),
			fmt.Sprint(r.TradesCount), fmt.Sprintf("%.0f", r.Volume), fmt.Sprintf("%.2f", r.ROI), my, fmt.Sprint(st.Total)})
	}
	l.w.Flush()
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
	fmt.Fprintln(os.Stderr, "sigcup:", msg)
	os.Exit(1)
}
