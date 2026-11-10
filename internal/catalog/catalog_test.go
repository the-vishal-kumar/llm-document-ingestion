package catalog_test

import (
	"testing"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/catalog"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/synth"
)

func TestMatch(t *testing.T) {
	c := catalog.New(synth.Catalog())
	tests := []struct {
		name       string
		part, desc string
		unit       string
		wantSKU    string
		method     string
		minConf    float64
		maxConf    float64
	}{
		{"exact", "BRG-6204-2RS", "", "EA", "BRG-6204-2RS", catalog.MethodExact, 1, 1},
		{"normalized case and separators", "brg/6204/2rs", "", "", "BRG-6204-2RS", catalog.MethodNormalized, 0.95, 0.97},
		{"ocr typo confirmed by description", "CTY-2O0-BK", "Cable ties 200mm black box of 100", "BOX", "CTY-200-BK", catalog.MethodFuzzy, 0.9, 0.9},
		{"typo next to a SKU whose size contradicts the description", "HXB-M8-20-ZM", "Hex bolt M8 x 30mm zinc plated", "EA", "HXB-M8-20-ZN", catalog.MethodFuzzy, 0, 0.5},
		{"typo without description stays below threshold", "CTY-2O0-BK", "", "", "CTY-200-BK", catalog.MethodFuzzy, 0.5, 0.8},
		// No close runner-up, so the only thing keeping it from auto-accepting
		// is the uncorroborated-fuzzy ceiling itself.
		{"lone typo without description stays below threshold", "OIL-CUT-NEAI", "", "", "OIL-CUT-NEAT", catalog.MethodFuzzy, 0.5, 0.84},
		{"digit slip onto another valid SKU is caught", "HXB-M8-20-ZN", "Hex bolt M8 x 30mm zinc plated", "EA", "HXB-M8-20-ZN", catalog.MethodExact, 0, 0.5},
		{"description only", "", "Neat cutting oil", "L", "OIL-CUT-NEAT", catalog.MethodDescription, 0.5, 0.85},
		{"abbreviated description", "", "DGBB 6205 2RS", "EA", "BRG-6205-2RS", catalog.MethodDescription, 0.3, 0.85},
		{"unit disagrees", "HOS-HY-08", "", "EA", "HOS-HY-08", catalog.MethodExact, 0.7, 0.7},
		{"unknown part", "ZZZ-9999", "Widget", "", "", catalog.MethodNone, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := c.Match(tt.part, tt.desc, tt.unit)
			if m.SKU != tt.wantSKU || m.Method != tt.method || m.Confidence < tt.minConf || m.Confidence > tt.maxConf {
				t.Fatalf("got %+v; want %s via %s in [%.2f, %.2f]", m, tt.wantSKU, tt.method, tt.minConf, tt.maxConf)
			}
		})
	}
}

func TestDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"ABC", "ABC", 0}, {"ABC", "ACB", 1}, {"ABC", "AB", 1}, {"ABC", "XBC", 1}, {"KITTEN", "SITTING", 3},
	}
	for _, tt := range tests {
		if got := catalog.Distance(tt.a, tt.b); got != tt.want {
			t.Errorf("Distance(%q,%q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestTokens(t *testing.T) {
	got := catalog.Tokens("Hex bolt M8x30mm ZP")
	want := []string{"hex", "bolt", "8", "30", "zinc", "plated"}
	if len(got) != len(want) {
		t.Fatalf("Tokens = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Tokens = %v, want %v", got, want)
		}
	}
}
