// Package pipeline wires the stages together: load, extract, validate, score,
// match and route.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/adapters"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/catalog"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/extract"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/schema"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/score"
)

// Decisions.
const (
	AutoAccept = "auto_accept"
	Review     = "review"
)

// DefaultThreshold is the document confidence needed to skip human review.
// The eval's threshold sweep is how you choose it; see results/baseline.md.
const DefaultThreshold = 0.85

// Pipeline is safe for concurrent use if its extractors are.
type Pipeline struct {
	Primary   extract.Extractor
	Fallback  extract.Extractor // optional; tried when Primary is unsure or cannot read the input
	Catalog   *catalog.Catalog
	Threshold float64
}

// Result is everything known about one document after processing.
type Result struct {
	ID         string             `json:"id"`
	Source     string             `json:"source"`
	SHA256     string             `json:"sha256"`
	Format     string             `json:"format"`
	Extractor  string             `json:"extractor,omitempty"`
	Decision   string             `json:"decision"`
	Confidence float64            `json:"confidence"`
	Reasons    []string           `json:"reasons,omitempty"`
	Document   *po.Document       `json:"document,omitempty"`
	Fields     map[string]float64 `json:"field_confidence,omitempty"`
	Checks     []score.Check      `json:"checks,omitempty"`
	Matches    []catalog.Match    `json:"matches,omitempty"`
	Usage      []extract.Usage    `json:"llm_usage,omitempty"`
}

// RunFile loads and processes path. Load failures become review items with
// the reason, never silent drops.
func (p *Pipeline) RunFile(ctx context.Context, path string) *Result {
	in, err := adapters.Load(ctx, path)
	if err != nil {
		return &Result{Source: path, Decision: Review, Reasons: []string{"could not read input: " + err.Error()}}
	}
	in.Path = path
	return p.Run(ctx, in)
}

// Run processes a loaded input.
func (p *Pipeline) Run(ctx context.Context, in *adapters.Input) *Result {
	res := &Result{ID: in.SHA256[:16], Source: in.Path, SHA256: in.SHA256, Format: in.Format, Decision: Review}

	attempt := func(x extract.Extractor) (*Result, error) {
		r := *res
		ex, err := x.Extract(ctx, in)
		if ex != nil && ex.Usage != nil {
			r.Usage = append(slices.Clone(res.Usage), *ex.Usage)
		}
		if err != nil {
			return &r, err
		}
		p.evaluate(&r, ex, in)
		return &r, nil
	}

	r, err := attempt(p.Primary)
	if err != nil {
		r.Reasons = []string{fmt.Sprintf("%s: %v", p.Primary.Name(), err)}
	}
	if p.Fallback != nil && (err != nil || r.Decision == Review) {
		r2, err2 := attempt(p.Fallback)
		r2.Usage = append(r.Usage, r2.Usage...)
		switch {
		case err2 != nil:
			r.Reasons = append(r.Reasons, fmt.Sprintf("%s: %v", p.Fallback.Name(), err2))
			r.Usage = r2.Usage
		case err != nil || r2.Confidence > r.Confidence:
			r = r2
		}
	}
	if errors.Is(err, extract.ErrNeedsVision) && p.Fallback == nil {
		r.Reasons = append(r.Reasons, "set ANTHROPIC_API_KEY and use -extractor llm or cascade to read images")
	}
	return r
}

func (p *Pipeline) evaluate(r *Result, ex *extract.Extraction, in *adapters.Input) {
	r.Extractor = ex.Extractor
	doc := ex.Doc
	if len(doc.LineItems) == 0 {
		r.Decision, r.Confidence, r.Document = Review, 0, &doc
		r.Reasons = append(r.Reasons, "no line items found")
		return
	}
	// Every extraction is validated, including the rule-based one: the
	// schema is the contract with downstream systems, not an LLM safeguard.
	raw, _ := json.Marshal(doc)
	if err := schema.PO.ValidateJSON(raw); err != nil {
		r.Decision, r.Confidence = Review, 0
		r.Reasons = append(r.Reasons, err.Error())
		r.Document = &doc
		return
	}
	a := score.Assess(ex, in.Text)
	r.Document, r.Fields, r.Checks = &doc, a.Fields, a.Checks
	conf := a.Confidence
	if a.Weakest != "" && a.Fields[a.Weakest] < p.Threshold {
		r.Reasons = append(r.Reasons, fmt.Sprintf("low confidence in %s (%.2f)", a.Weakest, a.Fields[a.Weakest]))
	}
	for _, c := range a.Checks {
		if !c.Passed {
			r.Reasons = append(r.Reasons, fmt.Sprintf("check %s failed: %s", c.Name, c.Detail))
		}
	}
	for i, ln := range doc.LineItems {
		m := p.Catalog.Match(ln.PartNumber, ln.Description, ln.Unit)
		r.Matches = append(r.Matches, m)
		if m.Confidence < conf {
			conf = m.Confidence
		}
		if m.Confidence < p.Threshold {
			reason := fmt.Sprintf("line %d: catalog match %s %q (%.2f)", i+1, m.Method, m.SKU, m.Confidence)
			if len(m.Notes) > 0 {
				reason += ": " + m.Notes[0]
			}
			r.Reasons = append(r.Reasons, reason)
		}
	}
	for _, n := range ex.Notes {
		r.Reasons = append(r.Reasons, "note: "+n)
	}
	r.Confidence = conf
	if conf >= p.Threshold {
		r.Decision = AutoAccept
	}
}
