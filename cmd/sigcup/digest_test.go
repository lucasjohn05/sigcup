package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sigcup/internal/journal"
	"sigcup/internal/strategy"
	"sigcup/internal/susq"
)

func TestDigest(t *testing.T) {
	markets := []any{map[string]any{"id": "m1", "title": "House control"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		switch {
		case strings.HasSuffix(r.URL.Path, "/leaderboard"):
			enc.Encode(map[string]any{"leaderboard": []any{map[string]any{"rank": 1, "pnl": 8900}}, "total": 412, "myRank": 14})
		case strings.HasSuffix(r.URL.Path, "/smart-score"):
			enc.Encode([]any{map[string]any{"marketType": "politics", "smartScoreDecayed": 0.83, "rank": 40, "totalTraders": 400}})
		case strings.HasSuffix(r.URL.Path, "/markets"):
			enc.Encode(map[string]any{"data": markets, "pagination": map[string]any{"hasMore": false}})
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	j, _ := journal.Open(filepath.Join(dir, "j"), "")
	eng := &strategy.Engine{LastValue: 104210, LastCash: 71000, LastDeployed: 33210}
	d := &digester{s: susq.New(srv.URL, "k"), slug: "cup", j: j, eng: eng, known: filepath.Join(dir, "known")}
	t.Log("\n" + d.build(context.Background()))
	markets = append(markets, map[string]any{"id": "m2", "title": "AZ-06 GOP"})
	eng.LastValue = 105000
	out := d.build(context.Background())
	t.Log("\n" + out)
	if !strings.Contains(out, "AZ-06 GOP") || !strings.Contains(out, "14 of 412") {
		t.Fatal("digest missing new market or rank")
	}
}
