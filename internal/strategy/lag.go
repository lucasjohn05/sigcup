package strategy

import (
	"math"
	"sort"
	"time"
)

// lagTracker measures how fast the SUSQ book follows Kalshi. When a mapped
// Kalshi mid moves Thresh or more from its anchor, an event opens. It
// closes as "followed" once the SUSQ mid has moved at least half as far in
// the same direction, or "unfollowed" after MaxWait. If Kalshi reverts
// first, the event is dropped.
//
// It can't tell SIG's market maker apart from other traders. That's fine:
// election night cares about how stale the whole book gets, whoever moves it.
// Resolution is one tick, so lag 0 means "within one tick".
type lagTracker struct {
	Thresh  float64
	MaxWait time.Duration

	anchor map[string]lagPoint
	open   map[string]*lagEvent
}

type lagPoint struct{ k, s float64 }

type lagEvent struct {
	start        time.Time
	kFrom, kMove float64
	sFrom, dir   float64
}

type lagResult struct {
	Exchange string
	Followed bool
	Lag      time.Duration
	KMove    float64
}

func (l *lagTracker) observe(id string, now time.Time, k, s float64) *lagResult {
	if l.Thresh <= 0 {
		return nil
	}
	if l.anchor == nil {
		l.anchor, l.open = map[string]lagPoint{}, map[string]*lagEvent{}
	}
	a, ok := l.anchor[id]
	if !ok {
		l.anchor[id] = lagPoint{k, s}
		return nil
	}
	ev := l.open[id]
	if ev == nil {
		if math.Abs(k-a.k) < l.Thresh {
			return nil
		}
		dir := 1.0
		if k < a.k {
			dir = -1
		}
		ev = &lagEvent{start: now, kFrom: a.k, kMove: math.Abs(k - a.k), sFrom: a.s, dir: dir}
		l.open[id] = ev
	}
	// Kalshi reverted: ambiguous, drop it.
	if (k-ev.kFrom)*ev.dir < ev.kMove/2 && now.After(ev.start) {
		delete(l.open, id)
		l.anchor[id] = lagPoint{k, s}
		return nil
	}
	if (s-ev.sFrom)*ev.dir >= ev.kMove/2 {
		delete(l.open, id)
		l.anchor[id] = lagPoint{k, s}
		return &lagResult{Exchange: id, Followed: true, Lag: now.Sub(ev.start), KMove: ev.kMove}
	}
	if now.Sub(ev.start) > l.MaxWait {
		delete(l.open, id)
		l.anchor[id] = lagPoint{k, s}
		return &lagResult{Exchange: id, Followed: false, Lag: now.Sub(ev.start), KMove: ev.kMove}
	}
	return nil
}

// LagSummary condenses lag results for the digest.
type LagSummary struct {
	Followed, Unfollowed, WithinTick int
	Median                           time.Duration
}

func summarizeLags(followed []time.Duration, unfollowed int) LagSummary {
	s := LagSummary{Followed: len(followed), Unfollowed: unfollowed}
	if len(followed) == 0 {
		return s
	}
	d := append([]time.Duration(nil), followed...)
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	s.Median = d[len(d)/2]
	for _, x := range d {
		if x == 0 {
			s.WithinTick++
		}
	}
	return s
}
