// Package score turns an extractor's per-field confidences into a decision
// input. It applies checks that need no model: is each value actually in the
// source text (grounding), do line totals add up, are the dates sane, is the
// currency consistent, and does the document carry instruction-like text.
//
// Document confidence is the weakest required field, multiplied by a penalty
// for each failed cross-field check. Taking the minimum is deliberate: one
// wrong quantity makes the whole order wrong, so averages would hide exactly
// the errors that matter.
package score

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/extract"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
)

// Check is one cross-field or grounding check.
type Check struct {
	Name    string  `json:"name"`
	Passed  bool    `json:"passed"`
	Detail  string  `json:"detail,omitempty"`
	Penalty float64 `json:"penalty,omitempty"` // multiplier applied when failed
}

// Assessment is the scored extraction.
type Assessment struct {
	Fields     map[string]float64 `json:"field_confidence"`
	Checks     []Check            `json:"checks"`
	Confidence float64            `json:"confidence"`
	Weakest    string             `json:"weakest_field"`
}

var (
	numberTok   = regexp.MustCompile(`\d(?:[\d.,]*\d)?`)
	injectionRe = regexp.MustCompile(`(?i)(ignore|disregard|override)\s+(all\s+|any\s+|the\s+)?(previous|prior|above|earlier|system)\s+(instructions|rules|prompts?)|you are (an|a) (ai|assistant|language model)|note to (the )?(ai|assistant|model)|system prompt|auto-?approve`)
)

// Assess scores ex. source is the document text; pass "" for images, which
// disables grounding.
func Assess(ex *extract.Extraction, source string) Assessment {
	a := Assessment{Fields: map[string]float64{}}
	for k, v := range ex.Conf {
		a.Fields[k] = v
	}
	d := ex.Doc
	isPO := d.DocType != po.TypeRFQ

	if source != "" {
		a.ground(d, source)
		if m := injectionRe.FindString(source); m != "" {
			a.Checks = append(a.Checks, Check{Name: "instruction_like_text", Detail: "document contains text addressed to an AI system; a human must review", Penalty: 0.5})
		} else {
			a.Checks = append(a.Checks, Check{Name: "instruction_like_text", Passed: true})
		}
		a.currency(d, source)
	}
	if isPO {
		a.totals(d)
	}
	a.quantities(d)
	a.dates(d)

	// Required fields and their weakest confidence.
	// Dates are optional (chats rarely state one) but must be right if given.
	required := []string{"doc_type", "buyer", "po_number", "total"}
	if isPO {
		required = append(required, "currency")
	}
	if d.PODate != "" {
		required = append(required, "po_date")
	}
	if d.DeliveryDate != "" {
		required = append(required, "delivery_date")
	}
	lineFields := []string{"part_number", "description", "quantity", "unit"}
	if isPO {
		lineFields = append(lineFields, "unit_price")
	}
	for i := range d.LineItems {
		for _, f := range lineFields {
			required = append(required, extract.LineKey(i, f))
		}
	}
	conf := 1.0
	a.Weakest = ""
	for _, k := range required {
		if v := a.Fields[k]; v < conf {
			conf, a.Weakest = v, k
		}
	}
	if len(d.LineItems) == 0 {
		conf, a.Weakest = 0, "line_items"
	}
	for _, c := range a.Checks {
		if !c.Passed {
			conf *= c.Penalty
		}
	}
	a.Confidence = round(conf)
	return a
}

// ground lowers confidence for values that do not appear in the source. This
// is the main defence against hallucinated or injected values: an LLM can
// only score well by copying what the document says.
func (a *Assessment) ground(d po.Document, source string) {
	folded := po.Fold(source)
	nums := map[float64]bool{}
	for _, t := range numberTok.FindAllString(source, -1) {
		if f, ok := po.ParseNumber(t); ok {
			nums[f] = true
		}
	}
	var missing []string
	str := func(key, v string) {
		if v != "" && !strings.Contains(folded, po.Fold(v)) {
			a.Fields[key] *= 0.3
			missing = append(missing, key)
		}
	}
	num := func(key string, v *float64) {
		if v != nil && !nums[*v] && !nums[math.Round(*v*100)/100] {
			a.Fields[key] *= 0.3
			missing = append(missing, key)
		}
	}
	str("buyer", d.Buyer)
	str("po_number", d.PONumber)
	num("total", d.Total)
	for i, ln := range d.LineItems {
		str(extract.LineKey(i, "part_number"), ln.PartNumber)
		str(extract.LineKey(i, "description"), ln.Description)
		q := ln.Quantity
		num(extract.LineKey(i, "quantity"), &q)
		num(extract.LineKey(i, "unit_price"), ln.UnitPrice)
	}
	c := Check{Name: "grounding", Passed: len(missing) == 0, Penalty: 1}
	if !c.Passed {
		c.Detail = "not found in source: " + strings.Join(missing, ", ")
	}
	a.Checks = append(a.Checks, c)
}

func (a *Assessment) totals(d po.Document) {
	if d.Total == nil || len(d.LineItems) == 0 {
		return
	}
	sum := 0.0
	for _, ln := range d.LineItems {
		if ln.UnitPrice == nil {
			return
		}
		sum += ln.Quantity * *ln.UnitPrice
	}
	tol := math.Max(0.01, 0.005**d.Total)
	c := Check{Name: "line_totals", Passed: math.Abs(sum-*d.Total) <= tol, Penalty: 0.6}
	if !c.Passed {
		c.Detail = fmt.Sprintf("lines sum to %.2f, document total is %.2f", sum, *d.Total)
	}
	a.Checks = append(a.Checks, c)
}

// quantities flags fractional quantities of count units: 2.5 pieces is
// almost always a misread "2.500" or "2,5".
func (a *Assessment) quantities(d po.Document) {
	c := Check{Name: "count_quantities", Passed: true, Penalty: 0.6}
	for i, ln := range d.LineItems {
		switch ln.Unit {
		case "EA", "SET", "BOX":
			if ln.Quantity != math.Trunc(ln.Quantity) {
				c.Passed, c.Detail = false, fmt.Sprintf("line %d has a fractional quantity of a count unit", i+1)
			}
		}
	}
	a.Checks = append(a.Checks, c)
}

func (a *Assessment) dates(d po.Document) {
	c := Check{Name: "dates", Passed: true, Penalty: 0.7}
	var p, del time.Time
	var err error
	if d.PODate != "" {
		if p, err = time.Parse("2006-01-02", d.PODate); err != nil || p.Year() < 2000 || p.Year() > 2100 {
			c.Passed, c.Detail = false, "po_date out of range"
		}
	}
	if d.DeliveryDate != "" && c.Passed {
		if del, err = time.Parse("2006-01-02", d.DeliveryDate); err != nil {
			c.Passed, c.Detail = false, "delivery_date unparseable"
		} else if !p.IsZero() && del.Before(p) {
			c.Passed, c.Detail = false, "delivery_date is before po_date"
		} else if !p.IsZero() && del.After(p.AddDate(2, 0, 0)) {
			c.Passed, c.Detail = false, "delivery_date is more than two years after po_date"
		}
	}
	a.Checks = append(a.Checks, c)
}

func (a *Assessment) currency(d po.Document, source string) {
	seen := extract.CurrenciesIn(source)
	c := Check{Name: "currency_consistency", Passed: true, Penalty: 0.7}
	for cur := range seen {
		if d.Currency != "" && cur != d.Currency {
			c.Passed, c.Detail = false, fmt.Sprintf("document also mentions %s", cur)
		}
	}
	if d.Currency != "" && len(seen) > 0 && !seen[d.Currency] {
		c.Passed, c.Detail = false, "extracted currency is not marked in the document"
	}
	a.Checks = append(a.Checks, c)
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }
