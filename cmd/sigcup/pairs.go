package main

import (
	"strings"

	"sigcup/internal/sim"
	"sigcup/internal/susq"
)

// slowStates may not settle cleanly on Nov 4: runoffs (GA, LA) and
// ranked-choice counts (ME, AK). Neither side might be declared the winner by
// settlement, so "one of the two must pay" isn't safe to assume there.
var slowStates = map[string]bool{"GA": true, "LA": true, "ME": true, "AK": true}

// pairRelationships builds one relationship per race from races.csv: the
// Dem (or Independent) contract and the GOP contract can't both pay.
//
// Selling both when their bids sum past 1 is always safe: at most one pays.
// That's the common case, created when someone pushes one side up.
// Buying both when their asks sum under 1 is only safe if one of them must
// pay, so races are marked exhaustive only when they're plain two-party
// contests expected to settle on time.
func pairRelationships(races []sim.Race) []susq.Relationship {
	yes := "YES"
	var out []susq.Relationship
	for _, r := range races {
		if r.AntiID == "" || r.GOPID == "" {
			continue
		}
		label := "pair " + r.Key
		threeWay := r.AntiParty != "D" || strings.Contains(r.Note, "Independent")
		out = append(out, susq.Relationship{
			ID:           "pair-" + r.Key,
			Type:         "mutually_exclusive",
			Status:       "active",
			Label:        &label,
			IsExhaustive: !threeWay && !slowStates[r.State] && r.Kind != "control",
			Nodes: []susq.RelNode{
				{ExchangeID: r.AntiID, Outcome: &yes, Role: "member"},
				{ExchangeID: r.GOPID, Outcome: &yes, Role: "member"},
			},
		})
	}
	return out
}
