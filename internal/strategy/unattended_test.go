package strategy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// mock is a scriptable SUSQ + Kalshi pair. Exchanges 1,2 are an exhaustive
// ME pair with bids summing to 1.03 (sell-all arb). Exchange 3 is mapped to
// Kalshi K3.
type mock struct {
	mu        sync.Mutex
	book      map[string][2]float64
	kalshiMid float64
	arb       bool
	// legFill returns (filled, open) for a multi-leg leg by index.
	legFill func(i int, qty float64) (float64, bool)
	// orderFill returns filled qty for a single order.
	orderFill func(o map[string]any) float64
	orderErr  *[2]string // status code text, error code
	orders    []map[string]any
	fills     map[int64]float64
	nextID    int64
}

func (m *mock) servers(t *testing.T) (*httptest.Server, *httptest.Server) {
	m.fills = map[int64]float64{}
	m.nextID = 100
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		enc := json.NewEncoder(w)
		switch {
		case p == "/tournaments/cup":
			enc.Encode(map[string]any{"id": "t1", "slug": "cup", "myBalance": 100000})
		case p == "/tournaments/cup/portfolio/positions":
			enc.Encode(map[string]any{"positions": []any{}, "summary": map[string]any{"totalMarketValue": 0}})
		case p == "/relationships":
			var d []any
			if m.arb {
				d = append(d, map[string]any{"id": "r1", "type": "mutually_exclusive", "status": "active",
					"isExhaustive": true, "nodes": []any{
						map[string]any{"exchangeId": "1", "outcome": "YES", "role": "member"},
						map[string]any{"exchangeId": "2", "outcome": "YES", "role": "member"}}})
			}
			enc.Encode(map[string]any{"data": d, "pagination": map[string]any{"hasMore": false}})
		case p == "/exchanges/prices":
			var d []any
			for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
				b := m.book[id]
				d = append(d, map[string]any{"exchangeId": id, "bestBid": b[0], "bestAsk": b[1]})
			}
			enc.Encode(map[string]any{"data": d})
		case strings.HasSuffix(p, "/orderbook"):
			id := strings.Split(p, "/")[2]
			b := m.book[id]
			enc.Encode(map[string]any{"exchangeId": id,
				"bids": []any{map[string]any{"price": b[0], "quantity": 500}},
				"asks": []any{map[string]any{"price": b[1], "quantity": 500}}})
		case p == "/orders" && r.Method == "POST":
			var o map[string]any
			json.NewDecoder(r.Body).Decode(&o)
			m.orders = append(m.orders, o)
			if m.orderErr != nil {
				w.WriteHeader(403)
				enc.Encode(map[string]any{"error": map[string]any{"code": m.orderErr[1], "message": m.orderErr[0]}})
				return
			}
			f := o["quantity"].(float64)
			if m.orderFill != nil {
				f = m.orderFill(o)
			}
			m.nextID++
			m.fills[m.nextID] = f
			enc.Encode(map[string]any{"orderId": m.nextID, "exchangeId": o["exchangeId"],
				"open": f < o["quantity"].(float64), "quantityTraded": f, "totalCost": 10})
		case p == "/orders/multi-leg":
			var body struct{ Legs []map[string]any }
			json.NewDecoder(r.Body).Decode(&body)
			var res []any
			for i, l := range body.Legs {
				m.orders = append(m.orders, l)
				q := l["quantity"].(float64)
				f, open := q, false
				if m.legFill != nil {
					f, open = m.legFill(i, q)
				}
				m.nextID++
				m.fills[m.nextID] = f
				res = append(res, map[string]any{"index": i, "data": map[string]any{"orderId": m.nextID,
					"exchangeId": l["exchangeId"], "open": open, "quantityTraded": f, "totalCost": 50}})
			}
			enc.Encode(map[string]any{"results": res})
		case strings.HasSuffix(p, "/fills"):
			var id int64
			json.Unmarshal([]byte(strings.Split(p, "/")[2]), &id)
			// NO-side fills come back negative; the client must take abs.
			enc.Encode(map[string]any{"data": []any{map[string]any{"quantity": -m.fills[id]}}})
		case strings.HasPrefix(p, "/orders/") && r.Method == "DELETE":
			enc.Encode(map[string]any{"message": "ok"})
		default:
			w.WriteHeader(404)
			enc.Encode(map[string]any{"error": map[string]any{"code": "NOT_FOUND", "message": p}})
		}
	}))
	k := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bid := m.kalshiMid - 0.01
		json.NewEncoder(w).Encode(map[string]any{"markets": []any{map[string]any{"ticker": "K3",
			"status": "active", "yes_bid_dollars": ftoa(bid), "yes_ask_dollars": ftoa(bid + 0.02)}}})
	}))
	t.Cleanup(s.Close)
	t.Cleanup(k.Close)
	return s, k
}

func ftoa(f float64) string { b, _ := json.Marshal(f); return string(b) }

func newEngine(t *testing.T, s, k *httptest.Server, rows []mapping.Row) *Engine {
	dir := t.TempDir()
	j, _ := journal.Open(filepath.Join(dir, "j.jsonl"), "")
	return &Engine{
		S: susq.New(s.URL+"/api/v1", "k"), K: kalshi.New(k.URL), J: j,
		G: &risk.Guard{StopFile: filepath.Join(dir, "STOP"), L: risk.Limits{MaxOrderNotional: 1000,
			MaxExchangeNotional: 5000, MaxDeployedFrac: 0.6, DailyLossFrac: 0.1, MinPrice: 0.03, MaxPrice: 0.97}},
		Map: rows,
		Cfg: Config{Mode: "live", Slug: "cup", Edge: 0.04, MaxKalshiSpread: 0.04, ArbEnabled: true,
			ArbEdge: 0.02, MaxActions: 5, OrderTTL: 15 * time.Second, SuspectGap: 0.15,
			RepairTTL: 5 * time.Second, UnwindSlack: 0.03},
	}
}

func arbBook() map[string][2]float64 {
	return map[string][2]float64{"1": {0.40, 0.42}, "2": {0.63, 0.65}}
}

// Leg 2 only fills 200 of 500; the repair order completes the other 300.
func TestArbRepairCompletesShortLeg(t *testing.T) {
	m := &mock{book: arbBook(), arb: true, legFill: func(i int, q float64) (float64, bool) {
		if i == 1 {
			return 200, true
		}
		return q, false
	}}
	s, k := m.servers(t)
	e := newEngine(t, s, k, nil)
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e.TakeStats()
	if st.ArbRepaired != 1 || st.ArbStuck != 0 {
		t.Fatalf("want 1 repaired, got %+v", st)
	}
	last := m.orders[len(m.orders)-1]
	// Sell leg at 0.63 with 0.03 edge => may go down to 0.60.
	if last["exchangeId"] != "2" || last["action"] != "sell" || last["quantity"].(float64) != 300 || last["price"].(float64) != 0.6 {
		t.Fatalf("bad repair order %v", last)
	}
}

// Repair can't fill, so the excess on leg 1 gets bought back.
func TestArbUnwindsWhenRepairFails(t *testing.T) {
	m := &mock{book: arbBook(), arb: true, legFill: func(i int, q float64) (float64, bool) {
		if i == 1 {
			return 200, true
		}
		return q, false
	}}
	m.orderFill = func(o map[string]any) float64 {
		if o["exchangeId"] == "2" {
			return 0 // nobody left to sell to
		}
		return o["quantity"].(float64)
	}
	s, k := m.servers(t)
	e := newEngine(t, s, k, nil)
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e.TakeStats()
	if st.ArbUnwound != 1 || st.ArbStuck != 0 {
		t.Fatalf("want 1 unwound, got %+v", st)
	}
	last := m.orders[len(m.orders)-1]
	if last["exchangeId"] != "1" || last["action"] != "buy" || last["quantity"].(float64) != 300 || last["price"].(float64) != 0.43 {
		t.Fatalf("bad unwind order %v", last)
	}
}

// A 30-point gap quarantines the mapping; verified=true lets it trade.
func TestSuspectMappingQuarantined(t *testing.T) {
	m := &mock{book: map[string][2]float64{"3": {0.47, 0.50}}, kalshiMid: 0.80}
	s, k := m.servers(t)
	e := newEngine(t, s, k, []mapping.Row{{ExchangeID: "3", KalshiTicker: "K3", Note: "test"}})
	e.Cfg.ArbEnabled = false
	for i := 0; i < 2; i++ {
		if err := e.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.orders) != 0 || len(e.Suspects()) != 1 {
		t.Fatalf("want quarantine and no orders, got %d orders, suspects %v", len(m.orders), e.Suspects())
	}

	e2 := newEngine(t, s, k, []mapping.Row{{ExchangeID: "3", KalshiTicker: "K3", Verified: true}})
	e2.Cfg.ArbEnabled = false
	if err := e2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.orders) != 1 {
		t.Fatalf("verified mapping should trade, got %d orders", len(m.orders))
	}
}

// A normal-sized gap still trades and feeds the gap stat.
func TestNormalGapTrades(t *testing.T) {
	m := &mock{book: map[string][2]float64{"3": {0.47, 0.50}}, kalshiMid: 0.56}
	s, k := m.servers(t)
	e := newEngine(t, s, k, []mapping.Row{{ExchangeID: "3", KalshiTicker: "K3"}})
	e.Cfg.ArbEnabled = false
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e.TakeStats()
	if len(m.orders) != 1 || st.Filled["fv"] != 1 || st.GapN != 1 {
		t.Fatalf("want one fv fill, got orders=%d stats=%+v", len(m.orders), st)
	}
}

// TERMS_NOT_ACKNOWLEDGED must surface from Tick so main can halt and page.
func TestFatalOrderErrorPropagates(t *testing.T) {
	m := &mock{book: map[string][2]float64{"3": {0.47, 0.50}}, kalshiMid: 0.56,
		orderErr: &[2]string{"accept terms", "TERMS_NOT_ACKNOWLEDGED"}}
	s, k := m.servers(t)
	e := newEngine(t, s, k, []mapping.Row{{ExchangeID: "3", KalshiTicker: "K3"}})
	e.Cfg.ArbEnabled = false
	err := e.Tick(context.Background())
	code, fatal := susq.IsFatal(err)
	if !fatal || code != "TERMS_NOT_ACKNOWLEDGED" {
		t.Fatalf("want fatal TERMS_NOT_ACKNOWLEDGED, got %v", err)
	}
}

func TestTicks(t *testing.T) {
	cases := []struct{ in, floor, ceil float64 }{
		{0.633, 0.63, 0.635}, {0.63, 0.63, 0.63}, {0.001, 0.005, 0.005}, {0.999, 0.995, 0.995},
	}
	for _, c := range cases {
		if f := floorTick(c.in); f != c.floor {
			t.Errorf("floorTick(%v)=%v want %v", c.in, f, c.floor)
		}
		if f := ceilTick(c.in); f != c.ceil {
			t.Errorf("ceilTick(%v)=%v want %v", c.in, f, c.ceil)
		}
	}
}

// Pair arb from races.csv: no SIG relationships, but the bot's own Dem/GOP
// pair shows bids summing to 1.03 (someone pushed a side up). Sell both.
func TestPairArbSellBoth(t *testing.T) {
	yes := "YES"
	m := &mock{book: arbBook(), arb: false}
	s, k := m.servers(t)
	e := newEngine(t, s, k, nil)
	e.PairRels = []susq.Relationship{{ID: "pair-X", Type: "mutually_exclusive", Status: "active",
		Nodes: []susq.RelNode{{ExchangeID: "1", Outcome: &yes, Role: "member"}, {ExchangeID: "2", Outcome: &yes, Role: "member"}}}}
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.orders) != 2 || m.orders[0]["action"] != "sell" || m.orders[1]["action"] != "sell" {
		t.Fatalf("want both sides sold, got %v", m.orders)
	}
}

// Buy both only when the pair is marked exhaustive.
func TestPairArbBuyBothOnlyWhenExhaustive(t *testing.T) {
	yes := "YES"
	for _, exh := range []bool{false, true} {
		m := &mock{book: map[string][2]float64{"1": {0.40, 0.45}, "2": {0.48, 0.50}}} // asks sum 0.95
		s, k := m.servers(t)
		e := newEngine(t, s, k, nil)
		e.PairRels = []susq.Relationship{{ID: "pair-X", Type: "mutually_exclusive", Status: "active", IsExhaustive: exh,
			Nodes: []susq.RelNode{{ExchangeID: "1", Outcome: &yes, Role: "member"}, {ExchangeID: "2", Outcome: &yes, Role: "member"}}}}
		if err := e.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		bought := len(m.orders) == 2 && m.orders[0]["action"] == "buy"
		if bought != exh {
			t.Fatalf("exhaustive=%v: bought=%v orders=%v", exh, bought, m.orders)
		}
	}
}
