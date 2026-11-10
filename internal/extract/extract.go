// Package extract turns a loaded document into a po.Document with per-field
// confidences. Two implementations share one interface: a deterministic
// rule-based baseline (offline, free, used by CI and the eval) and an LLM
// extractor over HTTP.
package extract

import (
	"context"
	"errors"
	"fmt"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/adapters"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
)

// Extraction is an extractor's output. Conf maps a field key (see Key) to the
// extractor's confidence in that value, in [0, 1]. The scorer adjusts these
// with checks the extractor cannot see.
type Extraction struct {
	Doc       po.Document        `json:"document"`
	Conf      map[string]float64 `json:"field_confidence"`
	Extractor string             `json:"extractor"`
	Notes     []string           `json:"notes,omitempty"`
	Usage     *Usage             `json:"usage,omitempty"` // LLM tokens, for cost tracking
}

// Extractor produces an Extraction from an input.
type Extractor interface {
	Name() string
	Extract(ctx context.Context, in *adapters.Input) (*Extraction, error)
}

// ErrNeedsVision is returned for image inputs by extractors that cannot read
// images.
var ErrNeedsVision = errors.New("image input needs a vision-capable LLM extractor")

// LineKey names a field of line i, e.g. "line_items[2].quantity". Header
// fields use their JSON name ("po_number").
func LineKey(i int, field string) string { return fmt.Sprintf("line_items[%d].%s", i, field) }
