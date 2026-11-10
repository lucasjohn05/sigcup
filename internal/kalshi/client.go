// Package kalshi fetches public Kalshi top-of-book for fair value.
// One batched GET per loop (tickers=...), no auth needed for market data.
package kalshi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBase = "https://api.elections.kalshi.com/trade-api/v2"

type Client struct {
	base string
	hc   *http.Client
}

func New(base string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 15 * time.Second}}
}

// Quote is YES bid/ask in [0,1].
type Quote struct {
	Ticker  string
	Bid     float64
	Ask     float64
	Fetched time.Time
}

type rawMarket struct {
	Ticker        string   `json:"ticker"`
	Status        string   `json:"status"`
	YesBid        *float64 `json:"yes_bid"` // cents (legacy)
	YesAsk        *float64 `json:"yes_ask"`
	YesBidDollars *string  `json:"yes_bid_dollars"` // "0.4500" (newer)
	YesAskDollars *string  `json:"yes_ask_dollars"`
}

func pick(dollars *string, cents *float64) (float64, bool) {
	if dollars != nil && *dollars != "" {
		if v, err := strconv.ParseFloat(*dollars, 64); err == nil {
			return v, true
		}
	}
	if cents != nil {
		return *cents / 100, true
	}
	return 0, false
}

func (c *Client) Quotes(ctx context.Context, tickers []string) (map[string]Quote, error) {
	out := map[string]Quote{}
	for i := 0; i < len(tickers); i += 100 {
		j := min(i+100, len(tickers))
		q := url.Values{"tickers": {strings.Join(tickers[i:j], ",")}, "limit": {"200"}}
		req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/markets?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		var r struct {
			Markets []rawMarket `json:"markets"`
		}
		err = json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("kalshi status %d", resp.StatusCode)
		}
		if err != nil {
			return nil, err
		}
		now := time.Now()
		for _, m := range r.Markets {
			bid, ok1 := pick(m.YesBidDollars, m.YesBid)
			ask, ok2 := pick(m.YesAskDollars, m.YesAsk)
			// Closed or settled markets keep their last book; don't price off it.
			if m.Status != "" && m.Status != "active" && m.Status != "open" {
				continue
			}
			// Empty sides come back as 0 / 1.00; treat those as no quote.
			if !ok1 || !ok2 || bid <= 0 || ask >= 1 || ask <= bid {
				continue
			}
			out[m.Ticker] = Quote{Ticker: m.Ticker, Bid: bid, Ask: ask, Fetched: now}
		}
	}
	return out, nil
}
