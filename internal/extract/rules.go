package extract

import (
	"context"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/adapters"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
)

// Rules is the deterministic baseline. It understands labelled header fields
// ("PO Number: ..."), item tables in any column order (pipe-, tab- or
// space-aligned), and terse chat-style item lines. It never sees images.
type Rules struct{}

// Name implements Extractor.
func (Rules) Name() string { return "rules" }

var headerLabels = map[string]string{}

func init() {
	for field, labels := range map[string][]string{
		"po_number": {"po number", "po no", "po", "purchase order number", "purchase order no", "purchase order",
			"order ref", "order reference", "order no", "order number", "rfq no", "rfq number", "rfq", "rfq ref", "reference", "ref"},
		"buyer":         {"buyer", "customer", "company", "bill to", "buyer name", "customer name"},
		"po_date":       {"po date", "order date", "date", "rfq date", "date issued", "issued", "issue date"},
		"delivery_date": {"delivery date", "required by", "need by", "delivery", "required delivery", "deliver by", "due date"},
		"currency":      {"currency", "ccy"},
		"total":         {"total", "grand total", "order total", "total amount", "total value"},
	} {
		for _, l := range labels {
			headerLabels[l] = field
		}
	}
}

const (
	colLine   = "line"
	colPart   = "part_number"
	colDesc   = "description"
	colQty    = "quantity"
	colUnit   = "unit"
	colPrice  = "unit_price"
	colAmount = "amount"
)

var columnLabels = map[string]string{
	"line": colLine, "#": colLine, "no": colLine, "sr": colLine, "sr no": colLine, "sl no": colLine, "s no": colLine, "item no": colLine,
	"part no": colPart, "part number": colPart, "part": colPart, "part #": colPart, "sku": colPart, "item code": colPart, "pn": colPart, "code": colPart,
	"description": colDesc, "desc": colDesc, "item": colDesc, "item description": colDesc, "material": colDesc,
	"qty": colQty, "quantity": colQty, "qnty": colQty,
	"unit": colUnit, "uom": colUnit, "units": colUnit,
	"unit price": colPrice, "price": colPrice, "rate": colPrice, "unit rate": colPrice, "price/unit": colPrice,
	"amount": colAmount, "line total": colAmount, "value": colAmount, "ext price": colAmount,
}

var (
	multiSpace  = regexp.MustCompile(`\s{2,}`)
	poFallback  = regexp.MustCompile(`\b([A-Z]{0,3}-?(?:PO|RFQ)[-/]?[A-Z0-9/]*\d{3,}[A-Z0-9-]*)\b`)
	rfqWord     = regexp.MustCompile(`\brfq\b`)
	chatLine    = regexp.MustCompile(`(?i)^(?:\[[^\]]*\]\s*[^:]{1,40}:\s*)?\W*(\d[\d,]*(?:\.\d+)?)\s*(pcs|pc|nos|ea|each|units?|kgs?|mtrs?|m|meters?|metres?|box(?:es)?|sets?|ltrs?|l|litres?)\.?\s+(?:of\s+|x\s+)?([A-Za-z0-9][A-Za-z0-9./-]{3,})\s*(.*?)\s*(?:@\s*(.+?))?\s*$`)
	currencyTok = regexp.MustCompile(`(?i)\b(?:USD|EUR|GBP|INR|Rs)\b\.?|US\$|\$|€|£|₹`)
)

// Extract implements Extractor.
func (Rules) Extract(_ context.Context, in *adapters.Input) (*Extraction, error) {
	if in.Format == adapters.FormatImage {
		return nil, ErrNeedsVision
	}
	p := &rulesParser{ex: &Extraction{Extractor: "rules", Conf: map[string]float64{}}, currencies: map[string]int{}}
	p.parse(in.Text)
	return p.ex, nil
}

type rulesParser struct {
	ex         *Extraction
	currencies map[string]int // currency evidence from money values
}

func (p *rulesParser) set(field string, conf float64) { p.ex.Conf[field] = conf }

func (p *rulesParser) parse(text string) {
	d := &p.ex.Doc
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "request for quotation") || rfqWord.MatchString(lower):
		d.DocType = po.TypeRFQ
		p.set("doc_type", 0.9)
	case strings.Contains(lower, "purchase order"):
		d.DocType = po.TypePO
		p.set("doc_type", 0.95)
	default:
		d.DocType = po.TypePO
		p.set("doc_type", 0.6)
	}

	lines := strings.Split(text, "\n")
	currencyLabel := ""
	tableFound := false
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if cols, split := tableHeader(line); cols != nil {
			i = p.parseTable(lines, i+1, cols, split)
			tableFound = true
			continue
		}
		key, val, ok := splitKV(line)
		if !ok {
			continue
		}
		field, ok := headerLabels[key]
		if !ok || p.ex.Conf[field] > 0 {
			continue
		}
		switch field {
		case "po_number":
			v := strings.Trim(strings.Fields(val)[0], ".,;")
			d.PONumber = v
			p.set(field, 0.95)
		case "buyer":
			d.Buyer = val
			p.set(field, 0.95)
		case "po_date", "delivery_date":
			iso, conf := ParseDate(val)
			if iso == "" {
				continue
			}
			if field == "po_date" {
				d.PODate = iso
			} else {
				d.DeliveryDate = iso
			}
			p.set(field, conf)
		case "currency":
			if c := currencyCode(val); c != "" {
				currencyLabel = c
				d.Currency = c
				p.set(field, 0.95)
			}
		case "total":
			if v, ok := p.money(val); ok {
				d.Total = &v
				p.set(field, 0.9)
			}
		}
	}

	if !tableFound {
		p.parseChat(lines)
	}

	if d.PONumber == "" {
		if m := poFallback.FindStringSubmatch(text); m != nil {
			d.PONumber = m[1]
			p.set("po_number", 0.75)
		}
	}
	if currencyLabel == "" {
		best, n, kinds := "", 0, 0
		for c, k := range p.currencies {
			kinds++
			if k > n || (k == n && c < best) {
				best, n = c, k
			}
		}
		switch {
		case kinds == 1:
			d.Currency = best
			p.set("currency", 0.85)
		case kinds > 1:
			d.Currency = best
			p.set("currency", 0.4)
			p.ex.Notes = append(p.ex.Notes, "several currencies appear in prices")
		}
	}
	if d.Total == nil {
		p.set("total", 0.9) // absent total is a valid reading, not a gap
	}
	if len(d.LineItems) == 0 {
		p.ex.Notes = append(p.ex.Notes, "no line items found")
	}
}

// tableHeader recognises an item-table header and returns the column roles in
// order, plus the splitter that separates its cells.
func tableHeader(line string) ([]string, func(string) []string) {
	split := splitterFor(line)
	cells := split(line)
	if len(cells) < 3 {
		return nil, nil
	}
	cols := make([]string, len(cells))
	known, hasQty := 0, false
	for i, c := range cells {
		role, ok := columnLabels[normLabel(c)]
		if !ok {
			cols[i] = ""
			continue
		}
		cols[i] = role
		known++
		hasQty = hasQty || role == colQty
	}
	if known < 3 || !hasQty {
		return nil, nil
	}
	return cols, split
}

func splitterFor(line string) func(string) []string {
	switch {
	case strings.Contains(line, "|"):
		return func(s string) []string {
			parts := strings.Split(s, "|")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			return parts
		}
	case strings.Contains(line, "\t"):
		return func(s string) []string {
			parts := strings.Split(s, "\t")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			return parts
		}
	default:
		return func(s string) []string { return multiSpace.Split(strings.TrimSpace(s), -1) }
	}
}

// parseTable reads rows until a blank line, a total line or a malformed row.
// It returns the index of the last consumed line.
func (p *rulesParser) parseTable(lines []string, start int, cols []string, split func(string) []string) int {
	hasUnitCol := false
	for _, c := range cols {
		hasUnitCol = hasUnitCol || c == colUnit
	}
	i := start
	for ; i < len(lines); i++ {
		raw := strings.TrimSpace(lines[i])
		if raw == "" || strings.Trim(raw, "-=_| ") == "" {
			if raw == "" {
				break
			}
			continue // separator rule
		}
		cells := split(raw)
		if first := normLabel(cells[0]); strings.Contains(first, "total") {
			return i - 1
		}
		if len(cells) < 2 {
			break
		}
		base := 0.95
		if len(cells) != len(cols) {
			base = 0.3 // misaligned row; positional guess only
		}
		idx := len(p.ex.Doc.LineItems)
		var ln po.Line
		conf := map[string]float64{}
		for j, role := range cols {
			if j >= len(cells) {
				break
			}
			v := cells[j]
			switch role {
			case colPart:
				if isBlank(v) {
					v = ""
				}
				ln.PartNumber = v
				conf[colPart] = base
			case colDesc:
				ln.Description = v
				conf[colDesc] = base
			case colQty:
				num, unit := splitQty(v)
				if q, ok := parseNumber(num); ok {
					ln.Quantity = q
					conf[colQty] = base
				}
				if !hasUnitCol && unit != "" {
					if u, ok := po.NormalizeUnit(unit); ok {
						ln.Unit = u
						conf[colUnit] = base - 0.05
					}
				}
			case colUnit:
				if u, ok := po.NormalizeUnit(v); ok {
					ln.Unit = u
					conf[colUnit] = base
				}
			case colPrice:
				if isBlank(v) {
					conf[colPrice] = base
					continue
				}
				if pr, ok := p.money(v); ok {
					ln.UnitPrice = &pr
					conf[colPrice] = base
				}
			}
		}
		p.addLine(idx, ln, conf)
	}
	return i
}

func (p *rulesParser) parseChat(lines []string) {
	for _, l := range lines {
		m := chatLine.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		q, ok := parseNumber(m[1])
		if !ok {
			continue
		}
		u, _ := po.NormalizeUnit(m[2])
		ln := po.Line{PartNumber: m[3], Description: strings.Trim(m[4], " -,()"), Quantity: q, Unit: u}
		if !strings.ContainsAny(ln.PartNumber, "0123456789") {
			// A word, not a part number: it belongs to the description.
			ln.Description = strings.TrimSpace(ln.PartNumber + " " + ln.Description)
			ln.PartNumber = ""
		}
		conf := map[string]float64{colPart: 0.7, colDesc: 0.6, colQty: 0.75, colUnit: 0.75, colPrice: 0.7}
		if m[5] != "" {
			if pr, ok := p.money(m[5]); ok {
				ln.UnitPrice = &pr
			}
		}
		p.addLine(len(p.ex.Doc.LineItems), ln, conf)
	}
	if len(p.ex.Doc.LineItems) > 0 {
		p.ex.Notes = append(p.ex.Notes, "line items parsed from free text")
	}
}

func (p *rulesParser) addLine(idx int, ln po.Line, conf map[string]float64) {
	p.ex.Doc.LineItems = append(p.ex.Doc.LineItems, ln)
	for _, f := range po.LineFields {
		p.ex.Conf[LineKey(idx, f)] = conf[f] // missing means 0
	}
}

// money parses a price, recording any currency marker as evidence.
func (p *rulesParser) money(s string) (float64, bool) {
	if c := currencyCode(s); c != "" {
		p.currencies[c]++
	}
	return parseNumber(currencyTok.ReplaceAllString(s, ""))
}

func currencyCode(s string) string {
	m := currencyTok.FindString(s)
	switch strings.ToUpper(strings.TrimSuffix(m, ".")) {
	case "USD", "US$", "$":
		return "USD"
	case "EUR", "€":
		return "EUR"
	case "GBP", "£":
		return "GBP"
	case "INR", "₹", "RS":
		return "INR"
	}
	return ""
}

func splitKV(line string) (key, val string, ok bool) {
	for _, sep := range []string{"|", ":", "\t"} {
		if k, v, found := strings.Cut(line, sep); found {
			v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), "|"))
			if sep == "|" {
				// Value is the first non-empty remaining cell.
				for _, c := range strings.Split(v, "|") {
					if c = strings.TrimSpace(c); c != "" {
						v = c
						break
					}
				}
			}
			if v == "" {
				return "", "", false
			}
			return normLabel(k), v, true
		}
	}
	if parts := multiSpace.Split(line, 2); len(parts) == 2 {
		return normLabel(parts[0]), strings.TrimSpace(parts[1]), true
	}
	return "", "", false
}

func normLabel(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer(".", " ", ":", " ", "_", " ").Replace(s)
	if strings.TrimSpace(s) != "#" {
		s = strings.ReplaceAll(s, "#", " ")
	}
	return strings.Join(strings.Fields(s), " ")
}

func isBlank(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "-", "--", "—", "n/a", "na", "tbd":
		return true
	}
	return false
}

// splitQty separates "500 pcs" into "500" and "pcs".
func splitQty(s string) (num, unit string) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool { return !(r >= '0' && r <= '9' || r == '.' || r == ',') })
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}

func parseNumber(s string) (float64, bool) {
	return po.ParseNumber(strings.Trim(strings.TrimSpace(s), "/-="))
}

var dateLayouts = []string{
	"2006-01-02", "02 Jan 2006", "2 Jan 2006", "January 2, 2006", "Jan 2, 2006", "2 January 2006",
	"02.01.2006", "2.1.2006", "02-Jan-2006", "2-Jan-2006", "Mon, 02 Jan 2006",
}

var slashDate = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})/(\d{4})$`)

// ParseDate normalises a written date to YYYY-MM-DD and says how sure it is.
// Slash dates are ambiguous (day-first vs month-first) unless one part is
// above 12; ambiguous ones are read day-first with low confidence.
func ParseDate(s string) (string, float64) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "."))
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format("2006-01-02"), 0.95
		}
	}
	if m := slashDate.FindStringSubmatch(s); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		day, month, conf := a, b, 0.5
		switch {
		case a > 12 && b <= 12:
			conf = 0.9
		case b > 12 && a <= 12:
			day, month, conf = b, a, 0.85
		}
		t := time.Date(y, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		if t.Day() == day && int(t.Month()) == month {
			return t.Format("2006-01-02"), conf
		}
	}
	// Excel stores dates as serial day numbers.
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 30000 && f < 80000 && f == math.Trunc(f) {
		t := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(f))
		return t.Format("2006-01-02"), 0.9
	}
	return "", 0
}

// CurrenciesIn returns every currency marked anywhere in text.
func CurrenciesIn(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range currencyTok.FindAllString(text, -1) {
		if c := currencyCode(m); c != "" {
			out[c] = true
		}
	}
	return out
}
