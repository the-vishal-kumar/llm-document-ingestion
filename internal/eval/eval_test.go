package eval

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/catalog"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/extract"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/pipeline"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/synth"
)

func TestGenerateIsReproducible(t *testing.T) {
	a, b := synth.Generate(9, 40), synth.Generate(9, 40)
	for i := range a {
		if a[i].Name != b[i].Name || !bytes.Equal(a[i].Data, b[i].Data) {
			t.Fatalf("sample %d differs between runs", i)
		}
	}
	if c := synth.Generate(10, 40); bytes.Equal(a[0].Data, c[0].Data) && bytes.Equal(a[1].Data, c[1].Data) {
		t.Fatal("different seeds produced the same documents")
	}
}

func TestCount(t *testing.T) {
	rep := &Report{Fields: map[string]*Counts{"x": {}}}
	for _, c := range [][2]string{{"a", "a"}, {"a", "b"}, {"a", ""}, {"", "b"}, {"", ""}} {
		rep.count("x", c[0], c[1])
	}
	got := *rep.Fields["x"]
	if got != (Counts{TP: 1, FP: 2, FN: 2, Support: 3}) {
		t.Fatalf("counts = %+v", got)
	}
	if p, r := got.Precision(), got.Recall(); p != 1.0/3 || r != 1.0/3 {
		t.Fatalf("p=%v r=%v", p, r)
	}
}

func TestRunSmall(t *testing.T) {
	samples := synth.Generate(3, 30)
	var noPDF []synth.Sample
	for _, s := range samples {
		if s.Format != "pdf" { // keep the unit test independent of poppler
			noPDF = append(noPDF, s)
		}
	}
	p := &pipeline.Pipeline{Primary: extract.Rules{}, Catalog: catalog.New(synth.Catalog()), Threshold: pipeline.DefaultThreshold}
	rep, err := Run(context.Background(), p, noPDF, t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	md := rep.Markdown()
	for _, want := range []string{"Straight-through rate", "| quantity |", "Threshold sweep"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if rep.Fields["po_number"].Support != len(noPDF) {
		t.Errorf("po_number support = %d, want %d", rep.Fields["po_number"].Support, len(noPDF))
	}
}

func emptyFields() map[string]*Counts {
	m := map[string]*Counts{}
	for _, f := range FieldOrder {
		m[f] = &Counts{}
	}
	return m
}

func TestLLMRows(t *testing.T) {
	rep := &Report{Fields: emptyFields(), PriceIn: 2, PriceOut: 10, Docs: []DocOutcome{
		{InTokens: 1000, OutTokens: 500, LLMCalls: 1, LatencyMS: []int64{1200}},
		{InTokens: 3000, OutTokens: 1500, LLMCalls: 2, LatencyMS: []int64{800, 4000}},
		{}, // the rules were sure: no call
	}}
	md := rep.Markdown()
	// (4000*2 + 2000*10) / 1e6 / 3 docs = $0.0093 per document.
	for _, want := range []string{"| Documents that called the LLM | 66.7% (2/3) |", "| LLM calls | 3 |", "$0.0093", "1.2 s / 4.0 s / 4.0 s"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains((&Report{Fields: emptyFields(), Docs: []DocOutcome{{}}}).Markdown(), "LLM") {
		t.Error("a run without LLM calls must not mention the LLM")
	}
}
