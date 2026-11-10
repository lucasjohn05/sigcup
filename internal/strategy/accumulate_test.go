package strategy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sigcup/internal/journal"
	"sigcup/internal/kalshi"
	"sigcup/internal/mapping"
	"sigcup/internal/risk"
	"sigcup/internal/susq"
)

type accMock struct {
	mu       sync.Mutex
	book     map[string][2]float64 // YES bid, ask
	open     []map[string]any      // resting orders as the API reports them
	pos      []map[string]any
	placed   []map[string]any
	canceled []string
}

func (m *accMock) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		enc := json.NewEncoder(w)
		switch {
		case p == "/tournaments/cup":
			enc.Encode(map[string]any{"id": "t1", "myBalance": 100000})
		case p == "/tournaments/cup/portfolio/positions":
			enc.Encode(map[string]any{"positions": m.pos, "summary": map[string]any{"totalMarketValue": 0}})
		case p == "/relationships":
			enc.Encode(map[string]any{"data": []any{}, "pagination": map[string]any{"hasMore": false}})
		case p == "/exchanges/prices":
			var d []any
			for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
				b := m.book[id]
				q := map[string]any{"exchangeId": id}
				if b[0] > 0 {
					q["bestBid"] = b[0]
				}
				if b[1] > 0 {
					q["bestAsk"] = b[1]
				}
				d = append(d, q)
			}
			enc.Encode(map[string]any{"data": d})
		case p == "/orders" && r.Method == "GET":
			if r.URL.Query().Get("status") != "open" {
				t.Error("accumulate must list open orders")
			}
			enc.Encode(map[string]any{"data": m.open, "pagination": map[string]any{"hasMore": false}})
		case p == "/orders" && r.Method == "POST":
			var o map[string]any
			json.NewDecoder(r.Body).Decode(&o)
			m.placed = append(m.placed, o)
			enc.Encode(map[string]any{"orderId": 500 + len(m.placed), "exchangeId": o["exchangeId"], "open": true, "quantityTraded": 0})
		case strings.HasPrefix(p, "/orders/") && strings.HasSuffix(p, "/fills"):
			enc.Encode(map[string]any{"data": []any{map[string]any{"quantity": 100}}})
		case strings.HasPrefix(p, "/orders/") && r.Method == "DELETE":
			m.canceled = append(m.canceled, strings.TrimPrefix(p, "/orders/"))
			enc.Encode(map[string]any{"message": "ok"})
		default:
			w.WriteHeader(404)
			enc.Encode(map[string]any{"error": map[string]any{"code": "NOT_FOUND", "message": p}})
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func accEngine(t *testing.T, s *httptest.Server, targets []Target) *Engine {
	dir := t.TempDir()
	j, _ := journal.Open(filepath.Join(dir, "j.jsonl"), "")
	return &Engine{S: susq.New(s.URL+"/api/v1", "k"), K: kalshi.New("http://127.0.0.1:1"), J: j,
		G: &risk.Guard{StopFile: filepath.Join(dir, "STOP"), L: risk.Limits{MaxOrderNotional: 1000,
			MaxExchangeNotional: 5000, MaxDeployedFrac: 0.4, DailyLossFrac: 0.1, MinPrice: 0.03, MaxPrice: 0.97}},
		Targets: targets,
		Cfg: Config{Mode: "live", Slug: "cup", Edge: 0.05, FairEdge: 0.08, MaxKalshiSpread: 0.03, MaxActions: 5,
			OrderTTL: 15 * time.Second, SuspectGap: 0.15, RepairTTL: 5 * time.Second, UnwindSlack: 0.03,
			AccChunk: 1000, AccTTL: 30 * time.Minute, AccEvery: 1, AccMaxFrac: 0.8}}
}

func pf(o map[string]any, k string) float64 { v, _ := o[k].(float64); return v }

func TestAccumulateRestsOneTickAboveBid(t *testing.T) {
	m := &accMock{book: map[string][2]float64{"10": {0.30, 0.36}, "20": {0.40, 0.45}}}
	e := accEngine(t, m.server(t), []Target{
		{ExchangeID: "10", Dir: 1, Max: 0.40, Shares: 1000},
		{ExchangeID: "20", Dir: -1, Max: 0.65, Shares: 500}, // buy NO: NO bid 0.55, NO ask 0.60
	})
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.placed) != 2 {
		t.Fatalf("want 2 resting orders, got %v", m.placed)
	}
	a, b := m.placed[0], m.placed[1]
	if a["action"] != "buy" || pf(a, "price") != 0.305 || pf(a, "quantity") != 1000 || a["expirationDate"] == nil {
		t.Fatalf("YES target order wrong: %v", a)
	}
	// NO at 0.555 is sent as sell YES at 0.445.
	if b["action"] != "sell" || pf(b, "price") != 0.445 || pf(b, "quantity") != 500 {
		t.Fatalf("NO target order wrong: %v", b)
	}
	if p := e.Progress(); p.Resting != 2 || p.Complete != 0 {
		t.Fatalf("progress: %+v", p)
	}
}

func TestAccumulateKeepsBestBidAndStepsWhenOutbid(t *testing.T) {
	lim := 0.305
	m := &accMock{book: map[string][2]float64{"10": {0.305, 0.36}},
		open: []map[string]any{{"id": 77, "exchangeId": "10", "side": "yes", "action": "buy", "priceLimit": lim, "quantity": 1000, "open": true}}}
	e := accEngine(t, m.server(t), []Target{{ExchangeID: "10", Dir: 1, Max: 0.40, Shares: 1000}})
	e.Tick(context.Background())
	if len(m.placed) != 0 || len(m.canceled) != 0 {
		t.Fatalf("our order is the best bid; should not touch it: placed %v canceled %v", m.placed, m.canceled)
	}
	// Someone bids 0.32 over us: cancel, go to 0.325.
	m.book["10"] = [2]float64{0.32, 0.36}
	e.Tick(context.Background())
	if len(m.canceled) != 1 || m.canceled[0] != "77" || len(m.placed) != 1 || pf(m.placed[0], "price") != 0.325 {
		t.Fatalf("should reprice to 0.325: placed %v canceled %v", m.placed, m.canceled)
	}
}

func TestAccumulateCapsAtMaxAndNeverCrosses(t *testing.T) {
	m := &accMock{book: map[string][2]float64{"10": {0.45, 0.50}, "11": {0.20, 0.205}}}
	e := accEngine(t, m.server(t), []Target{
		{ExchangeID: "10", Dir: 1, Max: 0.40, Shares: 100},
		{ExchangeID: "11", Dir: 1, Max: 0.40, Shares: 100},
	})
	e.Tick(context.Background())
	if pf(m.placed[0], "price") != 0.40 {
		t.Fatalf("should cap at max 0.40: %v", m.placed[0])
	}
	// Bid 0.20 / ask 0.205: one tick above the bid would cross; stay at 0.20.
	if pf(m.placed[1], "price") != 0.2 {
		t.Fatalf("must not cross the ask: %v", m.placed[1])
	}
}

func TestAccumulateStopsAtTargetAndHonorsNOSpaceOrders(t *testing.T) {
	// We hold -500 (NO) on 20: the NO target of 500 is complete. The
	// resting order comes back as side no / buy at 0.555.
	m := &accMock{book: map[string][2]float64{"20": {0.40, 0.45}},
		pos:  []map[string]any{{"exchangeId": "20", "quantity": -500, "costBasis": 280}},
		open: []map[string]any{{"id": 9, "exchangeId": "20", "side": "no", "action": "buy", "priceLimit": 0.555, "quantity": 100, "open": true}}}
	e := accEngine(t, m.server(t), []Target{{ExchangeID: "20", Dir: -1, Max: 0.65, Shares: 500}})
	e.Tick(context.Background())
	if len(m.placed) != 0 || len(m.canceled) != 1 || m.canceled[0] != "9" {
		t.Fatalf("target reached: should cancel the NO-space order, placed %v canceled %v", m.placed, m.canceled)
	}
	if p := e.Progress(); p.Complete != 1 || p.HeldShares != 500 {
		t.Fatalf("progress: %+v", p)
	}
}

func TestAccumulateTakeBelow(t *testing.T) {
	m := &accMock{book: map[string][2]float64{"10": {0.30, 0.33}}}
	e := accEngine(t, m.server(t), []Target{{ExchangeID: "10", Dir: 1, Max: 0.40, Shares: 1000, TakeBelow: 0.34}})
	e.Tick(context.Background())
	if len(m.placed) < 1 || pf(m.placed[0], "price") != 0.33 {
		t.Fatalf("should take the 0.33 ask first: %v", m.placed)
	}
}

func TestAccumulateBlockedByStop(t *testing.T) {
	m := &accMock{book: map[string][2]float64{"10": {0.30, 0.36}},
		open: []map[string]any{{"id": 5, "exchangeId": "10", "side": "yes", "action": "buy", "priceLimit": 0.2, "quantity": 10, "open": true}}}
	e := accEngine(t, m.server(t), []Target{{ExchangeID: "10", Dir: 1, Max: 0.40, Shares: 1000}})
	os.WriteFile(e.G.StopFile, nil, 0o644)
	e.Tick(context.Background())
	if len(m.placed) != 0 || len(m.canceled) != 1 {
		t.Fatalf("STOP: no new orders and pull resting ones; placed %v canceled %v", m.placed, m.canceled)
	}
}

func TestFairValueNeverTradesAgainstStack(t *testing.T) {
	// fair.csv says 0.30 but SUSQ bids 0.45: fair value would sell. The
	// stack is long this contract, so it must not.
	m := &accMock{book: map[string][2]float64{"10": {0.45, 0.47}}}
	e := accEngine(t, m.server(t), []Target{{ExchangeID: "10", Dir: 1, Max: 0.40, Shares: 1}})
	e.Map = []mapping.Row{{ExchangeID: "10", Fair: 0.30, Verified: true}}
	e.Targets[0].Shares = 1
	m.pos = []map[string]any{{"exchangeId": "10", "quantity": 1}} // target done, so accumulate is quiet
	e.Tick(context.Background())
	if len(m.placed) != 0 {
		t.Fatalf("fair value traded against the stack: %v", m.placed)
	}
	if e.TakeStats().FVConflict != 1 {
		t.Fatal("conflict not counted")
	}
}

func TestLoadTargets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.csv")
	os.WriteFile(p, []byte("# c\nexchange_id,buy,max_price,target_shares,take_below,note\n10,yes,0.403,1000,,x\n20,NO,0.65,500,0.6,y\n"), 0o644)
	ts, err := LoadTargets(p)
	if err != nil {
		t.Fatal(err)
	}
	if ts[0].Max != 0.40 || ts[1].Dir != -1 || ts[1].TakeBelow != 0.6 {
		t.Fatalf("%+v", ts)
	}
	os.WriteFile(p, []byte(",yes,0.4,10,,from seed\n"), 0o644)
	if _, err := LoadTargets(p); err == nil || !strings.Contains(err.Error(), "no exchange_id") {
		t.Fatalf("rows without IDs must be rejected, got %v", err)
	}
}
