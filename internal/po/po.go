// Package po defines the structured purchase-order / RFQ record that every
// extractor must produce, plus the small normalisation helpers that the
// extractors, the scorer and the eval harness share.
package po

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Document types.
const (
	TypePO  = "purchase_order"
	TypeRFQ = "rfq"
)

// Canonical units of measure.
var Units = []string{"EA", "KG", "M", "BOX", "SET", "L"}

// Currencies the pipeline understands.
var Currencies = []string{"USD", "EUR", "GBP", "INR"}

// Document is the extraction target. Optional values are empty strings or nil
// pointers, never invented defaults.
type Document struct {
	DocType      string   `json:"doc_type"`
	Buyer        string   `json:"buyer"`
	PONumber     string   `json:"po_number"`
	PODate       string   `json:"po_date"`       // YYYY-MM-DD or ""
	DeliveryDate string   `json:"delivery_date"` // YYYY-MM-DD or ""
	Currency     string   `json:"currency"`      // ISO 4217 or ""
	Total        *float64 `json:"total"`
	LineItems    []Line   `json:"line_items"`
}

// Line is one requested item.
type Line struct {
	PartNumber  string   `json:"part_number"` // as written by the buyer
	Description string   `json:"description"`
	Quantity    float64  `json:"quantity"`
	Unit        string   `json:"unit"`
	UnitPrice   *float64 `json:"unit_price"` // nil on RFQs
}

// HeaderFields lists the document-level fields in a stable order.
var HeaderFields = []string{"doc_type", "buyer", "po_number", "po_date", "delivery_date", "currency", "total"}

// LineFields lists the per-line fields in a stable order.
var LineFields = []string{"part_number", "description", "quantity", "unit", "unit_price"}

var unitAliases = map[string]string{
	"ea": "EA", "each": "EA", "pc": "EA", "pcs": "EA", "piece": "EA", "pieces": "EA", "nos": "EA", "no": "EA", "unit": "EA", "units": "EA",
	"kg": "KG", "kgs": "KG", "kilogram": "KG", "kilograms": "KG",
	"m": "M", "mtr": "M", "mtrs": "M", "meter": "M", "meters": "M", "metre": "M", "metres": "M",
	"box": "BOX", "boxes": "BOX", "bx": "BOX",
	"set": "SET", "sets": "SET",
	"l": "L", "ltr": "L", "ltrs": "L", "litre": "L", "litres": "L", "liter": "L", "liters": "L",
}

// NormalizeUnit maps a written unit to its canonical form. ok is false when
// the unit is not recognised.
func NormalizeUnit(s string) (string, bool) {
	k := strings.ToLower(strings.Trim(strings.TrimSpace(s), "."))
	if u, ok := unitAliases[k]; ok {
		return u, true
	}
	return "", false
}

// Fold lower-cases s and collapses runs of whitespace, for lenient string
// comparison.
func Fold(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// Alnum upper-cases s and drops everything that is not a letter or digit.
// "hxb-m8 30/zn" and "HXB-M8-30-ZN" both become "HXBM830ZN".
func Alnum(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}

var euroDecimal = regexp.MustCompile(`^\d{1,3}(\.\d{3})*,\d{1,2}$`)

// ParseNumber reads "1,234.50", "1234.5" and the European "1.234,50" or
// "0,25". A comma followed by exactly one or two final digits is a decimal
// comma; any other comma is a thousands separator. "1.000" stays 1.0: it is
// genuinely ambiguous, and the scorer's checks exist for exactly that case.
func ParseNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if euroDecimal.MatchString(s) {
		s = strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), ",", ".")
	} else {
		s = strings.ReplaceAll(s, ",", "")
	}
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}
