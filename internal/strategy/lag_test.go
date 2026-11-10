package strategy

import (
	"testing"
	"time"

	"sigcup/internal/susq"
)

func TestLagFollowed(t *testing.T) {
	l := lagTracker{Thresh: 0.03, MaxWait: time.Hour}
	t0 := time.Unix(0, 0)
	l.observe("x", t0, 0.50, 0.50)
	if r := l.observe("x", t0.Add(20*time.Second), 0.55, 0.50); r != nil {
		t.Fatal("should open, not close")
	}
	if r := l.observe("x", t0.Add(40*time.Second), 0.55, 0.51); r != nil {
		t.Fatal("1pt SUSQ move is not half of 5")
	}
	r := l.observe("x", t0.Add(60*time.Second), 0.55, 0.53)
	if r == nil || !r.Followed || r.Lag != 40*time.Second {
		t.Fatalf("want followed after 40s, got %+v", r)
	}
}

func TestLagWithinTickAndUnfollowed(t *testing.T) {
	l := lagTracker{Thresh: 0.03, MaxWait: time.Minute}
	t0 := time.Unix(0, 0)
	l.observe("a", t0, 0.50, 0.50)
	if r := l.observe("a", t0.Add(20*time.Second), 0.44, 0.45); r == nil || !r.Followed || r.Lag != 0 {
		t.Fatalf("SUSQ moved in the same tick, want lag 0, got %+v", r)
	}
	l.observe("b", t0, 0.30, 0.30)
	l.observe("b", t0.Add(time.Second), 0.36, 0.30)
	if r := l.observe("b", t0.Add(2*time.Minute), 0.36, 0.30); r == nil || r.Followed {
		t.Fatalf("want unfollowed, got %+v", r)
	}
}

func TestLagRevertDropped(t *testing.T) {
	l := lagTracker{Thresh: 0.03, MaxWait: time.Hour}
	t0 := time.Unix(0, 0)
	l.observe("x", t0, 0.50, 0.50)
	l.observe("x", t0.Add(time.Second), 0.55, 0.50)
	if r := l.observe("x", t0.Add(2*time.Second), 0.51, 0.50); r != nil {
		t.Fatalf("revert should drop silently, got %+v", r)
	}
	if len(l.open) != 0 {
		t.Fatal("event should be closed")
	}
}

func TestMeasureDepth(t *testing.T) {
	b := susq.Book{
		Bids: lv(0.40, 100, 0.37, 200, 0.30, 999),
		Asks: lv(0.42, 50, 0.47, 100, 0.50, 999),
	}
	r := measureDepth(b)
	if r.BidQty != 300 || r.AskQty != 150 || r.BidLevels != 2 || r.AskLevels != 2 {
		t.Fatalf("band cut wrong: %+v", r)
	}
	// Selling YES at 0.40 costs 0.60 per share, at 0.37 costs 0.63.
	if int(r.BidNotional+0.5) != 186 || int(r.AskNotional+0.5) != 68 {
		t.Fatalf("notional wrong: %+v", r)
	}
}

func lv(pq ...float64) []susq.Level {
	var out []susq.Level
	for i := 0; i+1 < len(pq); i += 2 {
		out = append(out, susq.Level{Price: pq[i], Quantity: pq[i+1]})
	}
	return out
}
