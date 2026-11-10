package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"sigcup/internal/journal"
	"sigcup/internal/strategy"
	"sigcup/internal/susq"
)

// digester sends one low-priority summary a day. Everything routine goes
// here instead of your phone's alert channel.
type digester struct {
	s     *susq.Client
	slug  string
	j     *journal.J
	eng   *strategy.Engine
	hour  int
	known string // file of market ids already seen

	sentDay   string
	lastValue float64
}

func (d *digester) maybeSend(ctx context.Context, now time.Time) {
	day := now.UTC().Format("2006-01-02")
	if d.hour < 0 || now.UTC().Hour() < d.hour || d.sentDay == day || d.eng.LastValue == 0 {
		return
	}
	d.sentDay = day
	d.j.Notify("sigcup daily", d.build(ctx))
}

func (d *digester) build(ctx context.Context) string {
	e := d.eng
	st := e.TakeStats()
	var b strings.Builder

	change := ""
	if d.lastValue > 0 {
		change = fmt.Sprintf(" (%+.1f%% since last digest)", 100*(e.LastValue/d.lastValue-1))
	}
	d.lastValue = e.LastValue
	fmt.Fprintf(&b, "Value %s%s. Cash %s, deployed %s.\n",
		k(e.LastValue), change, k(e.LastCash), k(e.LastDeployed))

	if lb, err := d.s.Leaderboard(ctx, d.slug, 3); err == nil {
		rank := "unranked"
		if lb.MyRank != nil {
			rank = fmt.Sprintf("%d of %d", *lb.MyRank, lb.Total)
		}
		lead := ""
		if len(lb.Leaderboard) > 0 {
			lead = fmt.Sprintf(", leader pnl %+.0f", lb.Leaderboard[0].PnL)
		}
		fmt.Fprintf(&b, "Rank %s%s.\n", rank, lead)
	}
	if raw, err := d.s.SmartScore(ctx, d.slug); err == nil {
		var ss []susq.SmartScoreEntry
		if json.Unmarshal(raw, &ss) == nil {
			for _, x := range ss {
				fmt.Fprintf(&b, "Smart score %s: %.2f, rank %d/%d, elite %v.\n",
					x.MarketType, x.SmartScoreDecayed, x.Rank, x.TotalTraders, x.IsElite)
			}
		}
	}

	fills := 0
	for _, n := range st.Filled {
		fills += n
	}
	fmt.Fprintf(&b, "Since last: %d candidates, %d gone before execution, %d placed, %d filled (fv %d, arb %d), %d lost the race.\n",
		st.Candidates, st.EdgeGone, st.Placed, fills, st.Filled["fv"], st.Filled["arb"], st.ZeroFill)
	if st.ArbRepaired+st.ArbUnwound+st.ArbStuck > 0 {
		fmt.Fprintf(&b, "Uneven arbs: %d repaired, %d unwound, %d stuck.\n", st.ArbRepaired, st.ArbUnwound, st.ArbStuck)
	}
	if len(e.Targets) > 0 {
		pg := e.Progress()
		pct := 0.0
		if pg.TargetShares > 0 {
			pct = 100 * float64(pg.HeldShares) / float64(pg.TargetShares)
		}
		fmt.Fprintf(&b, "Stack: %d of %d targets complete, %.0f%% of shares held, %d resting bids.",
			pg.Complete, pg.Targets, pct, pg.Resting)
		if pg.Scale < 1 {
			fmt.Fprintf(&b, " Scaled to %.0f%% (acc-max).", pg.Scale*100)
		}
		fmt.Fprintf(&b, " Since last: %d bids placed, %d repriced/cancelled, %.0f shares taken.\n", st.AccPlaced, st.AccCancels, st.AccTaken)
	}
	if st.FVConflict > 0 {
		fmt.Fprintf(&b, "Fair value skipped %d trades that went against the stack.\n", st.FVConflict)
	}
	if st.FVCutoff > 0 {
		b.WriteString("Fair-value taker is past its cutoff; only arb opens positions now.\n")
	}
	if lg := st.Lags(); lg.Followed+lg.Unfollowed > 0 {
		fmt.Fprintf(&b, "Lag: %d Kalshi moves, SUSQ followed %d (%d within one tick, median %s), never followed %d.\n",
			lg.Followed+lg.Unfollowed, lg.Followed, lg.WithinTick, lg.Median.Round(time.Second), lg.Unfollowed)
	}
	if dp := e.Depth(); dp.Sampled > 0 {
		fmt.Fprintf(&b, "Depth within 5 pts (%d books): median %s to buy, %s to sell; %d with no asks, %d with no bids.\n",
			dp.Sampled, k(dp.MedianAsk), k(dp.MedianBid), dp.EmptyAsk, dp.EmptyBid)
	}
	if cs, err := d.s.Collateral(ctx); err == nil && len(cs) > 0 {
		floor, adv := 0.0, 0.0
		for _, c := range cs {
			if c.GuaranteedPayoutFloor != nil {
				floor += *c.GuaranteedPayoutFloor
			}
			adv += c.OutstandingAdvance
		}
		fmt.Fprintf(&b, "ALL collateral: %d components, payout floor %s, advance %s.\n", len(cs), k(floor), k(adv))
	}
	if st.GapN > 0 {
		fmt.Fprintf(&b, "Avg SUSQ-Kalshi gap on mapped contracts: %.1f pts.\n", 100*st.GapSum/float64(st.GapN))
	}
	if st.CancelFails > 0 {
		fmt.Fprintf(&b, "Cancel failures: %d (orders expire on their own).\n", st.CancelFails)
	}
	if sus := e.Suspects(); len(sus) > 0 {
		fmt.Fprintf(&b, "Quarantined mappings: %s.\n", strings.Join(sus, "; "))
	}
	if nm := d.newMarkets(ctx); nm != "" {
		b.WriteString(nm)
	}
	return strings.TrimSpace(b.String())
}

// newMarkets diffs the open market list against known_markets.txt so new
// listings (candidates to map) show up without you checking the site.
func (d *digester) newMarkets(ctx context.Context) string {
	ms, err := d.s.Markets(ctx)
	if err != nil {
		return ""
	}
	seen := map[string]bool{}
	first := false
	if f, err := os.Open(d.known); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			seen[strings.TrimSpace(sc.Text())] = true
		}
		f.Close()
	} else {
		first = true
	}
	var fresh []string
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
		if !seen[m.ID] {
			fresh = append(fresh, m.Title)
		}
	}
	sort.Strings(ids)
	_ = os.WriteFile(d.known, []byte(strings.Join(ids, "\n")+"\n"), 0o644)
	if first {
		return fmt.Sprintf("Tracking %d open markets for new listings.\n", len(ms))
	}
	if len(fresh) == 0 {
		return ""
	}
	sort.Strings(fresh)
	if len(fresh) > 8 {
		fresh = append(fresh[:8], fmt.Sprintf("and %d more", len(fresh)-8))
	}
	return fmt.Sprintf("New markets (run -mode markets to map): %s.\n", strings.Join(fresh, "; "))
}

func k(x float64) string {
	if x >= 1000 || x <= -1000 {
		return fmt.Sprintf("%.1fk", x/1000)
	}
	return fmt.Sprintf("%.0f", x)
}
