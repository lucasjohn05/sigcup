// Package strategy runs the October bankroll-building loop:
//
//  1. Fair-value taker: take SUSQ quotes that are far through Kalshi's mid
//     on contracts you mapped by hand.
//  2. Relationship arbitrage: Dutch-book linked SUSQ contracts
//     (mutually exclusive / complementary / implication) using executable
//     prices, placed atomically via multi-leg.
//
// Every order is a short-lived limit at the touch; any unfilled remainder is
// cancelled, so the bot never leaves stale quotes on the book.
//
// It is built to run unattended: uneven arb fills are repaired or unwound
// automatically, mappings that look wrong are quarantined, and routine
// activity goes into Stats for the daily digest instead of your phone.
package strategy

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"sigcup/internal/journal"
	"sigcup/internal/kalshi"
	"sigcup/internal/mapping"
	"sigcup/internal/risk"
	"sigcup/internal/susq"
)

type Config struct {
	Mode            string // scan | dry | live
	Slug            string
	Edge            float64 // min distance through Kalshi fair value
	MaxKalshiSpread float64 // ignore Kalshi books wider than this (thin = unreliable)
	ArbEnabled      bool
	ArbEdge         float64 // min guaranteed profit per set, in SUSQies per share
	MaxActions      int     // orders attempted per tick
	OrderTTL        time.Duration
	SuspectGap      float64       // SUSQ-vs-Kalshi gap that quarantines an unverified mapping
	RepairTTL       time.Duration // expiry for arb repair/unwind orders
	UnwindSlack     float64       // max per-share loss accepted to flatten a stuck arb leg
	FVStopAt        time.Time     // after this, no new fair-value trades (zero = never)
	LagThresh       float64       // Kalshi move that opens a lag event (0 = off)
	LagMaxWait      time.Duration // after this, a lag event counts as unfollowed
	DepthPerTick    int           // order books snapshotted per tick (0 = off)
	FairEdge        float64       // min edge vs fair.csv values (coarser than Kalshi)
	AccChunk        float64       // max SUSQies per resting accumulate order
	AccTTL          time.Duration // resting accumulate orders expire after this
	AccEvery        int           // run accumulate every N ticks
	AccMaxFrac      float64       // stack at max prices may use at most this share of value
}

// Contract is one tradable exchange, labeled for logs.
type Contract struct{ ID, Label string }

// Stats accumulates between digests. TakeStats returns and resets it.
type Stats struct {
	Ticks       int
	Candidates  int
	EdgeGone    int // edge disappeared between the price scan and the book re-check
	Placed      int
	ZeroFill    int // placed but nothing filled: someone got there first
	Filled      map[string]int
	FillCost    float64
	ArbRepaired int // missing leg completed
	ArbUnwound  int // excess leg flattened
	ArbStuck    int // still unhedged; you were paged
	CancelFails int
	GapSum      float64
	GapN        int
	FVCutoff    int // fair-value candidates skipped because of FVStopAt
	FVConflict  int // fair-value candidates skipped because they trade against the stack
	AccPlaced   int
	AccCancels  int
	AccTaken    float64
	LagFollowed []time.Duration
	LagMissed   int
}

func (s Stats) Lags() LagSummary { return summarizeLags(s.LagFollowed, s.LagMissed) }

type Engine struct {
	Cfg Config
	S   *susq.Client
	K   *kalshi.Client
	J   *journal.J
	G   *risk.Guard
	Map []mapping.Row

	rels        []susq.Relationship
	relsFetched time.Time
	// PairRels are relationships built from races.csv: in each race the Dem
	// and GOP contracts can't both pay. SIG hasn't published its own graph,
	// so these give arb the whole board.
	PairRels  []susq.Relationship
	posWarned bool
	suspect   map[string]string // exchange id -> reason; sticky until restart
	st        Stats

	// Universe is every open contract, refreshed by main; depth sampling
	// walks it. DepthOut receives the rows.
	Universe    []Contract
	DepthOut    *csv.Writer
	depthCursor int
	lastDepth   map[string]DepthRow
	lag         lagTracker

	// Targets is the stack from targets.csv; main reloads it on change.
	Targets  []Target
	accTick  int
	accProg  AccProgress
	accFatal error

	// Snapshot from the last tick, for the digest.
	LastValue, LastCash, LastDeployed float64
}

func (e *Engine) TakeStats() Stats {
	s := e.st
	e.st = Stats{}
	return s
}

// Suspects lists quarantined mappings.
func (e *Engine) Suspects() []string {
	var out []string
	for _, why := range e.suspect {
		out = append(out, why)
	}
	sort.Strings(out)
	return out
}

type leg struct {
	ExchangeID string
	Action     string // buy | sell, always YES space
	Price      float64
}

type candidate struct {
	Kind   string // fv | arb
	Edge   float64
	FV     float64 // fair value, fv candidates only
	Thresh float64 // edge required on re-check (fv only; 0 = Config.Edge)
	Legs   []leg
	Note   string
	RelID  string
}

// costPerShare: buying YES costs p; selling YES from flat is canonicalized by
// the engine to buying NO at 1-p. We size conservatively as if flat.
func costPerShare(l leg) float64 {
	if l.Action == "buy" {
		return l.Price
	}
	return 1 - l.Price
}

func (e *Engine) Tick(ctx context.Context) error {
	t, err := e.S.Tournament(ctx, e.Cfg.Slug)
	if err != nil {
		return fmt.Errorf("tournament: %w", err)
	}
	e.S.TournamentID = t.ID
	// Runs even when the tick has nothing to trade (day 1, no mapping yet).
	defer e.sampleDepth(ctx)

	exposure := map[string]float64{}
	signed := map[string]float64{}
	deployed, mv := 0.0, 0.0
	if pos, err := e.S.Positions(ctx, e.Cfg.Slug); err != nil {
		if !e.posWarned { // expected before the first order enrols you
			e.J.Event("positions_unavailable", map[string]any{"err": err.Error()})
			e.posWarned = true
		}
	} else {
		for _, p := range pos.Positions {
			if p.Settled {
				continue
			}
			exposure[p.ExchangeID] += math.Abs(p.CostBasis)
			signed[p.ExchangeID] += p.Quantity
			deployed += math.Abs(p.CostBasis)
		}
		mv = pos.Summary.TotalMarketValue
	}
	value := t.MyBalance + mv
	if msg := e.G.Update(time.Now(), value); msg != "" {
		e.J.Alert(msg)
	}

	if e.Cfg.ArbEnabled && time.Since(e.relsFetched) > 10*time.Minute {
		if rels, err := e.S.Relationships(ctx); err == nil {
			e.rels, e.relsFetched = rels, time.Now()
		} else {
			e.J.Event("relationships_error", map[string]any{"err": err.Error()})
		}
	}

	// One bulk price call covers every contract we might touch.
	idset := map[string]bool{}
	var tickers []string
	for _, m := range e.Map {
		idset[m.ExchangeID] = true
		if m.KalshiTicker != "" {
			tickers = append(tickers, m.KalshiTicker)
		}
	}
	for _, r := range e.allRels() {
		for _, n := range r.Nodes {
			idset[n.ExchangeID] = true
		}
	}
	for _, t := range e.Targets {
		idset[t.ExchangeID] = true
	}
	ids := make([]string, 0, len(idset))
	for id := range idset {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		e.J.Event("idle", map[string]any{"msg": "nothing mapped and no relationships"})
		return nil
	}
	quotes, err := e.S.Prices(ctx, ids)
	if err != nil {
		return fmt.Errorf("prices: %w", err)
	}

	var cands []candidate
	if len(e.Map) > 0 {
		kq := map[string]kalshi.Quote{}
		kerr := error(nil)
		if len(tickers) > 0 {
			kq, kerr = e.K.Quotes(ctx, tickers)
			if kerr != nil {
				e.J.Event("kalshi_error", map[string]any{"err": kerr.Error()})
				kq = map[string]kalshi.Quote{} // fair.csv rows still work
			}
		}
		cands = append(cands, e.fairValue(quotes, kq)...)
	}
	if e.Cfg.ArbEnabled {
		cands = append(cands, e.arbs(quotes)...)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Edge > cands[j].Edge })

	e.J.Event("tick", map[string]any{"value": round2(value), "cash": round2(t.MyBalance),
		"deployed": round2(deployed), "candidates": len(cands)})
	e.LastValue, e.LastCash, e.LastDeployed = value, t.MyBalance, deployed
	e.st.Ticks++
	e.st.Candidates += len(cands)

	// The stack goes first: it's the plan, fair value and arb are extras.
	e.accumulate(ctx, signed, value)
	if e.accFatal != nil {
		err := e.accFatal
		e.accFatal = nil
		return err
	}

	for i, c := range cands {
		if i >= e.Cfg.MaxActions {
			break
		}
		if err := e.execute(ctx, c, exposure, &deployed, value); err != nil {
			e.J.Event("execute_error", map[string]any{"err": err.Error(), "note": c.Note})
			if _, fatal := susq.IsFatal(err); fatal {
				return err // main halts and pages
			}
		}
	}
	return nil
}

func (e *Engine) fairValue(q map[string]susq.Quote, kq map[string]kalshi.Quote) []candidate {
	var out []candidate
	for _, m := range e.Map {
		s, ok2 := q[m.ExchangeID]
		if !ok2 {
			continue
		}
		var fv float64
		edge := e.Cfg.Edge
		if m.Fair > 0 {
			// fair.csv: your own number (e.g. a rating conversion). Coarser
			// than a live market, so it needs a bigger edge, and there is no
			// Kalshi move to time lag against.
			fv, edge = m.Fair, e.Cfg.FairEdge
		} else {
			k, ok := kq[m.KalshiTicker]
			if !ok {
				continue
			}
			if k.Ask-k.Bid > e.Cfg.MaxKalshiSpread {
				continue // thin Kalshi book: not a trustworthy fair value
			}
			fv = (k.Bid + k.Ask) / 2
		}
		if m.Invert {
			fv = 1 - fv
		}
		if m.Fair == 0 && s.BestBid != nil && s.BestAsk != nil {
			e.lag.Thresh, e.lag.MaxWait = e.Cfg.LagThresh, e.Cfg.LagMaxWait
			if r := e.lag.observe(m.ExchangeID, time.Now(), fv, (*s.BestBid+*s.BestAsk)/2); r != nil {
				e.J.Event("lag", map[string]any{"exchange": m.ExchangeID, "note": m.Note,
					"followed": r.Followed, "lag_s": r.Lag.Seconds(), "kalshi_move": round3(r.KMove)})
				if r.Followed {
					e.st.LagFollowed = append(e.st.LagFollowed, r.Lag)
				} else {
					e.st.LagMissed++
				}
			}
		}
		if !e.Cfg.FVStopAt.IsZero() && time.Now().After(e.Cfg.FVStopAt) {
			// Past the cutoff: still measure lag, but open nothing new. The
			// endgame gets the capital instead.
			e.st.FVCutoff++
			continue
		}
		if !m.Verified && e.Cfg.SuspectGap > 0 {
			if _, bad := e.suspect[m.ExchangeID]; bad {
				continue
			}
			if gap, ok := worstGap(s, fv); ok && gap >= e.Cfg.SuspectGap {
				e.quarantine(m, s, fv, gap)
				continue
			}
		}
		if s.BestBid != nil && s.BestAsk != nil {
			e.st.GapSum += math.Abs((*s.BestBid+*s.BestAsk)/2 - fv)
			e.st.GapN++
		}
		dir := e.TargetDir(m.ExchangeID)
		if s.BestAsk != nil && fv-*s.BestAsk >= edge {
			if dir == -1 { // the stack is short this contract
				e.st.FVConflict++
				continue
			}
			out = append(out, candidate{Kind: "fv", Edge: fv - *s.BestAsk, FV: fv, Thresh: edge,
				Legs: []leg{{m.ExchangeID, "buy", *s.BestAsk}},
				Note: fmt.Sprintf("%s buy@%.3f fv=%.3f %s", m.ExchangeID, *s.BestAsk, fv, m.Note)})
		}
		if s.BestBid != nil && *s.BestBid-fv >= edge {
			if dir == 1 { // the stack is long this contract
				e.st.FVConflict++
				continue
			}
			out = append(out, candidate{Kind: "fv", Edge: *s.BestBid - fv, FV: fv, Thresh: edge,
				Legs: []leg{{m.ExchangeID, "sell", *s.BestBid}},
				Note: fmt.Sprintf("%s sell@%.3f fv=%.3f %s", m.ExchangeID, *s.BestBid, fv, m.Note)})
		}
	}
	return out
}

// worstGap is how far the SUSQ book sits from fair value: the mid when both
// sides exist, else whichever side we'd hit.
func worstGap(s susq.Quote, fv float64) (float64, bool) {
	switch {
	case s.BestBid != nil && s.BestAsk != nil:
		return math.Abs((*s.BestBid+*s.BestAsk)/2 - fv), true
	case s.BestAsk != nil:
		return fv - *s.BestAsk, true
	case s.BestBid != nil:
		return *s.BestBid - fv, true
	}
	return 0, false
}

// quarantine stops trading a mapping whose gap is too large to trust. A gap
// that size is more often a wrong mapping (inverted, different resolution
// rules) than free money, and the bot would keep buying into it.
func (e *Engine) quarantine(m mapping.Row, s susq.Quote, fv, gap float64) {
	if e.suspect == nil {
		e.suspect = map[string]string{}
	}
	why := fmt.Sprintf("%s (%s, Kalshi %s) gap %.0f pts", m.ExchangeID, m.Note, m.KalshiTicker, gap*100)
	e.suspect[m.ExchangeID] = why
	e.J.Event("mapping_quarantined", map[string]any{"exchange": m.ExchangeID, "kalshi": m.KalshiTicker,
		"fv": round3(fv), "gap": round3(gap), "note": m.Note})
	e.J.AlertOnce("suspect:"+m.ExchangeID, "mapping quarantined: "+why+
		". Check both Info tabs. If the gap is real, set verified=true in mapping.csv and restart.")
}

func yesOutcome(n susq.RelNode) bool {
	return n.Outcome == nil || strings.EqualFold(*n.Outcome, "yes")
}

func (e *Engine) allRels() []susq.Relationship {
	if len(e.PairRels) == 0 {
		return e.rels
	}
	out := make([]susq.Relationship, 0, len(e.rels)+len(e.PairRels))
	return append(append(out, e.rels...), e.PairRels...)
}

func (e *Engine) arbs(q map[string]susq.Quote) []candidate {
	var out []candidate
	for _, r := range e.allRels() {
		if r.Status != "" && r.Status != "active" {
			continue
		}
		ok := true
		for _, n := range r.Nodes {
			if !yesOutcome(n) {
				ok = false
			}
		}
		if !ok {
			continue
		}
		label := r.Type
		if r.Label != nil {
			label = *r.Label
		}
		switch r.Type {
		case "mutually_exclusive", "complementary":
			var members []string
			for _, n := range r.Nodes {
				if n.Role == "member" || n.Role == "" {
					members = append(members, n.ExchangeID)
				}
			}
			if len(members) < 2 || len(members) > 10 {
				continue
			}
			sumBid, sumAsk, haveBids, haveAsks := 0.0, 0.0, true, true
			var sells, buys []leg
			for _, id := range members {
				s, ok := q[id]
				if !ok || s.BestBid == nil {
					haveBids = false
				} else {
					sumBid += *s.BestBid
					sells = append(sells, leg{id, "sell", *s.BestBid})
				}
				if !ok || s.BestAsk == nil {
					haveAsks = false
				} else {
					sumAsk += *s.BestAsk
					buys = append(buys, leg{id, "buy", *s.BestAsk})
				}
			}
			// At most one member can be YES, so selling one of each pays
			// at least n-1 on cost n-sumBid.
			if haveBids && sumBid-1 >= e.Cfg.ArbEdge {
				out = append(out, candidate{Kind: "arb", Edge: sumBid - 1, Legs: sells, RelID: r.ID,
					Note: fmt.Sprintf("ME sell-all sumBid=%.3f %s", sumBid, label)})
			}
			exhaustive := r.IsExhaustive || r.Type == "complementary"
			if exhaustive && haveAsks && 1-sumAsk >= e.Cfg.ArbEdge {
				out = append(out, candidate{Kind: "arb", Edge: 1 - sumAsk, Legs: buys, RelID: r.ID,
					Note: fmt.Sprintf("ME buy-all sumAsk=%.3f %s", sumAsk, label)})
			}
		case "implication":
			var ante, cons []string
			for _, n := range r.Nodes {
				switch n.Role {
				case "antecedent":
					ante = append(ante, n.ExchangeID)
				case "consequent":
					cons = append(cons, n.ExchangeID)
				}
			}
			if len(ante) != 1 || len(cons) != 1 {
				continue
			}
			a, okA := q[ante[0]]
			b, okB := q[cons[0]]
			if !okA || !okB || a.BestBid == nil || b.BestAsk == nil {
				continue
			}
			// A implies B, so P(A) <= P(B). Sell A, buy B: payout >= 1.
			if *a.BestBid-*b.BestAsk >= e.Cfg.ArbEdge {
				out = append(out, candidate{Kind: "arb", Edge: *a.BestBid - *b.BestAsk, RelID: r.ID,
					Legs: []leg{{ante[0], "sell", *a.BestBid}, {cons[0], "buy", *b.BestAsk}},
					Note: fmt.Sprintf("IMPL bidA=%.3f askB=%.3f %s", *a.BestBid, *b.BestAsk, label)})
			}
		}
	}
	return out
}

// execute re-checks each leg against the live book, sizes, and trades.
func (e *Engine) execute(ctx context.Context, c candidate, exposure map[string]float64, deployed *float64, value float64) error {
	minQty := math.Inf(1)
	costSet, edge := 0.0, 0.0
	legs := make([]leg, len(c.Legs))
	for i, l := range c.Legs {
		b, err := e.S.Orderbook(ctx, l.ExchangeID, 1)
		if err != nil {
			return err
		}
		var top []susq.Level
		if l.Action == "buy" {
			top = b.Asks
		} else {
			top = b.Bids
		}
		if len(top) == 0 {
			e.st.EdgeGone++
			return nil
		}
		l.Price = top[0].Price
		if !e.G.PriceOK(l.Price) {
			e.J.Event("skip_price_band", map[string]any{"note": c.Note, "price": l.Price})
			return nil
		}
		minQty = math.Min(minQty, top[0].Quantity)
		costSet += costPerShare(l)
		legs[i] = l
	}
	// Recompute edge on confirmed prices.
	switch {
	case c.Kind == "fv":
		if legs[0].Action == "buy" {
			edge = c.FV - legs[0].Price
		} else {
			edge = legs[0].Price - c.FV
		}
	default:
		n := float64(len(legs))
		minPayout := 1.0
		if legs[0].Action == "sell" && len(legs) > 1 && legs[len(legs)-1].Action == "sell" {
			minPayout = n - 1 // ME sell-all
		}
		edge = minPayout - costSet
	}
	thresh := e.Cfg.Edge
	if c.Thresh > 0 {
		thresh = c.Thresh
	}
	if c.Kind == "arb" {
		thresh = e.Cfg.ArbEdge
	}
	if edge < thresh {
		e.st.EdgeGone++
		return nil
	}
	exp := 0.0
	if c.Kind == "fv" {
		exp = exposure[legs[0].ExchangeID]
	}
	qty := e.G.Size(costSet, minQty, exp, *deployed, value)
	if qty < 1 {
		return nil
	}
	info := map[string]any{"kind": c.Kind, "note": c.Note, "edge": round3(edge), "qty": qty,
		"cost": round2(costSet * float64(qty)), "legs": legs}

	if e.Cfg.Mode != "live" {
		e.J.Event("would_trade", info)
		return nil
	}
	if ok, why := e.G.CanTrade(); !ok {
		info["blocked"] = why
		e.J.Event("blocked", info)
		return nil
	}

	exp8601 := time.Now().Add(e.Cfg.OrderTTL).UTC().Format(time.RFC3339)
	reqLegs := make([]susq.Leg, len(legs))
	for i, l := range legs {
		p := l.Price
		reqLegs[i] = susq.Leg{ExchangeID: l.ExchangeID, Side: "yes", Action: l.Action,
			Quantity: qty, Price: &p, ExpirationDate: exp8601}
	}
	var resps []susq.OrderResp
	if len(reqLegs) == 1 {
		r, err := e.S.PlaceOrder(ctx, susq.OrderReq{Leg: reqLegs[0], IdempotencyKey: newKey()})
		if err != nil {
			return err
		}
		resps = []susq.OrderResp{r}
	} else {
		rs, err := e.S.MultiLeg(ctx, newKey(), reqLegs)
		if err != nil {
			return err
		}
		resps = rs
	}

	e.st.Placed++
	filled := make([]float64, len(resps))
	cost := 0.0
	for i, r := range resps {
		filled[i] = e.settle(ctx, r)
		cost += r.TotalCost
		*deployed += r.TotalCost
		exposure[r.ExchangeID] += r.TotalCost
	}
	info["filled"] = filled
	e.J.Event("traded", info)

	total, hi, lo := 0.0, 0.0, math.Inf(1)
	for _, f := range filled {
		total += f
		hi, lo = math.Max(hi, f), math.Min(lo, f)
	}
	if total == 0 {
		e.st.ZeroFill++
		return nil
	}
	if e.st.Filled == nil {
		e.st.Filled = map[string]int{}
	}
	e.st.Filled[c.Kind]++
	e.st.FillCost += cost
	if c.Kind == "arb" && hi != lo {
		e.repairArb(ctx, c, legs, filled, edge)
	}
	return nil
}

// settle cancels any resting remainder and returns the filled quantity. The
// placement response can miss fills that land before the cancel, so it
// re-reads the order's fills when there was a remainder.
func (e *Engine) settle(ctx context.Context, r susq.OrderResp) float64 {
	if !r.Open || r.OrderID == nil {
		return r.QuantityTraded
	}
	if err := e.S.CancelOrder(ctx, *r.OrderID); err != nil {
		// Not a page: every order carries an expirationDate, so it dies anyway.
		e.st.CancelFails++
		e.J.Event("cancel_failed", map[string]any{"order": *r.OrderID, "err": err.Error()})
	}
	if f, err := e.S.OrderFilled(ctx, *r.OrderID); err == nil {
		return f
	}
	return r.QuantityTraded
}

// repairArb handles legs that filled unevenly. First it tries to complete the
// short legs, spending at most the arb's edge (so the set is still no worse
// than break-even). Whatever is still uneven gets flattened back to the
// smallest leg, accepting up to UnwindSlack per share. You're paged only if
// both fail.
func (e *Engine) repairArb(ctx context.Context, c candidate, legs []leg, filled []float64, edge float64) {
	before := append([]float64(nil), filled...)
	hi := 0.0
	var short []int
	for _, f := range filled {
		hi = math.Max(hi, f)
	}
	for i, f := range filled {
		if f < hi {
			short = append(short, i)
		}
	}
	slack := edge / float64(len(short))
	for _, i := range short {
		need := int(math.Round(hi - filled[i]))
		if need < 1 {
			continue
		}
		l := legs[i]
		p := floorTick(l.Price + slack)
		if l.Action == "sell" {
			p = ceilTick(l.Price - slack)
		}
		filled[i] += e.place(ctx, l.ExchangeID, l.Action, need, p)
	}
	if even(filled) {
		e.st.ArbRepaired++
		e.J.Event("arb_repaired", map[string]any{"note": c.Note, "before": before, "after": filled})
		return
	}

	lo := math.Inf(1)
	for _, f := range filled {
		lo = math.Min(lo, f)
	}
	for i, f := range filled {
		excess := int(math.Round(f - lo))
		if excess < 1 {
			continue
		}
		l := legs[i]
		// Undo the leg: sell back what we bought, or buy back what we sold.
		opp, p := "sell", ceilTick(l.Price-e.Cfg.UnwindSlack)
		if l.Action == "sell" {
			opp, p = "buy", floorTick(l.Price+e.Cfg.UnwindSlack)
		}
		filled[i] -= e.place(ctx, l.ExchangeID, opp, excess, p)
	}
	if even(filled) {
		e.st.ArbUnwound++
		e.J.Event("arb_unwound", map[string]any{"note": c.Note, "before": before, "after": filled})
		return
	}
	e.st.ArbStuck++
	e.J.Event("arb_stuck", map[string]any{"note": c.Note, "before": before, "after": filled, "legs": legs})
	e.J.Alert(fmt.Sprintf("arb still unhedged after repair+unwind: fills %v | %s", filled, c.Note))
}

// place sends one short-lived limit order and returns what filled.
func (e *Engine) place(ctx context.Context, id, action string, qty int, price float64) float64 {
	p := price
	r, err := e.S.PlaceOrder(ctx, susq.OrderReq{IdempotencyKey: newKey(), Leg: susq.Leg{
		ExchangeID: id, Side: "yes", Action: action, Quantity: qty, Price: &p,
		ExpirationDate: time.Now().Add(e.Cfg.RepairTTL).UTC().Format(time.RFC3339)}})
	if err != nil {
		e.J.Event("repair_order_error", map[string]any{"exchange": id, "action": action,
			"qty": qty, "price": p, "err": err.Error()})
		return 0
	}
	return e.settle(ctx, r)
}

func even(f []float64) bool {
	for _, x := range f {
		if math.Abs(x-f[0]) >= 1 {
			return false
		}
	}
	return true
}

// Prices must sit on the 0.005 tick in [0.005, 0.995] or the API rejects them.
const tick = 0.005

func clampTick(x float64) float64 {
	x = math.Round(x*1000) / 1000
	return math.Max(tick, math.Min(1-tick, x))
}

// FloorTick and CeilTick snap a price to the tick, conservatively for buys
// (floor) and sells (ceil).
func FloorTick(x float64) float64 { return floorTick(x) }
func CeilTick(x float64) float64  { return ceilTick(x) }

func floorTick(x float64) float64 { return clampTick(math.Floor(x/tick+1e-9) * tick) }
func ceilTick(x float64) float64  { return clampTick(math.Ceil(x/tick-1e-9) * tick) }

func newKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "sigcup-" + hex.EncodeToString(b)
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
