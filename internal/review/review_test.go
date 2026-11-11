package review

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/pipeline"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
)

func sample(id, decision string) *pipeline.Result {
	return &pipeline.Result{
		ID: id, SHA256: id + id + id + id, Source: "in/" + id + ".txt", Format: "txt", Extractor: "rules", Decision: decision, Confidence: 0.5,
		Reasons: []string{"low confidence in buyer (0.00)"},
		Document: &po.Document{DocType: po.TypePO, PONumber: "PO-1", Currency: "USD",
			LineItems: []po.Line{{PartNumber: "A-1", Quantity: 5, Unit: "EA", UnitPrice: ptr(1)}}},
	}
}

func TestStoreLifecycle(t *testing.T) {
	s := &Store{Root: t.TempDir(), Now: func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }}
	const a, b, c = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"
	for _, r := range []*pipeline.Result{sample(a, pipeline.Review), sample(b, pipeline.Review), sample(c, pipeline.AutoAccept)} {
		if _, err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.List()
	if err != nil || len(items) != 2 {
		t.Fatalf("list = %d items, %v; auto-accepted must not be queued", len(items), err)
	}

	if _, err := s.Approve(a, "pat"); err != nil {
		t.Fatal(err)
	}
	doc := *sample(b, "").Document
	doc.Buyer = "Harbor Valve Co"
	doc.LineItems = []po.Line{{PartNumber: "A-1", Quantity: 50, Unit: "EA", UnitPrice: ptr(1)}}
	l, err := s.Correct(b, doc, "pat")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"buyer", "line_items[0].quantity"}; !reflect.DeepEqual(l.Changed, want) {
		t.Fatalf("changed = %v, want %v", l.Changed, want)
	}
	if items, _ := s.List(); len(items) != 0 {
		t.Fatalf("queue not drained: %d", len(items))
	}
	for _, p := range []string{"labeled/" + a, "labeled/" + b, "accepted/" + a, "accepted/" + b, "accepted/" + c} {
		if _, err := os.Stat(filepath.Join(s.Root, p+".json")); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func TestStoreRejects(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	const id = "dddddddddddddddd"
	if _, err := s.Save(sample(id, pipeline.Review)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		fn   func() error
	}{
		{"path traversal id", func() error { _, err := s.Get("../../etc/passwd"); return err }},
		{"unknown id", func() error { _, err := s.Approve("eeeeeeeeeeeeeeee", "pat"); return err }},
		{"no reviewer", func() error { _, err := s.Approve(id, " "); return err }},
		{"correction fails schema", func() error {
			_, err := s.Correct(id, po.Document{DocType: "invoice"}, "pat")
			return err
		}},
		{"result without id", func() error { _, err := s.Save(&pipeline.Result{}); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.fn(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, err := s.Get("eeeeeeeeeeeeeeee"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func ptr(v float64) *float64 { return &v }
