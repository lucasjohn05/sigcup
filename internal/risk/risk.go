// Package risk sizes orders and decides whether the bot may trade at all.
//
// These caps exist to keep capital liquid and survive bugs during the
// bankroll-building phase, not to protect a "respectable" finish.
package risk

import (
	"math"
	"os"
	"sync"
	"time"
)

type Limits struct {
	MaxOrderNotional    float64 // SUSQies per order (or per arb set batch)
	MaxExchangeNotional float64 // cost basis cap per contract
	MaxDeployedFrac     float64 // share of portfolio value allowed in positions
	DailyLossFrac       float64 // halt new orders if value falls this much in a UTC day
	MinPrice, MaxPrice  float64 // never take at prices outside this band
}

type Guard struct {
	L        Limits
	StopFile string

	mu       sync.Mutex
	day      string
	dayStart float64
	halted   bool
	reason   string
}

// Update records portfolio value and trips the daily-loss halt. Returns a
// non-empty string the first time the halt trips.
func (g *Guard) Update(now time.Time, value float64) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := now.UTC().Format("2006-01-02")
	if d != g.day {
		g.day, g.dayStart = d, value
		if g.halted && g.reason == "daily loss" {
			g.halted, g.reason = false, "" // new day, reset
		}
	}
	if !g.halted && g.dayStart > 0 && value < g.dayStart*(1-g.L.DailyLossFrac) {
		g.halted, g.reason = true, "daily loss"
		return "daily loss limit hit; new orders halted until 00:00 UTC"
	}
	return ""
}

// Halt stops new orders until the process restarts. It is for failures a
// retry can't fix (revoked key, terms not accepted, banned). Returns true the
// first time it trips.
func (g *Guard) Halt(reason string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.halted && g.reason == reason {
		return false
	}
	g.halted, g.reason = true, reason
	return true
}

// CanTrade is false when the STOP file exists or a halt tripped.
func (g *Guard) CanTrade() (bool, string) {
	if _, err := os.Stat(g.StopFile); err == nil {
		return false, "STOP file present"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.halted {
		return false, g.reason
	}
	return true, ""
}

func (g *Guard) PriceOK(p float64) bool { return p >= g.L.MinPrice && p <= g.L.MaxPrice }

// Size returns whole shares given the per-share cash cost, book size available,
// current cost basis in this contract, total deployed, and portfolio value.
func (g *Guard) Size(costPerShare, bookQty, exchangeExposure, deployed, value float64) int {
	if costPerShare <= 0 {
		return 0
	}
	room := math.Min(g.L.MaxOrderNotional, g.L.MaxExchangeNotional-exchangeExposure)
	room = math.Min(room, g.L.MaxDeployedFrac*value-deployed)
	if room <= 0 {
		return 0
	}
	q := math.Min(bookQty, room/costPerShare)
	return int(math.Floor(q))
}
