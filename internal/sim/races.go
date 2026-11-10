// Package sim prices the whole board off one national swing and searches
// for the stack that maximizes P(final balance >= a target), which is what
// winning a top-heavy contest needs. Expected value is the wrong objective.
package sim

import (
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Race is one contest reduced to a binary: does the anti-GOP side (the
// Democrat, or the Independent where that's the real challenger, e.g. NE/MT
// Senate) beat the Republican?
type Race struct {
	Key       string // TX-SEN, TX-GOV, TX-23, US-SEN, US-HOUSE
	Kind      string // sen | gov | house | control
	State     string // TX, ... or US for chamber control
	AntiParty string // D or I
	AntiID    string // SUSQ exchange for the anti-GOP YES
	GOPID     string // SUSQ exchange for the GOP YES
	AntiPx    float64
	GOPPx     float64 // snapshot mids (YES prices); used offline and as fallback
	PFair     float64 // YOUR override of P(anti-GOP wins); 0 = derive from prices
	Note      string

	// Filled at runtime from live books (0 = unknown).
	AntiBid, AntiAsk, GOPBid, GOPAsk float64
}

// P is the baseline probability the anti-GOP side wins: your override if
// set, otherwise the average of what the two markets imply.
func (r Race) P() float64 {
	if r.PFair > 0 {
		return r.PFair
	}
	a, g := mid(r.AntiBid, r.AntiAsk, r.AntiPx), mid(r.GOPBid, r.GOPAsk, r.GOPPx)
	switch {
	case a > 0 && g > 0:
		return (a + (1 - g)) / 2
	case a > 0:
		return a
	case g > 0:
		return 1 - g
	}
	return 0
}

func mid(bid, ask, fallback float64) float64 {
	if bid > 0 && ask > 0 {
		return (bid + ask) / 2
	}
	return fallback
}

var raceHeader = []string{"race", "kind", "state", "anti_party", "anti_id", "gop_id", "anti_px", "gop_px", "p_fair", "note"}

func LoadRaces(path string) ([]Race, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comment = '#'
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var out []Race
	seen := map[string]bool{}
	for i, rec := range recs {
		for len(rec) < len(raceHeader) {
			rec = append(rec, "")
		}
		for j := range rec {
			rec[j] = strings.TrimSpace(rec[j])
		}
		if rec[0] == "" || rec[0] == "race" {
			continue
		}
		if seen[rec[0]] {
			return nil, fmt.Errorf("%s line %d: duplicate race %s", path, i+1, rec[0])
		}
		seen[rec[0]] = true
		rc := Race{Key: rec[0], Kind: rec[1], State: rec[2], AntiParty: rec[3], AntiID: rec[4], GOPID: rec[5], Note: rec[9]}
		var perr error
		num := func(s string) float64 {
			if s == "" {
				return 0
			}
			v, err := strconv.ParseFloat(s, 64)
			if err != nil || v < 0 || v > 1 {
				perr = fmt.Errorf("%s line %d: %q is not a probability", path, i+1, s)
			}
			return v
		}
		rc.AntiPx, rc.GOPPx, rc.PFair = num(rec[6]), num(rec[7]), num(rec[8])
		if perr != nil {
			return nil, perr
		}
		switch rc.Kind {
		case "sen", "gov", "house", "control":
		default:
			return nil, fmt.Errorf("%s line %d: kind must be sen/gov/house/control", path, i+1)
		}
		out = append(out, rc)
	}
	return out, nil
}

func SaveRaces(path string, races []Race) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, "# One row per race. anti = the side that beats the Republican (D, or I where noted).")
	fmt.Fprintln(f, "# anti_px/gop_px: YES mids when this file was built. p_fair: >>> YOU (optional) your own")
	fmt.Fprintln(f, "# P(anti-GOP wins), e.g. Kalshi or a rating conversion. Blank = use market prices.")
	w := csv.NewWriter(f)
	w.Write(raceHeader)
	ff := func(x float64) string {
		if x == 0 {
			return ""
		}
		return strconv.FormatFloat(x, 'f', 3, 64)
	}
	for _, r := range races {
		w.Write([]string{r.Key, r.Kind, r.State, r.AntiParty, r.AntiID, r.GOPID, ff(r.AntiPx), ff(r.GOPPx), ff(r.PFair), r.Note})
	}
	w.Flush()
	return w.Error()
}

var titleRE = regexp.MustCompile(`^Will the (Democratic|Republican|Independent) Party win the (.+?)\??$`)
var districtRE = regexp.MustCompile(`^([A-Z]{2})-(\d{2}) House race$`)

var states = map[string]string{
	"Alabama": "AL", "Alaska": "AK", "Arizona": "AZ", "Arkansas": "AR", "California": "CA", "Colorado": "CO",
	"Connecticut": "CT", "Delaware": "DE", "Florida": "FL", "Georgia": "GA", "Hawaii": "HI", "Idaho": "ID",
	"Illinois": "IL", "Indiana": "IN", "Iowa": "IA", "Kansas": "KS", "Kentucky": "KY", "Louisiana": "LA",
	"Maine": "ME", "Maryland": "MD", "Massachusetts": "MA", "Michigan": "MI", "Minnesota": "MN",
	"Mississippi": "MS", "Missouri": "MO", "Montana": "MT", "Nebraska": "NE", "Nevada": "NV",
	"New Hampshire": "NH", "New Jersey": "NJ", "New Mexico": "NM", "New York": "NY", "North Carolina": "NC",
	"North Dakota": "ND", "Ohio": "OH", "Oklahoma": "OK", "Oregon": "OR", "Pennsylvania": "PA",
	"Rhode Island": "RI", "South Carolina": "SC", "South Dakota": "SD", "Tennessee": "TN", "Texas": "TX",
	"Utah": "UT", "Vermont": "VT", "Virginia": "VA", "Washington": "WA", "West Virginia": "WV",
	"Wisconsin": "WI", "Wyoming": "WY",
}

// ParseTitle maps a SUSQ market title to (race key, kind, state, party).
func ParseTitle(title string) (key, kind, state, party string, ok bool) {
	m := titleRE.FindStringSubmatch(strings.TrimSpace(title))
	if m == nil {
		return
	}
	party = map[string]string{"Democratic": "D", "Republican": "R", "Independent": "I"}[m[1]]
	obj := m[2]
	switch {
	case obj == "U.S. Senate":
		return "US-SEN", "control", "US", party, true
	case obj == "U.S. House":
		return "US-HOUSE", "control", "US", party, true
	}
	if d := districtRE.FindStringSubmatch(obj); d != nil {
		return d[1] + "-" + d[2], "house", d[1], party, true
	}
	for _, suf := range []struct{ s, k, tag string }{{" Senate", "sen", "SEN"}, {" Governor", "gov", "GOV"}} {
		if strings.HasSuffix(obj, suf.s) {
			if st, ok2 := states[strings.TrimSuffix(obj, suf.s)]; ok2 {
				return st + "-" + suf.tag, suf.k, st, party, true
			}
		}
	}
	return "", "", "", "", false
}

// Contract is one SUSQ market's tradable exchange, as dumped by -mode markets.
type Contract struct {
	ExchangeID, Title string
	Px                float64 // YES mid if known, else 0
}

// BuildRaces groups contracts into races. The anti-GOP side is the Democrat
// unless an Independent market is priced above the Democrat (NE, MT
// Senate). Races missing a side are returned in skipped.
func BuildRaces(cs []Contract) (races []Race, skipped []string) {
	type sides struct {
		kind, state string
		by          map[string]Contract
	}
	all := map[string]*sides{}
	for _, c := range cs {
		key, kind, st, party, ok := ParseTitle(c.Title)
		if !ok {
			skipped = append(skipped, "unparsed: "+c.Title)
			continue
		}
		s := all[key]
		if s == nil {
			s = &sides{kind: kind, state: st, by: map[string]Contract{}}
			all[key] = s
		}
		s.by[party] = c
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := all[k]
		gop, okR := s.by["R"]
		anti, okA := s.by["D"]
		party, note := "D", ""
		if ind, okI := s.by["I"]; okI {
			if !okA || ind.Px > anti.Px {
				anti, okA, party = ind, true, "I"
				note = "Independent is the anti-GOP side; check the Info tab"
			} else {
				note = "has an Independent market too; GOP NO also pays if the Independent wins"
			}
		}
		if !okR || !okA {
			skipped = append(skipped, "one-sided race: "+k)
			continue
		}
		races = append(races, Race{Key: k, Kind: s.kind, State: s.state, AntiParty: party,
			AntiID: anti.ExchangeID, GOPID: gop.ExchangeID, AntiPx: anti.Px, GOPPx: gop.Px, Note: note})
	}
	return races, skipped
}

// FillFromSeed fills missing snapshot prices from the seed board (the prices
// the site displayed before the open). Live prices always win; only blank
// sides are filled. Returns how many races it touched.
func FillFromSeed(races, seed []Race) int {
	by := map[string]Race{}
	for _, r := range seed {
		by[r.Key] = r
	}
	n := 0
	for i := range races {
		s, ok := by[races[i].Key]
		if !ok {
			continue
		}
		touched := false
		if races[i].AntiPx == 0 && s.AntiPx > 0 {
			races[i].AntiPx, touched = s.AntiPx, true
		}
		if races[i].GOPPx == 0 && s.GOPPx > 0 {
			races[i].GOPPx, touched = s.GOPPx, true
		}
		if touched {
			n++
			if races[i].Note == "" {
				races[i].Note = "blank price filled from seed board"
			} else {
				races[i].Note += "; blank price filled from seed board"
			}
		}
	}
	return n
}
