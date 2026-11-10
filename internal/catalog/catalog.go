// Package catalog matches extracted line items to a part catalog.
//
// Matching runs in tiers, cheapest and most certain first:
//
//  1. exact       - the written part number is a catalog SKU
//  2. normalized  - equal after dropping case and punctuation
//  3. fuzzy       - small edit distance (with transpositions) on the
//     normalized SKU, with a margin check against the runner-up
//  4. description - token overlap between descriptions, when there is no
//     usable part number
//
// Every tier then cross-checks the description and the unit of measure
// against the catalog entry. That catches the dangerous case: a typo that
// turns one valid SKU into another valid SKU.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
)

// Item is one catalog entry.
type Item struct {
	SKU         string  `json:"sku"`
	Description string  `json:"description"`
	Unit        string  `json:"unit"`
	ListPrice   float64 `json:"list_price_usd"`
}

// Catalog is an in-memory, read-only part catalog.
type Catalog struct {
	Items   []Item
	byAlnum map[string][]int
	tokens  [][]string
}

// New indexes items.
func New(items []Item) *Catalog {
	c := &Catalog{Items: items, byAlnum: map[string][]int{}}
	for i, it := range items {
		k := po.Alnum(it.SKU)
		c.byAlnum[k] = append(c.byAlnum[k], i)
		c.tokens = append(c.tokens, Tokens(it.Description))
	}
	return c
}

// Load reads a JSON array of items.
func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []Item
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("catalog %s: %w", path, err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("catalog %s: empty", path)
	}
	return New(items), nil
}

// Match methods.
const (
	MethodExact       = "exact"
	MethodNormalized  = "normalized"
	MethodFuzzy       = "fuzzy"
	MethodDescription = "description"
	MethodNone        = "none"
)

// Match is the outcome for one line.
type Match struct {
	SKU        string   `json:"sku"`
	Method     string   `json:"method"`
	Confidence float64  `json:"confidence"`
	Notes      []string `json:"notes,omitempty"`
}

// Match finds the best catalog item for a written part number, description
// and canonical unit. Any of them may be empty.
func (c *Catalog) Match(partNumber, description, unit string) Match {
	m, cands := c.matchPart(partNumber)
	confirmed := false
	if m.Method == MethodFuzzy && description != "" {
		m, confirmed = c.resolveFuzzy(m, cands, description)
	}
	if m.Method == MethodNone && description != "" {
		m = c.matchDescription(description)
	}
	if m.Method == MethodNone {
		return m
	}
	idx := slices.IndexFunc(c.Items, func(it Item) bool { return it.SKU == m.SKU })
	item := c.Items[idx]

	// Cross-checks against the chosen item.
	if description != "" && m.Method != MethodDescription && !confirmed {
		written := Tokens(description)
		switch {
		case Jaccard(written, c.tokens[idx]) < 0.2:
			m.Confidence *= 0.5
			m.Notes = append(m.Notes, "description disagrees with catalog item")
		case !numbersAgree(written, c.tokens[idx]):
			m.Confidence *= 0.5
			m.Notes = append(m.Notes, "sizes in the description differ from the catalog item")
		}
	}
	if unit != "" && unit != item.Unit {
		m.Confidence *= 0.7
		m.Notes = append(m.Notes, fmt.Sprintf("unit %s differs from catalog unit %s", unit, item.Unit))
	}
	return m
}

// candidate is a fuzzy match candidate: a catalog index and its distance.
type candidate struct{ idx, dist int }

// resolveFuzzy uses the description to choose among near-miss SKUs. A fuzzy
// match is confirmed only when one candidate's description clearly fits
// best and its sizes agree; otherwise the SKU-only result stands.
func (c *Catalog) resolveFuzzy(m Match, cands []candidate, description string) (Match, bool) {
	written := Tokens(description)
	best, bestS, secondS := -1, -1.0, -1.0
	for i, cd := range cands {
		s := Jaccard(written, c.tokens[cd.idx])
		if !numbersAgree(written, c.tokens[cd.idx]) {
			s = 0
		}
		if s > bestS {
			secondS, bestS, best = bestS, s, i
		} else if s > secondS {
			secondS = s
		}
	}
	if best < 0 || bestS < 0.5 || bestS-secondS < 0.1 {
		return m, false
	}
	cd := cands[best]
	conf := 0.9
	if cd.dist > 1 {
		conf = 0.8
	}
	return Match{SKU: c.Items[cd.idx].SKU, Method: MethodFuzzy, Confidence: conf,
		Notes: []string{"description confirms the closest SKU"}}, true
}

func (c *Catalog) matchPart(pn string) (Match, []candidate) {
	none := Match{Method: MethodNone}
	pn = strings.TrimSpace(pn)
	if pn == "" {
		return none, nil
	}
	for _, it := range c.Items {
		if it.SKU == pn {
			return Match{SKU: it.SKU, Method: MethodExact, Confidence: 1}, nil
		}
	}
	key := po.Alnum(pn)
	if len(key) < 4 {
		return none, nil
	}
	if ids := c.byAlnum[key]; len(ids) == 1 {
		return Match{SKU: c.Items[ids[0]].SKU, Method: MethodNormalized, Confidence: 0.97}, nil
	} else if len(ids) > 1 {
		return Match{SKU: c.Items[ids[0]].SKU, Method: MethodNormalized, Confidence: 0.4, Notes: []string{"normalized key is ambiguous"}}, nil
	}

	dists := make([]int, len(c.Items))
	best, bestD, second := -1, 1<<30, 1<<30
	for i, it := range c.Items {
		d := Distance(key, po.Alnum(it.SKU))
		dists[i] = d
		if d < bestD {
			second, bestD, best = bestD, d, i
		} else if d < second {
			second = d
		}
	}
	// Allow one edit per 5 characters, at most 2.
	maxD := min(2, len(key)/5)
	if best < 0 || bestD > maxD {
		return none, nil
	}
	var cands []candidate
	for i, d := range dists {
		if d <= bestD+1 && d <= maxD {
			cands = append(cands, candidate{i, d})
		}
	}
	// A fuzzy guess alone stays below the default 0.85 auto-accept threshold:
	// it needs the description to agree before a line is accepted unseen.
	m := Match{SKU: c.Items[best].SKU, Method: MethodFuzzy, Confidence: map[int]float64{1: 0.8, 2: 0.65}[bestD]}
	switch second - bestD {
	case 0:
		m.Confidence = 0.3
		m.Notes = append(m.Notes, "tie between catalog items")
	case 1:
		m.Confidence *= 0.85
		m.Notes = append(m.Notes, "runner-up is one edit further away")
	}
	return m, cands
}

func (c *Catalog) matchDescription(desc string) Match {
	toks := Tokens(desc)
	best, bestS, secondS := -1, 0.0, 0.0
	for i := range c.Items {
		s := Jaccard(toks, c.tokens[i])
		if s > bestS {
			secondS, bestS, best = bestS, s, i
		} else if s > secondS {
			secondS = s
		}
	}
	if best < 0 || bestS < 0.3 {
		return Match{Method: MethodNone}
	}
	conf := 0.85 * bestS
	m := Match{SKU: c.Items[best].SKU, Method: MethodDescription, Confidence: conf}
	if bestS-secondS < 0.15 {
		m.Confidence *= 0.5
		m.Notes = append(m.Notes, "description is close to several items")
	}
	return m
}

var abbreviations = map[string][]string{
	"ss": {"stainless", "steel"}, "zp": {"zinc", "plated"}, "brg": {"bearing"},
	"hyd": {"hydraulic"}, "vlv": {"valve"}, "dgbb": {"deep", "groove", "ball", "bearing"},
}

var stopwords = map[string]bool{"x": true, "mm": true, "of": true, "with": true, "and": true, "for": true, "the": true}

// Tokens lower-cases s, splits it at punctuation and at letter/digit
// boundaries, expands a few trade abbreviations and drops noise words.
func Tokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		t := string(cur)
		cur = cur[:0]
		if exp, ok := abbreviations[t]; ok {
			out = append(out, exp...)
			return
		}
		if stopwords[t] || (len(t) == 1 && unicode.IsLetter(rune(t[0]))) {
			return
		}
		out = append(out, t)
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if len(cur) > 0 && unicode.IsDigit(cur[len(cur)-1]) != unicode.IsDigit(r) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// numbersAgree reports whether every number in the written description also
// appears in the catalog description. Sizes are where near-miss SKUs differ.
func numbersAgree(written, cat []string) bool {
	have := map[string]bool{}
	for _, t := range cat {
		have[t] = true
	}
	for _, t := range written {
		if t[0] >= '0' && t[0] <= '9' && !have[t] {
			return false
		}
	}
	return true
}

// Jaccard is |a ∩ b| / |a ∪ b| over token sets.
func Jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	sa := map[string]bool{}
	for _, t := range a {
		sa[t] = true
	}
	sb := map[string]bool{}
	for _, t := range b {
		sb[t] = true
	}
	inter := 0
	for t := range sa {
		if sb[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}

// Distance is the optimal-string-alignment edit distance (Levenshtein plus
// adjacent transpositions).
func Distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	n, m := len(ra), len(rb)
	d := make([][]int, n+1)
	for i := range d {
		d[i] = make([]int, m+1)
		d[i][0] = i
	}
	for j := range m + 1 {
		d[0][j] = j
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[n][m]
}
