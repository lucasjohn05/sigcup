package strategy

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"sigcup/internal/susq"
)

// DepthRow is one order book snapshot, measured within DepthBand of the
// touch on each side. Notional is what it costs to take that size (YES
// price for asks, NO price for bids).
type DepthRow struct {
	TS                       time.Time
	ExchangeID, Label        string
	BestBid, BestAsk         float64
	BidQty, AskQty           float64
	BidNotional, AskNotional float64
	BidLevels, AskLevels     int
}

const DepthBand = 0.05

func measureDepth(b susq.Book) DepthRow {
	r := DepthRow{BestBid: math.NaN(), BestAsk: math.NaN()}
	if len(b.Bids) > 0 {
		r.BestBid = b.Bids[0].Price
		for _, l := range b.Bids {
			if r.BestBid-l.Price > DepthBand+1e-9 {
				break
			}
			r.BidQty += l.Quantity
			r.BidNotional += l.Quantity * (1 - l.Price)
			r.BidLevels++
		}
	}
	if len(b.Asks) > 0 {
		r.BestAsk = b.Asks[0].Price
		for _, l := range b.Asks {
			if l.Price-r.BestAsk > DepthBand+1e-9 {
				break
			}
			r.AskQty += l.Quantity
			r.AskNotional += l.Quantity * l.Price
			r.AskLevels++
		}
	}
	return r
}

var depthHeader = []string{"ts", "exchange_id", "label", "best_bid", "best_ask",
	"bid_qty_5", "ask_qty_5", "bid_notional_5", "ask_notional_5", "bid_levels", "ask_levels"}

func f3(x float64) string {
	if math.IsNaN(x) {
		return ""
	}
	return fmt.Sprintf("%.3f", x)
}

func writeDepth(w *csv.Writer, r DepthRow) {
	w.Write([]string{r.TS.UTC().Format(time.RFC3339), r.ExchangeID, r.Label, f3(r.BestBid), f3(r.BestAsk),
		fmt.Sprintf("%.0f", r.BidQty), fmt.Sprintf("%.0f", r.AskQty),
		fmt.Sprintf("%.0f", r.BidNotional), fmt.Sprintf("%.0f", r.AskNotional),
		fmt.Sprint(r.BidLevels), fmt.Sprint(r.AskLevels)})
	w.Flush()
}

// NewDepthWriter wraps out and writes the header if the file is empty.
func NewDepthWriter(out io.Writer, empty bool) *csv.Writer {
	w := csv.NewWriter(out)
	if empty {
		w.Write(depthHeader)
		w.Flush()
	}
	return w
}

// sampleDepth snapshots DepthPerTick books from the universe, round robin,
// so the whole tournament gets covered every hour or so without bursting
// the rate limit.
func (e *Engine) sampleDepth(ctx context.Context) {
	if e.Cfg.DepthPerTick <= 0 || len(e.Universe) == 0 || e.DepthOut == nil {
		return
	}
	for i := 0; i < e.Cfg.DepthPerTick; i++ {
		u := e.Universe[e.depthCursor%len(e.Universe)]
		e.depthCursor++
		b, err := e.S.Orderbook(ctx, u.ID, 50)
		if err != nil {
			e.J.Event("depth_error", map[string]any{"exchange": u.ID, "err": err.Error()})
			return
		}
		r := measureDepth(b)
		r.TS, r.ExchangeID, r.Label = time.Now(), u.ID, u.Label
		writeDepth(e.DepthOut, r)
		if e.lastDepth == nil {
			e.lastDepth = map[string]DepthRow{}
		}
		e.lastDepth[u.ID] = r
	}
}

// DepthSummary is the median cost to take 5 points of book, over the latest
// snapshot of every exchange sampled so far.
type DepthSummary struct {
	Sampled              int
	MedianAsk, MedianBid float64
	EmptyAsk, EmptyBid   int
}

func (e *Engine) Depth() DepthSummary {
	var asks, bids []float64
	s := DepthSummary{Sampled: len(e.lastDepth)}
	for _, r := range e.lastDepth {
		asks = append(asks, r.AskNotional)
		bids = append(bids, r.BidNotional)
		if r.AskLevels == 0 {
			s.EmptyAsk++
		}
		if r.BidLevels == 0 {
			s.EmptyBid++
		}
	}
	s.MedianAsk, s.MedianBid = median(asks), median(bids)
	return s
}

func median(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	sort.Float64s(x)
	return x[len(x)/2]
}
