package main

// Pre-built order batches for the endgame.
//
//   -mode stage   -batch x.csv            what would fill right now, at what cost
//   -mode fire    -batch x.csv -confirm   send it (without -confirm, just stages)
//   -mode flatten -out flatten.csv        write your open positions as an exit batch
//
// Batch CSV, one order per row, all prices in YES space:
//
//   exchange_id,action,limit,quantity,notional,note
//
// action is buy or sell (YES). limit is the worst price you'll accept. Give
// quantity, or leave it blank and give notional (SUSQies to spend), which is
// converted at the limit. Rows fire in file order and cash is used up in
// that order, so put the most important rows first.

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"sigcup/internal/journal"
	"sigcup/internal/strategy"
	"sigcup/internal/susq"
)

type batchRow struct {
	Line       int
	ExchangeID string
	Action     string
	Limit      float64
	Qty        int
	Note       string
	Warn       string
}

// cost per share at price p: YES price to buy, NO price (1-p) to sell.
func shareCost(action string, p float64) float64 {
	if action == "buy" {
		return p
	}
	return 1 - p
}

func loadBatch(path string) ([]batchRow, error) {
	if path == "" {
		return nil, fmt.Errorf("-batch is required")
	}
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
	var out []batchRow
	for i, rec := range recs {
		for len(rec) < 6 {
			rec = append(rec, "")
		}
		for j := range rec {
			rec[j] = strings.TrimSpace(rec[j])
		}
		if rec[0] == "" || rec[0] == "exchange_id" {
			continue
		}
		row := batchRow{Line: i + 1, ExchangeID: rec[0], Action: strings.ToLower(rec[1]), Note: rec[5]}
		if row.Action != "buy" && row.Action != "sell" {
			return nil, fmt.Errorf("line %d: action must be buy or sell, got %q", i+1, rec[1])
		}
		lim, err := strconv.ParseFloat(rec[2], 64)
		if err != nil || lim <= 0 || lim >= 1 {
			return nil, fmt.Errorf("line %d: limit must be a price in (0,1), got %q", i+1, rec[2])
		}
		// Snap off-tick limits the safe way: never pay more / sell for less
		// than what you wrote.
		snapped := strategy.FloorTick(lim)
		if row.Action == "sell" {
			snapped = strategy.CeilTick(lim)
		}
		if math.Abs(snapped-lim) > 1e-9 {
			row.Warn = fmt.Sprintf("limit %.4f snapped to %.3f", lim, snapped)
		}
		row.Limit = snapped
		switch {
		case rec[3] != "":
			q, err := strconv.Atoi(rec[3])
			if err != nil || q < 1 {
				return nil, fmt.Errorf("line %d: bad quantity %q", i+1, rec[3])
			}
			row.Qty = q
		case rec[4] != "":
			n, err := strconv.ParseFloat(rec[4], 64)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("line %d: bad notional %q", i+1, rec[4])
			}
			row.Qty = int(math.Floor(n / shareCost(row.Action, row.Limit)))
			if row.Qty < 1 {
				return nil, fmt.Errorf("line %d: notional too small for one share", i+1)
			}
		default:
			return nil, fmt.Errorf("line %d: give quantity or notional", i+1)
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no orders", path)
	}
	return out, nil
}

// fillable walks the book: how much of the row fills at or better than its
// limit, and the average price.
func fillable(row batchRow, b susq.Book) (qty int, avg float64) {
	levels := b.Asks
	better := func(p float64) bool { return p <= row.Limit+1e-9 }
	if row.Action == "sell" {
		levels = b.Bids
		better = func(p float64) bool { return p >= row.Limit-1e-9 }
	}
	left, notional := float64(row.Qty), 0.0
	for _, l := range levels {
		if left <= 0 || !better(l.Price) {
			break
		}
		take := math.Min(left, l.Quantity)
		notional += take * l.Price
		left -= take
	}
	got := float64(row.Qty) - left
	if got <= 0 {
		return 0, 0
	}
	return int(got), notional / got
}

func runStage(ctx context.Context, s *susq.Client, path string, cash float64) {
	rows, err := loadBatch(path)
	if err != nil {
		fatal(err.Error())
	}
	stage(ctx, s, rows, cash)
}

func stage(ctx context.Context, s *susq.Client, rows []batchRow, cash float64) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "line\texchange\taction\tlimit\tqty\tfills now\tavg\tcost now\tnote")
	maxCost, nowCost := 0.0, 0.0
	for _, r := range rows {
		maxCost += float64(r.Qty) * shareCost(r.Action, r.Limit)
		b, err := s.Orderbook(ctx, r.ExchangeID, 100)
		if err != nil {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%.3f\t%d\tERR\t\t\t%s\n", r.Line, r.ExchangeID, r.Action, r.Limit, r.Qty, err)
			continue
		}
		q, avg := fillable(r, b)
		c := float64(q) * shareCost(r.Action, avg)
		nowCost += c
		note := r.Note
		if r.Warn != "" {
			note = r.Warn + "; " + note
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%.3f\t%d\t%d\t%.3f\t%.0f\t%s\n", r.Line, r.ExchangeID, r.Action, r.Limit, r.Qty, q, avg, c, note)
	}
	tw.Flush()
	fmt.Printf("\n%d orders. Cost if every row fills at its limit: %.0f. Fillable right now: %.0f. Cash: %.0f.\n",
		len(rows), maxCost, nowCost, cash)
	if maxCost > cash {
		fmt.Println("Rows past your cash get shrunk or rejected in file order. Put the important ones first.")
	}
}

func runFire(ctx context.Context, s *susq.Client, path string, cash float64, confirm bool, ttl time.Duration, jpath string) {
	rows, err := loadBatch(path)
	if err != nil {
		fatal(err.Error())
	}
	if !confirm {
		stage(ctx, s, rows, cash)
		fmt.Println("\nNot sent. Add -confirm to fire.")
		return
	}
	if _, err := os.Stat("STOP"); err == nil {
		fatal("STOP file present; delete it to fire")
	}
	j, err := journal.Open(jpath, os.Getenv("ALERT_URL"))
	if err != nil {
		fatal(err.Error())
	}
	defer j.Close()

	exp := time.Now().Add(ttl).UTC().Format(time.RFC3339)
	type outcome struct {
		row    batchRow
		filled float64
		err    string
	}
	var res []outcome
	for i := 0; i < len(rows); i += 50 {
		chunk := rows[i:min(i+50, len(rows))]
		legs := make([]susq.Leg, len(chunk))
		for k, r := range chunk {
			p := r.Limit
			legs[k] = susq.Leg{ExchangeID: r.ExchangeID, Side: "yes", Action: r.Action,
				Quantity: r.Qty, Price: &p, ExpirationDate: exp}
		}
		brs, err := s.Batch(ctx, fireKey(), legs)
		if err != nil {
			for _, r := range chunk {
				res = append(res, outcome{row: r, err: err.Error()})
			}
			continue
		}
		byIdx := map[int]susq.BatchResult{}
		for _, br := range brs {
			byIdx[br.Index] = br
		}
		for k, r := range chunk {
			br, ok := byIdx[k]
			switch {
			case !ok:
				res = append(res, outcome{row: r, err: "no result"})
			case !br.OK:
				res = append(res, outcome{row: r, err: fmt.Sprintf("status %d: %s", br.Status, string(br.Raw))})
			default:
				res = append(res, outcome{row: r, filled: settleFired(ctx, s, br.Data)})
			}
		}
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "line\texchange\taction\tlimit\twanted\tfilled\tnote")
	wanted, got := 0, 0.0
	for _, o := range res {
		wanted += o.row.Qty
		got += o.filled
		note := o.row.Note
		if o.err != "" {
			note = "ERROR " + o.err
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%.3f\t%d\t%.0f\t%s\n", o.row.Line, o.row.ExchangeID, o.row.Action, o.row.Limit, o.row.Qty, o.filled, note)
		j.Event("fired", map[string]any{"line": o.row.Line, "exchange": o.row.ExchangeID, "action": o.row.Action,
			"limit": o.row.Limit, "qty": o.row.Qty, "filled": o.filled, "err": o.err})
	}
	tw.Flush()
	fmt.Printf("\nFilled %.0f of %d shares. Remainders cancelled.\n", got, wanted)
}

// settleFired cancels any resting remainder and reads the real fill count.
func settleFired(ctx context.Context, s *susq.Client, r susq.OrderResp) float64 {
	if !r.Open || r.OrderID == nil {
		return r.QuantityTraded
	}
	_ = s.CancelOrder(ctx, *r.OrderID) // it expires on its own if this fails
	if f, err := s.OrderFilled(ctx, *r.OrderID); err == nil {
		return f
	}
	return r.QuantityTraded
}

func fireKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "sigcup-fire-" + hex.EncodeToString(b)
}

// ---- flatten ----

type flatRow struct {
	batchRow
	Skip string // non-empty: written commented out, with the reason
}

// flattenRows turns open positions into exit orders priced slack through
// the touch. YES holdings sell YES; NO holdings (negative quantity) buy YES
// back, since positions net. Positions that look like arb sets (two or more
// held contracts in one relationship) are written commented out, because
// selling one leg unhedges the rest.
func flattenRows(pos []susq.Position, books map[string]susq.Book, rels []susq.Relationship, slack float64) []flatRow {
	held := map[string]bool{}
	for _, p := range pos {
		if !p.Settled && math.Abs(p.Quantity) >= 1 {
			held[p.ExchangeID] = true
		}
	}
	arb := map[string]string{}
	for _, r := range rels {
		var in []string
		for _, n := range r.Nodes {
			if held[n.ExchangeID] {
				in = append(in, n.ExchangeID)
			}
		}
		if len(in) >= 2 {
			for _, id := range in {
				arb[id] = "likely arb set (relationship " + r.ID + ")"
			}
		}
	}
	var out []flatRow
	for _, p := range pos {
		if !held[p.ExchangeID] {
			continue
		}
		label := p.MarketTitle
		if p.Option != nil && *p.Option != "" {
			label += " | " + *p.Option
		}
		fr := flatRow{batchRow: batchRow{ExchangeID: p.ExchangeID, Qty: int(math.Abs(p.Quantity)), Note: label}}
		b := books[p.ExchangeID]
		if p.Quantity > 0 {
			fr.Action = "sell"
			if len(b.Bids) == 0 {
				fr.Skip = "no bids"
			} else {
				fr.Limit = strategy.CeilTick(b.Bids[0].Price - slack)
			}
		} else {
			fr.Action = "buy"
			if len(b.Asks) == 0 {
				fr.Skip = "no asks"
			} else {
				fr.Limit = strategy.FloorTick(b.Asks[0].Price + slack)
			}
		}
		if why, ok := arb[p.ExchangeID]; ok && fr.Skip == "" {
			fr.Skip = why
		}
		out = append(out, fr)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Skip == "" && out[j].Skip != "" })
	return out
}

func runFlatten(ctx context.Context, s *susq.Client, slug string, slack float64, outPath, targetsPath string) {
	pos, err := s.Positions(ctx, slug)
	if err != nil {
		fatal(err.Error())
	}
	rels, err := s.Relationships(ctx)
	if err != nil {
		fatal(err.Error())
	}
	books := map[string]susq.Book{}
	for _, p := range pos.Positions {
		if p.Settled || math.Abs(p.Quantity) < 1 {
			continue
		}
		if b, err := s.Orderbook(ctx, p.ExchangeID, 1); err == nil {
			books[p.ExchangeID] = b
		}
	}
	rows := flattenRows(pos.Positions, books, rels, slack)
	// Never exit the stack by accident: those rows are written commented out.
	if ts, err := strategy.LoadTargets(targetsPath); err == nil {
		stack := map[string]bool{}
		for _, t := range ts {
			stack[t.ExchangeID] = true
		}
		for i := range rows {
			if stack[rows[i].ExchangeID] && rows[i].Skip == "" {
				rows[i].Skip = "stack target in " + targetsPath
			}
		}
	}
	f, err := os.Create(outPath)
	if err != nil {
		fatal(err.Error())
	}
	defer f.Close()
	fmt.Fprintf(f, "# exit batch generated %s, %.3f through the touch.\n", time.Now().UTC().Format(time.RFC3339), slack)
	fmt.Fprintln(f, "# Review, then: -mode stage -batch "+outPath+", and -mode fire -batch "+outPath+" -confirm")
	fmt.Fprintln(f, "exchange_id,action,limit,quantity,notional,note")
	w := csv.NewWriter(f)
	active, skipped := 0, 0
	for _, r := range rows {
		rec := []string{r.ExchangeID, r.Action, fmt.Sprintf("%.3f", r.Limit), strconv.Itoa(r.Qty), "", r.Note}
		if r.Skip != "" {
			w.Flush()
			fmt.Fprintf(f, "# skipped (%s): %s\n", r.Skip, strings.Join(rec, ","))
			skipped++
			continue
		}
		w.Write(rec)
		active++
	}
	w.Flush()
	fmt.Printf("%s: %d exit orders, %d skipped (commented out, with reasons).\n", outPath, active, skipped)
}
