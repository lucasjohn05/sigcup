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

// Fake SUSQ + Kalshi: exchanges 1,2 form an exhaustive mutually exclusive
// pair whose bids sum to 1.03 (sell-all arb); exchange 3 is mapped to a
// Kalshi ticker with fv 0.56 while SUSQ offers at 0.50 (fv buy).
func TestTickLive(t *testing.T) {
	book := map[string][2]float64{"1": {0.40, 0.42}, "2": {0.63, 0.65}, "3": {0.47, 0.50}}
	var mu sync.Mutex
	var orders []map[string]any
	susqSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		enc := json.NewEncoder(w)
		switch {
		case p == "/tournaments/cup":
			enc.Encode(map[string]any{"id": "t1", "slug": "cup", "myBalance": 100000})
		case p == "/tournaments/cup/portfolio/positions":
			enc.Encode(map[string]any{"positions": []any{}, "summary": map[string]any{"totalMarketValue": 0}})
		case p == "/relationships":
			enc.Encode(map[string]any{"data": []any{map[string]any{"id": "r1", "type": "mutually_exclusive",
				"status": "active", "isExhaustive": true, "nodes": []any{
					map[string]any{"exchangeId": "1", "outcome": "YES", "role": "member"},
					map[string]any{"exchangeId": "2", "outcome": "YES", "role": "member"}}}},
				"pagination": map[string]any{"hasMore": false}})
		case p == "/exchanges/prices":
			var d []any
			for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
				b := book[id]
				d = append(d, map[string]any{"exchangeId": id, "bestBid": b[0], "bestAsk": b[1]})
			}
			enc.Encode(map[string]any{"data": d})
		case strings.HasSuffix(p, "/orderbook"):
			id := strings.Split(p, "/")[2]
			b := book[id]
			enc.Encode(map[string]any{"exchangeId": id,
				"bids": []any{map[string]any{"price": b[0], "quantity": 500}},
				"asks": []any{map[string]any{"price": b[1], "quantity": 500}}})
		case p == "/orders" && r.Method == "POST":
			var o map[string]any
			json.NewDecoder(r.Body).Decode(&o)
			mu.Lock()
			orders = append(orders, o)
			mu.Unlock()
			enc.Encode(map[string]any{"orderId": 1, "exchangeId": o["exchangeId"], "open": false,
				"quantityTraded": o["quantity"], "totalCost": 100})
		case p == "/orders/multi-leg":
			var body struct{ Legs []map[string]any }
			json.NewDecoder(r.Body).Decode(&body)
			var res []any
			for i, l := range body.Legs {
				mu.Lock()
				orders = append(orders, l)
				mu.Unlock()
				res = append(res, map[string]any{"index": i, "data": map[string]any{"orderId": 10 + i,
					"exchangeId": l["exchangeId"], "open": false, "quantityTraded": l["quantity"], "totalCost": 50}})
			}
			enc.Encode(map[string]any{"results": res})
		default:
			w.WriteHeader(404)
			enc.Encode(map[string]any{"error": map[string]any{"code": "NOT_FOUND", "message": p}})
		}
	}))
	defer susqSrv.Close()
	kSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"markets": []any{map[string]any{"ticker": "K3",
			"yes_bid_dollars": "0.5500", "yes_ask_dollars": "0.5700"}}})
	}))
	defer kSrv.Close()

	dir := t.TempDir()
	j, _ := journal.Open(filepath.Join(dir, "j.jsonl"), "")
	e := &Engine{
		S: susq.New(susqSrv.URL+"/api/v1", "k"), K: kalshi.New(kSrv.URL), J: j,
		G: &risk.Guard{StopFile: filepath.Join(dir, "STOP"), L: risk.Limits{MaxOrderNotional: 1000,
			MaxExchangeNotional: 5000, MaxDeployedFrac: 0.6, DailyLossFrac: 0.1, MinPrice: 0.03, MaxPrice: 0.97}},
		Map: []mapping.Row{{ExchangeID: "3", KalshiTicker: "K3"}},
		Cfg: Config{Mode: "live", Slug: "cup", Edge: 0.04, MaxKalshiSpread: 0.04, ArbEnabled: true,
			ArbEdge: 0.02, MaxActions: 5, OrderTTL: 15 * time.Second,
			SuspectGap: 0.15, RepairTTL: 5 * time.Second, UnwindSlack: 0.03},
	}
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(orders) != 3 {
		t.Fatalf("want 3 orders (1 fv + 2 arb legs), got %d: %v", len(orders), orders)
	}
	for _, o := range orders {
		t.Logf("order %v", o)
	}
	// STOP file blocks trading.
	orders = nil
	os.WriteFile(filepath.Join(dir, "STOP"), nil, 0o644)
	e.Tick(context.Background())
	if len(orders) != 0 {
		t.Fatalf("STOP file should block orders, got %d", len(orders))
	}
}
