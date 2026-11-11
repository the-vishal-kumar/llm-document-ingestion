// Package eval scores the pipeline against synthetic ground truth and writes a
// markdown report. The report has no timestamps, so a given seed, size and
// extractor always produce the same bytes and CI can diff it.
package eval

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/pipeline"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/synth"
)

// Counts holds true positives, false positives and false negatives.
type Counts struct{ TP, FP, FN, Support int }

// Precision, Recall and F1 return NaN-free values (0 when undefined).
func (c Counts) Precision() float64 { return ratio(c.TP, c.TP+c.FP) }
func (c Counts) Recall() float64    { return ratio(c.TP, c.TP+c.FN) }
func (c Counts) F1() float64 {
	p, r := c.Precision(), c.Recall()
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// DocOutcome is the per-document result.
type DocOutcome struct {
	Name       string
	Format     string
	Decision   string
	Confidence float64
	Correct    bool
	Reasons    []string
	Mismatches []string // fields that differ from ground truth
	InTokens   int
	OutTokens  int
	LLMCalls   int
	LatencyMS  []int64 // one per LLM call
}

// Report aggregates an eval run.
type Report struct {
	Label     string
	Command   string
	Threshold float64
	Fields    map[string]*Counts
	Docs      []DocOutcome
	// List prices in USD per million tokens, used to estimate cost when
	// the run made LLM calls. Zero leaves cost out of the report.
	PriceIn, PriceOut float64
}

// FieldOrder is the row order of the field table.
var FieldOrder = append(append(slices.Clone(po.HeaderFields), po.LineFields...), "catalog_sku")

// Run writes samples into dir, runs each through p, and scores the results.
func Run(ctx context.Context, p *pipeline.Pipeline, samples []synth.Sample, dir string, workers int) (*Report, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	for _, s := range samples {
		if err := os.WriteFile(filepath.Join(dir, s.Name), s.Data, 0o600); err != nil {
			return nil, err
		}
	}
	results := make([]*pipeline.Result, len(samples))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, workers))
	for i, s := range samples {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = p.RunFile(ctx, filepath.Join(dir, s.Name))
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rep := &Report{Threshold: p.Threshold, Fields: map[string]*Counts{}}
	for _, f := range FieldOrder {
		rep.Fields[f] = &Counts{}
	}
	for i, s := range samples {
		r := results[i]
		mism := rep.score(s.Truth, r)
		o := DocOutcome{Name: s.Name, Format: s.Format, Decision: r.Decision, Confidence: r.Confidence,
			Correct: len(mism) == 0, Reasons: r.Reasons, Mismatches: mism}
		for _, u := range r.Usage {
			o.InTokens += u.InputTokens
			o.OutTokens += u.OutputTokens
			o.LLMCalls++
			o.LatencyMS = append(o.LatencyMS, u.LatencyMS)
		}
		rep.Docs = append(rep.Docs, o)
	}
	return rep, nil
}

func (rep *Report) count(field, truth, pred string) bool {
	c := rep.Fields[field]
	switch {
	case truth == "" && pred == "":
		return true
	case truth == "":
		c.FP++
		return false
	case pred == "":
		c.Support++
		c.FN++
		return false
	case truth == pred:
		c.Support++
		c.TP++
		return true
	default:
		c.Support++
		c.FP++
		c.FN++
		return false
	}
}

func num(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(math.Round(*v*1e4)/1e4, 'f', -1, 64)
}

func header(d *po.Document) map[string]string {
	if d == nil {
		return map[string]string{}
	}
	return map[string]string{
		"doc_type": d.DocType, "buyer": po.Fold(d.Buyer), "po_number": strings.TrimSpace(d.PONumber),
		"po_date": d.PODate, "delivery_date": d.DeliveryDate, "currency": d.Currency, "total": num(d.Total),
	}
}

func line(l po.Line) map[string]string {
	q := l.Quantity
	m := map[string]string{
		"part_number": strings.TrimSpace(l.PartNumber), "description": po.Fold(l.Description),
		"quantity": num(&q), "unit": l.Unit, "unit_price": num(l.UnitPrice),
	}
	if l.Quantity == 0 {
		m["quantity"] = ""
	}
	return m
}

// score updates field counts and returns the fields that differ from
// ground truth, catalog matches included. None means the document is right.
func (rep *Report) score(t synth.Truth, r *pipeline.Result) []string {
	var bad []string
	th, ph := header(&t.Document), header(r.Document)
	for _, f := range po.HeaderFields {
		if !rep.count(f, th[f], ph[f]) {
			bad = append(bad, f)
		}
	}
	var pred []po.Line
	if r.Document != nil {
		pred = r.Document.LineItems
	}
	n := max(len(t.Document.LineItems), len(pred))
	for i := range n {
		tl, pl := map[string]string{}, map[string]string{}
		tsku, psku := "", ""
		if i < len(t.Document.LineItems) {
			tl, tsku = line(t.Document.LineItems[i]), t.SKUs[i]
		}
		if i < len(pred) {
			pl = line(pred[i])
			if i < len(r.Matches) {
				psku = r.Matches[i].SKU
			}
		}
		for _, f := range po.LineFields {
			if !rep.count(f, tl[f], pl[f]) {
				bad = append(bad, fmt.Sprintf("line_items[%d].%s", i, f))
			}
		}
		if !rep.count("catalog_sku", tsku, psku) {
			bad = append(bad, fmt.Sprintf("line_items[%d].catalog_sku", i))
		}
	}
	return bad
}

// --- report ---

type routing struct {
	n, accepted, wrongAccepted, correct int
}

func (rep *Report) routing(th float64, filter func(DocOutcome) bool) routing {
	var r routing
	for _, d := range rep.Docs {
		if filter != nil && !filter(d) {
			continue
		}
		r.n++
		if d.Correct {
			r.correct++
		}
		// The pipeline auto-accepts exactly when confidence >= threshold, so
		// re-thresholding the recorded confidence reproduces its decision.
		if d.Confidence >= th {
			r.accepted++
			if !d.Correct {
				r.wrongAccepted++
			}
		}
	}
	return r
}

func pct(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
}

var (
	reIndex  = regexp.MustCompile(`\[\d+\]`)
	reParen  = regexp.MustCompile(`\s*\([^)]*\)`)
	reQuoted = regexp.MustCompile(`"[^"]*"`)
	reNumber = regexp.MustCompile(`\d+(\.\d+)?`)
)

func reasonKind(s string) string {
	s = reIndex.ReplaceAllString(s, "[i]")
	s = reParen.ReplaceAllString(s, "")
	s = reQuoted.ReplaceAllString(s, "…")
	s = reNumber.ReplaceAllString(s, "N")
	if k, _, ok := strings.Cut(s, ": not found in source"); ok {
		s = k + ": value not found in source"
	}
	return strings.TrimSpace(s)
}

// llmRows adds cost and latency rows when the run called a model. A
// rule-based run makes no calls, so its report stays byte-for-byte stable.
func (rep *Report) llmRows(w func(string, ...any)) {
	in, out, calls, withCall := 0, 0, 0, 0
	var lat []int64
	for _, d := range rep.Docs {
		in, out, calls = in+d.InTokens, out+d.OutTokens, calls+d.LLMCalls
		lat = append(lat, d.LatencyMS...)
		if d.LLMCalls > 0 {
			withCall++
		}
	}
	if calls == 0 {
		return
	}
	n := float64(len(rep.Docs))
	w("| Documents that called the LLM | %s (%d/%d) |\n", pct(withCall, len(rep.Docs)), withCall, len(rep.Docs))
	w("| LLM calls | %d |\n", calls)
	if in+out > 0 {
		w("| LLM tokens per document (mean in / out) | %.0f / %.0f |\n", float64(in)/n, float64(out)/n)
		if rep.PriceIn > 0 || rep.PriceOut > 0 {
			usd := (float64(in)*rep.PriceIn + float64(out)*rep.PriceOut) / 1e6
			w("| Estimated LLM cost per document (at $%g / $%g per M tokens in / out) | $%.4f |\n", rep.PriceIn, rep.PriceOut, usd/n)
		}
	}
	slices.Sort(lat)
	w("| LLM latency per call (p50 / p95 / max, retries included) | %s / %s / %s |\n",
		ms(quantile(lat, 0.5)), ms(quantile(lat, 0.95)), ms(lat[len(lat)-1]))
}

// quantile returns the nearest-rank quantile of sorted, which is non-empty.
func quantile(sorted []int64, q float64) int64 {
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func ms(v int64) string { return fmt.Sprintf("%.1f s", float64(v)/1000) }

// Markdown renders the report.
func (rep *Report) Markdown() string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	w("# Evaluation: %s\n\n", rep.Label)
	w("Produced by `%s`. Synthetic documents only; see `internal/synth` for how they are generated.\n\n", rep.Command)

	formats := map[string]int{}
	for _, d := range rep.Docs {
		formats[d.Format]++
	}
	keys := make([]string, 0, len(formats))
	for k := range formats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var mix []string
	for _, k := range keys {
		mix = append(mix, fmt.Sprintf("%s %d", k, formats[k]))
	}
	w("Documents: %d (%s).\n\n", len(rep.Docs), strings.Join(mix, ", "))

	all := rep.routing(rep.Threshold, nil)
	w("## Routing at threshold %.2f\n\n| Metric | Value |\n|---|---|\n", rep.Threshold)
	w("| Straight-through rate (auto-accepted / all) | %s (%d/%d) |\n", pct(all.accepted, all.n), all.accepted, all.n)
	w("| Error rate among auto-accepted | %s (%d/%d) |\n", pct(all.wrongAccepted, all.accepted), all.wrongAccepted, all.accepted)
	w("| Documents fully correct, any route | %s (%d/%d) |\n", pct(all.correct, all.n), all.correct, all.n)
	w("| Sent to human review | %d |\n", all.n-all.accepted)
	rep.llmRows(w)
	w("\nA document is \"fully correct\" when every header field, every line field and every catalog match equals ground truth.\n\n")

	w("## Field-level extraction\n\n| Field | Support | Precision | Recall | F1 |\n|---|---:|---:|---:|---:|\n")
	for _, f := range FieldOrder {
		c := rep.Fields[f]
		w("| %s | %d | %.3f | %.3f | %.3f |\n", f, c.Support, c.Precision(), c.Recall(), c.F1())
	}
	w("\nLine fields are aligned by position. Support counts ground-truth values that are present; a wrong value counts as one false positive and one false negative.\n\n")

	w("## By format\n\n| Format | Docs | Straight-through | Errors among auto-accepted | Fully correct |\n|---|---:|---:|---:|---:|\n")
	for _, k := range keys {
		r := rep.routing(rep.Threshold, func(d DocOutcome) bool { return d.Format == k })
		w("| %s | %d | %s | %s | %s |\n", k, r.n, pct(r.accepted, r.n), pct(r.wrongAccepted, r.accepted), pct(r.correct, r.n))
	}

	w("\n## Threshold sweep\n\nRe-thresholds the recorded document confidence. Use it to pick the operating point: lower thresholds buy straight-through rate with errors.\n\n")
	w("| Threshold | Straight-through | Errors among auto-accepted |\n|---:|---:|---:|\n")
	for _, th := range []float64{0.5, 0.6, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95} {
		r := rep.routing(th, nil)
		w("| %.2f | %s | %s (%d) |\n", th, pct(r.accepted, r.n), pct(r.wrongAccepted, r.accepted), r.wrongAccepted)
	}

	kinds := map[string]int{}
	reviewed := 0
	for _, d := range rep.Docs {
		if d.Decision == pipeline.AutoAccept {
			continue
		}
		reviewed++
		seen := map[string]bool{}
		for _, r := range d.Reasons {
			k := reasonKind(r)
			if !seen[k] {
				seen[k] = true
				kinds[k]++
			}
		}
	}
	type kv struct {
		k string
		n int
	}
	var top []kv
	for k, n := range kinds {
		top = append(top, kv{k, n})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].n != top[j].n {
			return top[i].n > top[j].n
		}
		return top[i].k < top[j].k
	})
	if len(top) > 12 {
		top = top[:12]
	}
	w("\n## Why documents went to review\n\nShare of the %d reviewed documents that carry each reason (a document can carry several).\n\n| Reason | Docs |\n|---|---:|\n", reviewed)
	for _, t := range top {
		w("| %s | %s |\n", strings.ReplaceAll(t.k, "|", "\\|"), pct(t.n, reviewed))
	}
	return b.String()
}
