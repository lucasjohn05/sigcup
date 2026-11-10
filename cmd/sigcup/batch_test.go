package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigcup/internal/susq"
)

func writeFile(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "b.csv")
	os.WriteFile(p, []byte(body), 0o644)
	return p
}

func TestLoadBatch(t *testing.T) {
	p := writeFile(t, `# comment
exchange_id,action,limit,quantity,notional,note
11,buy,0.123,,1000,AZ-06 Dem
12,sell,0.401,500,,NV Sen GOP
`)
	rows, err := loadBatch(p)
	if err != nil {
		t.Fatal(err)
	}
	// Buy limit floors to 0.12; 1000 notional at 0.12 = 8333 shares.
	if rows[0].Limit != 0.12 || rows[0].Qty != 8333 || rows[0].Warn == "" {
		t.Fatalf("row 0: %+v", rows[0])
	}
	// Sell limit ceils to 0.405 (never sell for less than you wrote).
	if rows[1].Limit != 0.405 || rows[1].Qty != 500 {
		t.Fatalf("row 1: %+v", rows[1])
	}
	if _, err := loadBatch(writeFile(t, "11,hold,0.5,1,,\n")); err == nil {
		t.Fatal("bad action should fail")
	}
	if _, err := loadBatch(writeFile(t, "11,buy,0.5,,,\n")); err == nil {
		t.Fatal("missing size should fail")
	}
}

func book(bids, asks [][2]float64) susq.Book {
	var b susq.Book
	for _, x := range bids {
		b.Bids = append(b.Bids, susq.Level{Price: x[0], Quantity: x[1]})
	}
	for _, x := range asks {
		b.Asks = append(b.Asks, susq.Level{Price: x[0], Quantity: x[1]})
	}
	return b
}

func TestFillable(t *testing.T) {
	b := book([][2]float64{{0.40, 100}, {0.38, 100}}, [][2]float64{{0.10, 100}, {0.12, 100}, {0.15, 999}})
	q, avg := fillable(batchRow{Action: "buy", Limit: 0.12, Qty: 500}, b)
	if q != 200 || avg < 0.1099 || avg > 0.1101 {
		t.Fatalf("buy: q=%d avg=%v", q, avg)
	}
	q, _ = fillable(batchRow{Action: "sell", Limit: 0.39, Qty: 500}, b)
	if q != 100 {
		t.Fatalf("sell: q=%d", q)
	}
}

func TestFlattenRows(t *testing.T) {
	pos := []susq.Position{
		{ExchangeID: "1", MarketTitle: "OH Sen", Quantity: 300}, // YES -> sell
		{ExchangeID: "2", MarketTitle: "PA-07", Quantity: -200}, // NO -> buy back
		{ExchangeID: "3", MarketTitle: "arb A", Quantity: -100}, // arb pair
		{ExchangeID: "4", MarketTitle: "arb B", Quantity: -100},
		{ExchangeID: "5", MarketTitle: "done", Quantity: 50, Settled: true},
	}
	books := map[string]susq.Book{
		"1": book([][2]float64{{0.40, 10}}, nil),
		"2": book(nil, [][2]float64{{0.62, 10}}),
		"3": book([][2]float64{{0.4, 1}}, [][2]float64{{0.5, 1}}),
		"4": book([][2]float64{{0.4, 1}}, [][2]float64{{0.5, 1}}),
	}
	rels := []susq.Relationship{{ID: "r1", Nodes: []susq.RelNode{{ExchangeID: "3"}, {ExchangeID: "4"}}}}
	rows := flattenRows(pos, books, rels, 0.02)
	if len(rows) != 4 {
		t.Fatalf("want 4 rows (settled excluded), got %d", len(rows))
	}
	if rows[0].Action != "sell" || rows[0].Limit != 0.38 || rows[0].Qty != 300 || rows[0].Skip != "" {
		t.Fatalf("row 0: %+v", rows[0])
	}
	if rows[1].Action != "buy" || rows[1].Limit != 0.64 || rows[1].Qty != 200 || rows[1].Skip != "" {
		t.Fatalf("row 1: %+v", rows[1])
	}
	if rows[2].Skip == "" || rows[3].Skip == "" {
		t.Fatal("arb legs should be skipped")
	}
}

func TestFireEndToEnd(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		switch {
		case r.URL.Path == "/orders/batch":
			var body struct {
				Key    string           `json:"idempotencyKey"`
				Orders []map[string]any `json:"orders"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Key == "" {
				t.Error("missing idempotency key")
			}
			got = body.Orders
			enc.Encode(map[string]any{"results": []any{
				map[string]any{"index": 0, "ok": true, "status": 201, "data": map[string]any{
					"orderId": 7, "open": true, "quantityTraded": 100}},
				map[string]any{"index": 1, "ok": false, "status": 422, "data": map[string]any{
					"error": map[string]any{"code": "VALIDATION_ERROR"}}},
			}})
		case r.Method == "DELETE":
			enc.Encode(map[string]any{"message": "ok"})
		case strings.HasSuffix(r.URL.Path, "/fills"):
			enc.Encode(map[string]any{"data": []any{map[string]any{"quantity": 150}}})
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	os.Chdir(dir)
	p := writeFile(t, "11,buy,0.12,300,,a\n12,sell,0.40,100,,b\n")
	s := susq.New(srv.URL, "k")
	s.TournamentID = "t1"
	runFire(context.Background(), s, p, 1000, true, 5*time.Second, filepath.Join(dir, "j.jsonl"))
	if len(got) != 2 || got[0]["price"].(float64) != 0.12 || got[0]["tournamentId"] != "t1" || got[0]["expirationDate"] == nil {
		t.Fatalf("bad orders sent: %v", got)
	}
	j, _ := os.ReadFile(filepath.Join(dir, "j.jsonl"))
	// Order 0 was open: cancelled, then fills re-read as 150 (not 100).
	if !strings.Contains(string(j), `"filled":150`) || !strings.Contains(string(j), "status 422") {
		t.Fatalf("journal missing fill/error:\n%s", j)
	}
}
