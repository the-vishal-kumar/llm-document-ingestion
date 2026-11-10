package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
)

func TestApplySet(t *testing.T) {
	base := po.Document{DocType: po.TypePO, PONumber: "PO-1", LineItems: []po.Line{{Quantity: 1, Unit: "EA"}}}
	tests := []struct {
		set     string
		check   func(po.Document) bool
		wantErr bool
	}{
		{"po_number=PO-2", func(d po.Document) bool { return d.PONumber == "PO-2" }, false},
		{"total=12.5", func(d po.Document) bool { return d.Total != nil && *d.Total == 12.5 }, false},
		{"line_items[0].quantity=40", func(d po.Document) bool { return d.LineItems[0].Quantity == 40 }, false},
		{"line_items[0].unit_price=null", func(d po.Document) bool { return d.LineItems[0].UnitPrice == nil }, false},
		{"line_items[3].quantity=1", nil, true},
		{"approved=true", nil, true},
		{"total=abc", nil, true},
		{"nonsense", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.set, func(t *testing.T) {
			d := base
			d.LineItems = append([]po.Line(nil), base.LineItems...)
			err := applySet(&d, tt.set)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tt.check != nil && !tt.check(d) {
				t.Fatalf("doc = %+v", d)
			}
		})
	}
}

// End to end through the CLI: generate, run, review, correct.
func TestCLIFlow(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "var")
	ctx := context.Background()
	var out bytes.Buffer
	if err := run(ctx, []string{"gen", "-n", "8", "-seed", "5", "-out", dir}, &out); err != nil {
		t.Fatal(err)
	}
	txt, _ := filepath.Glob(filepath.Join(dir, "doc-*.txt"))
	csv, _ := filepath.Glob(filepath.Join(dir, "doc-*.csv"))
	files := append(txt, csv...)
	if len(files) == 0 {
		t.Fatal("no generated files")
	}
	out.Reset()
	if err := run(ctx, append([]string{"run", "-store", store}, files...), &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(ctx, []string{"review", "list", "-store", store}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "empty") {
		t.Skip("nothing queued for this seed")
	}
	id := strings.Fields(out.String())[0]
	out.Reset()
	if err := run(ctx, []string{"review", "correct", "-store", store, "-reviewer", "pat", "-set", "buyer=Harbor Valve Co", id}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store, "labeled", id+".json")); err != nil {
		t.Fatal(err)
	}
}
