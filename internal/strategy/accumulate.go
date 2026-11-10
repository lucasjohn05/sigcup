package strategy

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"sigcup/internal/susq"
)

// Target is one position the stack wants, written by ./sim to targets.csv.
// Prices are in the space of the side being bought: buying NO at 0.62 means
// paying 0.62 per NO share.
type Target struct {
	ExchangeID string
	Dir        int     // +1 buy YES, -1 buy NO
	Max        float64 // never bid above this
	Shares     int     // how many shares of that side to own
	TakeBelow  float64 // take the ask outright at or below this (0 = only rest bids)
	Note       string
}

func LoadTargets(path string) ([]Target, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comment = '#'
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var out []Target
	seen := map[string]bool{}
	for i, rec := range recs {
		for len(rec) < 6 {
			rec = append(rec, "")
		}
		for j := range rec {
			rec[j] = strings.TrimSpace(rec[j])
		}
		if rec[0] == "exchange_id" || (rec[0] == "" && rec[1] == "") {
			continue
		}
		line := i + 1
		if rec[0] == "" {
			return nil, fmt.Errorf("%s line %d: no exchange_id (built from the offline seed?). Rebuild races.csv and rerun sim", path, line)
		}
		if seen[rec[0]] {
			return nil, fmt.Errorf("%s line %d: exchange %s listed twice", path, line, rec[0])
		}
		seen[rec[0]] = true
		t := Target{ExchangeID: rec[0], Note: rec[5]}
		switch strings.ToLower(rec[1]) {
		case "yes":
			t.Dir = 1
		case "no":
			t.Dir = -1
		default:
			return nil, fmt.Errorf("%s line %d: buy must be yes or no", path, line)
		}
		mx, err := strconv.ParseFloat(rec[2], 64)
		if err != nil || mx <= 0.005 || mx >= 1 {
			return nil, fmt.Errorf("%s line %d: max_price must be in (0.005, 1)", path, line)
		}
		t.Max = floorTick(mx)
		n, err := strconv.Atoi(rec[3])
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s line %d: target_shares must be a positive integer", path, line)
		}
		t.Shares = n
		if rec[4] != "" {
			tb, err := strconv.ParseFloat(rec[4], 64)
			if err != nil || tb <= 0 || tb > t.Max {
				return nil, fmt.Errorf("%s line %d: take_below must be in (0, max_price]", path, line)
			}
			t.TakeBelow = floorTick(tb)
		}
		out = append(out, t)
	}
	return out, nil
}

// AccProgress summarizes the stack for the digest.
type AccProgress struct {
	Targets, Complete, Resting int
	HeldShares, TargetShares   int
	Scale                      float64
}

// accumulate works the stack with patient resting bids: join or improve the
// best bid on your side by one tick, never above max_price, never crossing
// the ask. It only takes the ask when a row's take_below says so. Each
// resting order is at most AccChunk SUSQies and expires after AccTTL, so
// nothing big sits on the book if the bot dies.
func (e *Engine) accumulate(ctx context.Context, pos map[string]float64, value float64) {
	if len(e.Targets) == 0 {
		return
	}
	e.accTick++
	if e.Cfg.AccEvery > 1 && (e.accTick-1)%e.Cfg.AccEvery != 0 {
		return
	}
	// Scale the whole stack down if it would exceed AccMaxFrac of value.
	planned := 0.0
	for _, t := range e.Targets {
		planned += float64(t.Shares) * t.Max
	}
	scale := 1.0
	if e.Cfg.AccMaxFrac > 0 && value > 0 && planned > e.Cfg.AccMaxFrac*value {
		scale = e.Cfg.AccMaxFrac * value / planned
		e.J.AlertOnce("acc-scale", fmt.Sprintf("targets.csv plans %.0f at max prices, over %.0f%% of value; scaling every target to %.0f%%",
			planned, e.Cfg.AccMaxFrac*100, scale*100))
	}

	orders, err := e.S.OpenOrders(ctx)
	if err != nil {
		e.J.Event("acc_error", map[string]any{"step": "open_orders", "err": err.Error()})
		return
	}
	type ours struct {
		id    int64
		price float64 // side space
	}
	mine := map[string][]ours{}
	byID := map[string]Target{}
	ids := make([]string, 0, len(e.Targets))
	for _, t := range e.Targets {
		byID[t.ExchangeID] = t
		ids = append(ids, t.ExchangeID)
	}
	for _, o := range orders {
		t, ok := byID[o.ExchangeID]
		if !ok || o.ID == 0 {
			continue
		}
		act, p, ok := o.YesSpace()
		if !ok {
			continue
		}
		if (t.Dir == 1 && act == "buy") || (t.Dir == -1 && act == "sell") {
			sp := p
			if t.Dir == -1 {
				sp = 1 - p
			}
			mine[o.ExchangeID] = append(mine[o.ExchangeID], ours{o.ID, math.Round(sp*1000) / 1000})
		}
	}
	quotes, err := e.S.Prices(ctx, ids)
	if err != nil {
		e.J.Event("acc_error", map[string]any{"step": "prices", "err": err.Error()})
		return
	}
	canTrade, _ := e.G.CanTrade()
	live := e.Cfg.Mode == "live"
	prog := AccProgress{Targets: len(e.Targets), Scale: scale}

	for _, t := range e.Targets {
		want := int(math.Floor(float64(t.Shares) * scale))
		held := math.Max(0, float64(t.Dir)*pos[t.ExchangeID])
		prog.TargetShares += want
		prog.HeldShares += int(math.Min(held, float64(want)))
		remaining := float64(want) - held
		cur := mine[t.ExchangeID]
		cancelAll := func(why string) {
			for _, o := range cur {
				if live {
					if err := e.S.CancelOrder(ctx, o.id); err != nil {
						e.J.Event("acc_error", map[string]any{"step": "cancel", "order": o.id, "err": err.Error()})
					}
				}
				e.st.AccCancels++
				e.J.Event("acc_cancel", map[string]any{"exchange": t.ExchangeID, "order": o.id, "why": why})
			}
			cur = nil
		}
		if remaining < 1 {
			prog.Complete++
			cancelAll("target reached")
			continue
		}
		if !canTrade {
			cancelAll("trading blocked")
			continue
		}
		q := quotes[t.ExchangeID]
		bid, ask := sideBook(q, t.Dir)

		// Take outright only where the row says it's cheap enough.
		if t.TakeBelow > 0 && ask > 0 && ask <= t.TakeBelow {
			qty := int(math.Min(remaining, math.Floor(e.Cfg.AccChunk/ask)))
			if qty >= 1 {
				if !live {
					e.J.Event("would_trade", map[string]any{"kind": "acc_take", "exchange": t.ExchangeID, "price": ask, "qty": qty})
				} else {
					act, p := yesOrder(t.Dir, ask)
					got := e.place(ctx, t.ExchangeID, act, qty, p)
					remaining -= got
					if got > 0 {
						e.st.AccTaken += got
					}
				}
			}
		}
		if remaining < 1 {
			continue
		}

		// Rest a bid: keep ours if it's already the best bid, otherwise go
		// one tick above the best bid, capped at max and below the ask.
		var desired float64
		keep := false
		for _, o := range cur {
			if bid > 0 && math.Abs(o.price-bid) < 1e-9 && o.price <= t.Max {
				desired, keep = o.price, true
			}
		}
		if !keep {
			switch {
			case bid > 0:
				desired = bid + tick
			case ask > 0:
				desired = ask - 2*tick
			default:
				desired = t.Max - 0.02
			}
			desired = math.Min(desired, t.Max)
			if ask > 0 {
				desired = math.Min(desired, ask-tick)
			}
			desired = floorTick(desired)
		}
		if desired < 0.01 {
			cancelAll("no sensible bid")
			continue
		}
		if len(cur) == 1 && math.Abs(cur[0].price-desired) < 1e-9 {
			prog.Resting++
			continue
		}
		cancelAll("reprice")
		qty := int(math.Min(remaining, math.Floor(e.Cfg.AccChunk/desired)))
		if qty < 1 {
			continue
		}
		act, p := yesOrder(t.Dir, desired)
		info := map[string]any{"exchange": t.ExchangeID, "buy": map[int]string{1: "yes", -1: "no"}[t.Dir],
			"price": desired, "qty": qty, "held": held, "want": want, "note": t.Note}
		if !live {
			e.J.Event("would_rest", info)
			continue
		}
		pp := p
		r, err := e.S.PlaceOrder(ctx, susq.OrderReq{IdempotencyKey: newKey(), Leg: susq.Leg{ExchangeID: t.ExchangeID,
			Side: "yes", Action: act, Quantity: qty, Price: &pp,
			ExpirationDate: time.Now().Add(e.Cfg.AccTTL).UTC().Format(time.RFC3339)}})
		if err != nil {
			info["err"] = err.Error()
			e.J.Event("acc_error", info)
			if _, fatal := susq.IsFatal(err); fatal {
				e.accFatal = err
				return
			}
			continue
		}
		info["order"] = r.OrderID
		info["filled_now"] = r.QuantityTraded
		e.J.Event("acc_rest", info)
		e.st.AccPlaced++
		prog.Resting++
	}
	e.accProg = prog
}

// sideBook returns the best bid and ask for the side being bought.
func sideBook(q susq.Quote, dir int) (bid, ask float64) {
	if dir == 1 {
		if q.BestBid != nil {
			bid = *q.BestBid
		}
		if q.BestAsk != nil {
			ask = *q.BestAsk
		}
		return
	}
	if q.BestAsk != nil {
		bid = 1 - *q.BestAsk
	}
	if q.BestBid != nil {
		ask = 1 - *q.BestBid
	}
	return
}

// yesOrder converts "buy side at p" into a YES-space order.
func yesOrder(dir int, p float64) (string, float64) {
	if dir == 1 {
		return "buy", p
	}
	return "sell", clampTick(1 - p)
}

// Progress returns the last accumulate summary.
func (e *Engine) Progress() AccProgress { return e.accProg }

// TargetDir reports the stack direction for an exchange (0 = not a target).
func (e *Engine) TargetDir(id string) int {
	for _, t := range e.Targets {
		if t.ExchangeID == id {
			return t.Dir
		}
	}
	return 0
}
