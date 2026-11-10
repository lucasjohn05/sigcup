// Package susq is a minimal client for the Super Market API used by the
// SIG Predictions Cup. Shapes follow the published OpenAPI spec.
package susq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBase = "https://www.thesuper.market/api/v1"

type Client struct {
	base         string
	key          string
	hc           *http.Client
	TournamentID string // UUID, set after resolving the slug
}

func New(base, key string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{base: strings.TrimRight(base, "/"), key: key, hc: &http.Client{Timeout: 20 * time.Second}}
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("susq %d %s: %s", e.Status, e.Code, e.Message)
}

// fatalCodes are errors a retry can't fix. The bot halts and pages you.
var fatalCodes = map[string]bool{
	"TERMS_NOT_ACKNOWLEDGED": true, "ACCOUNT_BANNED": true, "API_KEY_EXPIRED": true,
	"API_KEY_REVOKED": true, "INVALID_API_KEY": true, "MISSING_API_KEY": true,
	"INSUFFICIENT_SCOPES": true, "UNAUTHORIZED": true,
}

// IsFatal reports whether err is an auth/terms/ban error. It returns the code.
func IsFatal(err error) (string, bool) {
	var ae *APIError
	if errors.As(err, &ae) && (fatalCodes[ae.Code] || ae.Status == 401) {
		if ae.Code == "" {
			return fmt.Sprintf("HTTP_%d", ae.Status), true
		}
		return ae.Code, true
	}
	return "", false
}

// retryable per the spec: 429, 502 ORDER_STATUS_UNKNOWN (safe with same
// idempotency key), 503 TX_CONFLICT / SERVICE_UNAVAILABLE.
func retryable(status int) bool { return status == 429 || status == 502 || status == 503 }

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			d := time.Duration(250*(1<<attempt)) * time.Millisecond
			d += time.Duration(rand.Intn(200)) * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil || len(raw) == 0 {
				return nil
			}
			return json.Unmarshal(raw, out)
		}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &env)
		lastErr = &APIError{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
		if !retryable(resp.StatusCode) {
			return lastErr
		}
	}
	return lastErr
}

// ---- tournaments ----

type Tournament struct {
	ID             string  `json:"id"`
	Slug           string  `json:"slug"`
	Name           string  `json:"name"`
	Status         string  `json:"status"`
	EndDate        *string `json:"endDate"`
	InitialBalance float64 `json:"initialBalance"`
	MyBalance      float64 `json:"myBalance"`
	MemberCount    int     `json:"memberCount"`
	MarketCount    int     `json:"marketCount"`
}

func (c *Client) Tournament(ctx context.Context, slug string) (Tournament, error) {
	var t Tournament
	err := c.do(ctx, "GET", "/tournaments/"+url.PathEscape(slug), nil, nil, &t)
	return t, err
}

func (c *Client) TournamentsRaw(ctx context.Context) (json.RawMessage, error) {
	var r json.RawMessage
	err := c.do(ctx, "GET", "/tournaments", nil, nil, &r)
	return r, err
}

// ---- markets ----

type Exchange struct {
	ID          string   `json:"id"`
	Option      *string  `json:"option"`
	LatestPrice *float64 `json:"latestPrice"`
}

type Market struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	SettlementDate *string    `json:"settlementDate"`
	Categories     []string   `json:"categories"`
	IsComposite    bool       `json:"isComposite"`
	IsMultiOutcome bool       `json:"isMultiOutcome"`
	Exchanges      []Exchange `json:"exchanges"`
}

func (c *Client) Markets(ctx context.Context) ([]Market, error) {
	var all []Market
	cursor := ""
	for {
		q := url.Values{"limit": {"100"}, "status": {"open"}}
		if c.TournamentID != "" {
			q.Set("tournamentId", c.TournamentID)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Data       []Market `json:"data"`
			Pagination struct {
				HasMore    bool    `json:"hasMore"`
				NextCursor *string `json:"nextCursor"`
			} `json:"pagination"`
		}
		if err := c.do(ctx, "GET", "/markets", q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if !page.Pagination.HasMore || page.Pagination.NextCursor == nil {
			return all, nil
		}
		cursor = *page.Pagination.NextCursor
	}
}

// ---- prices / books ----

type Quote struct {
	ExchangeID  string   `json:"exchangeId"`
	MarketID    string   `json:"marketId"`
	Option      *string  `json:"option"`
	LatestPrice *float64 `json:"latestPrice"`
	BestBid     *float64 `json:"bestBid"`
	BestAsk     *float64 `json:"bestAsk"`
}

// Prices fetches top of book for many exchanges (spec caps ids at 100 per call).
func (c *Client) Prices(ctx context.Context, ids []string) (map[string]Quote, error) {
	out := map[string]Quote{}
	for i := 0; i < len(ids); i += 100 {
		j := min(i+100, len(ids))
		q := url.Values{"ids": {strings.Join(ids[i:j], ",")}}
		if c.TournamentID != "" {
			q.Set("tournamentId", c.TournamentID)
		}
		var r struct {
			Data []Quote `json:"data"`
		}
		if err := c.do(ctx, "GET", "/exchanges/prices", q, nil, &r); err != nil {
			return nil, err
		}
		for _, x := range r.Data {
			out[x.ExchangeID] = x
		}
	}
	return out, nil
}

type Level struct {
	Price    float64 `json:"price"`
	Quantity float64 `json:"quantity"`
}

type Book struct {
	ExchangeID string   `json:"exchangeId"`
	Bids       []Level  `json:"bids"`
	Asks       []Level  `json:"asks"`
	BestBid    *float64 `json:"bestBid"`
	BestAsk    *float64 `json:"bestAsk"`
}

func (c *Client) Orderbook(ctx context.Context, exchangeID string, depth int) (Book, error) {
	q := url.Values{"depth": {strconv.Itoa(depth)}}
	if c.TournamentID != "" {
		q.Set("tournamentId", c.TournamentID)
	}
	var b Book
	err := c.do(ctx, "GET", "/exchanges/"+exchangeID+"/orderbook", q, nil, &b)
	return b, err
}

// ---- orders ----

type Leg struct {
	ExchangeID     string   `json:"exchangeId"`
	Side           string   `json:"side"`   // yes | no
	Action         string   `json:"action"` // buy | sell
	Quantity       int      `json:"quantity"`
	Price          *float64 `json:"price,omitempty"`
	ExpirationDate string   `json:"expirationDate,omitempty"`
	TournamentID   string   `json:"tournamentId,omitempty"`
}

type OrderReq struct {
	Leg
	IdempotencyKey string `json:"idempotencyKey"`
}

type OrderResp struct {
	OrderID            *int64   `json:"orderId"`
	ExchangeID         string   `json:"exchangeId"`
	Open               bool     `json:"open"`
	RemainingQuantity  float64  `json:"remainingQuantity"`
	Action             string   `json:"action"`
	Side               string   `json:"side"`
	Price              float64  `json:"price"`
	Quantity           int      `json:"quantity"`
	TerminalReasonCode *string  `json:"terminalReasonCode"`
	QuantityTraded     float64  `json:"quantityTraded"`
	TotalCost          float64  `json:"totalCost"`
	FillPrice          *float64 `json:"fillPrice"`
}

func (c *Client) PlaceOrder(ctx context.Context, o OrderReq) (OrderResp, error) {
	if o.TournamentID == "" {
		o.TournamentID = c.TournamentID
	}
	var r OrderResp
	err := c.do(ctx, "POST", "/orders", nil, o, &r)
	return r, err
}

// MultiLeg places up to 10 legs atomically (all placed or none). Placement is
// atomic; fills are not, so callers must clean up resting remainders.
func (c *Client) MultiLeg(ctx context.Context, key string, legs []Leg) ([]OrderResp, error) {
	for i := range legs {
		if legs[i].TournamentID == "" {
			legs[i].TournamentID = c.TournamentID
		}
	}
	body := map[string]any{"idempotencyKey": key, "legs": legs}
	var r struct {
		Results []struct {
			Index int       `json:"index"`
			Data  OrderResp `json:"data"`
		} `json:"results"`
	}
	if err := c.do(ctx, "POST", "/orders/multi-leg", nil, body, &r); err != nil {
		return nil, err
	}
	out := make([]OrderResp, len(legs))
	for _, x := range r.Results {
		if x.Index >= 0 && x.Index < len(out) {
			out[x.Index] = x.Data
		}
	}
	return out, nil
}

func (c *Client) CancelOrder(ctx context.Context, id int64) error {
	return c.do(ctx, "DELETE", "/orders/"+strconv.FormatInt(id, 10), nil, nil, nil)
}

// Order is a resting or historical order as the API reports it.
type Order struct {
	ID         int64    `json:"id"`
	ExchangeID string   `json:"exchangeId"`
	Side       string   `json:"side"`
	Action     string   `json:"action"`
	Quantity   float64  `json:"quantity"` // executable remainder while open
	PriceLimit *float64 `json:"priceLimit"`
	Open       bool     `json:"open"`
}

// YesSpace normalizes an order to (action on YES, YES price). The engine
// may record "sell YES at p" as "buy NO at 1-p"; both mean the same thing.
func (o Order) YesSpace() (action string, price float64, ok bool) {
	if o.PriceLimit == nil {
		return "", 0, false
	}
	p := *o.PriceLimit
	if o.Side == "no" {
		if o.Action == "buy" {
			return "sell", 1 - p, true
		}
		return "buy", 1 - p, true
	}
	return o.Action, p, true
}

// OpenOrders lists every resting order in the tournament in one pass.
func (c *Client) OpenOrders(ctx context.Context) ([]Order, error) {
	var all []Order
	cursor := ""
	for {
		q := url.Values{"status": {"open"}, "limit": {"200"}}
		if c.TournamentID != "" {
			q.Set("tournamentId", c.TournamentID)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Data       []Order `json:"data"`
			Pagination struct {
				HasMore    bool    `json:"hasMore"`
				NextCursor *string `json:"nextCursor"`
			} `json:"pagination"`
		}
		if err := c.do(ctx, "GET", "/orders", q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if !page.Pagination.HasMore || page.Pagination.NextCursor == nil {
			return all, nil
		}
		cursor = *page.Pagination.NextCursor
	}
}

// OrderFilled returns the total quantity an order filled, from its fill
// records. Use it after cancelling a remainder, since the placement response
// can miss fills that land between placement and cancel.
func (c *Client) OrderFilled(ctx context.Context, id int64) (float64, error) {
	var r struct {
		Data []struct {
			Quantity float64 `json:"quantity"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/orders/"+strconv.FormatInt(id, 10)+"/fills", nil, nil, &r); err != nil {
		return 0, err
	}
	total := 0.0
	for _, f := range r.Data {
		total += math.Abs(f.Quantity) // outcome-signed: NO fills are negative
	}
	return total, nil
}

func (c *Client) CancelAll(ctx context.Context) error {
	body := map[string]string{}
	if c.TournamentID != "" {
		body["tournamentId"] = c.TournamentID
	}
	return c.do(ctx, "POST", "/orders/cancel-all", nil, body, nil)
}

// ---- portfolio ----

type Lot struct {
	Side       string  `json:"side"`
	Quantity   float64 `json:"quantity"`
	EntryPrice float64 `json:"entryPrice"`
}

type Position struct {
	ExchangeID    string   `json:"exchangeId"`
	MarketID      string   `json:"marketId"`
	MarketTitle   string   `json:"marketTitle"`
	Option        *string  `json:"option"`
	Settled       bool     `json:"settled"`
	Quantity      float64  `json:"quantity"`
	AvgCost       float64  `json:"avgCost"`
	CurrentPrice  *float64 `json:"currentPrice"`
	MarketValue   float64  `json:"marketValue"`
	CostBasis     float64  `json:"costBasis"`
	UnrealizedPnl float64  `json:"unrealizedPnl"`
	Lots          []Lot    `json:"lots"`
}

type Positions struct {
	Positions []Position `json:"positions"`
	Summary   struct {
		TotalMarketValue   float64 `json:"totalMarketValue"`
		TotalCostBasis     float64 `json:"totalCostBasis"`
		TotalUnrealizedPnl float64 `json:"totalUnrealizedPnl"`
	} `json:"summary"`
}

func (c *Client) Positions(ctx context.Context, slug string) (Positions, error) {
	var p Positions
	err := c.do(ctx, "GET", "/tournaments/"+url.PathEscape(slug)+"/portfolio/positions", nil, nil, &p)
	return p, err
}

func (c *Client) SmartScore(ctx context.Context, slug string) (json.RawMessage, error) {
	var r json.RawMessage
	err := c.do(ctx, "GET", "/tournaments/"+url.PathEscape(slug)+"/me/smart-score", nil, nil, &r)
	return r, err
}

// ---- leaderboard ----

type LeaderRow struct {
	Rank        int      `json:"rank"`
	ProfileID   string   `json:"profileId"`
	Username    *string  `json:"username"`
	PnL         float64  `json:"pnl"`
	TradesCount int      `json:"tradesCount"`
	Volume      float64  `json:"volume"`
	WinRate     *float64 `json:"winRate"`
	ROI         float64  `json:"roi"`
}

type Standing struct {
	Leaderboard []LeaderRow `json:"leaderboard"`
	Total       int         `json:"total"`
	MyRank      *int        `json:"myRank"`
}

func (c *Client) Leaderboard(ctx context.Context, slug string, limit int) (Standing, error) {
	var s Standing
	q := url.Values{"period": {"all"}, "limit": {strconv.Itoa(limit)}}
	err := c.do(ctx, "GET", "/tournaments/"+url.PathEscape(slug)+"/leaderboard", q, nil, &s)
	return s, err
}

type SmartScoreEntry struct {
	MarketType        string  `json:"marketType"`
	SmartScoreDecayed float64 `json:"smartScoreDecayed"`
	IsElite           bool    `json:"isElite"`
	Rank              int     `json:"rank"`
	TotalTraders      int     `json:"totalTraders"`
	MinimumEliteScore float64 `json:"minimumEliteScore"`
}

// ---- ALL collateral ----

type Collateral struct {
	ComponentID           string   `json:"componentId"`
	OutstandingAdvance    float64  `json:"outstandingAdvance"`
	CoveredExchangeIDs    []string `json:"coveredExchangeIds"`
	GuaranteedPayoutFloor *float64 `json:"guaranteedPayoutFloor"`
	Diagnostic            *string  `json:"diagnostic"`
}

func (c *Client) Collateral(ctx context.Context) ([]Collateral, error) {
	q := url.Values{}
	if c.TournamentID != "" {
		q.Set("tournamentId", c.TournamentID)
	}
	var r struct {
		Data []Collateral `json:"data"`
	}
	err := c.do(ctx, "GET", "/portfolio/collateral", q, nil, &r)
	return r.Data, err
}

// ---- batch orders ----

type BatchResult struct {
	Index  int       `json:"index"`
	OK     bool      `json:"ok"`
	Status int       `json:"status"`
	Data   OrderResp `json:"data"`
	Raw    json.RawMessage
}

// Batch places up to 50 orders. Success is per order (partial is normal),
// so check each result's OK.
func (c *Client) Batch(ctx context.Context, key string, orders []Leg) ([]BatchResult, error) {
	for i := range orders {
		if orders[i].TournamentID == "" {
			orders[i].TournamentID = c.TournamentID
		}
	}
	body := map[string]any{"idempotencyKey": key, "orders": orders}
	var r struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := c.do(ctx, "POST", "/orders/batch", nil, body, &r); err != nil {
		return nil, err
	}
	out := make([]BatchResult, 0, len(r.Results))
	for _, raw := range r.Results {
		var br BatchResult
		_ = json.Unmarshal(raw, &br)
		br.Raw = raw
		out = append(out, br)
	}
	return out, nil
}

// ---- relationships (ALL) ----

type RelNode struct {
	ExchangeID  string  `json:"exchangeId"`
	MarketTitle *string `json:"marketTitle"`
	Outcome     *string `json:"outcome"`
	Role        string  `json:"role"`
}

type Relationship struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Status       string    `json:"status"`
	Label        *string   `json:"label"`
	IsExhaustive bool      `json:"isExhaustive"`
	Nodes        []RelNode `json:"nodes"`
}

func (c *Client) Relationships(ctx context.Context) ([]Relationship, error) {
	var all []Relationship
	cursor := ""
	for {
		q := url.Values{"limit": {"200"}}
		if c.TournamentID != "" {
			q.Set("tournamentId", c.TournamentID)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Data       []Relationship `json:"data"`
			Pagination struct {
				HasMore    bool    `json:"hasMore"`
				NextCursor *string `json:"nextCursor"`
			} `json:"pagination"`
		}
		if err := c.do(ctx, "GET", "/relationships", q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		if !page.Pagination.HasMore || page.Pagination.NextCursor == nil {
			return all, nil
		}
		cursor = *page.Pagination.NextCursor
	}
}
