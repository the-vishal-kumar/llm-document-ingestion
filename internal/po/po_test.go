package po

import "testing"

func TestParseNumber(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"1234.5", 1234.5, true},
		{"1,234.50", 1234.5, true},
		{"1.234,50", 1234.5, true},
		{"0,25", 0.25, true},
		{" 12 ", 12, true},
		{"1,234", 1234, true},
		{"1.000", 1, true}, // ambiguous on purpose: see the doc comment
		{".5", 0.5, true},
		{"-3", -3, true},
		{"", 0, false},
		{"abc", 0, false},
		{"1e3", 0, false},
		{"0x1p4", 0, false},
		{"Inf", 0, false},
		{"NaN", 0, false},
		{"1.2.3", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := ParseNumber(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("ParseNumber(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestNormalizeUnit(t *testing.T) {
	for in, want := range map[string]string{"pcs": "EA", " Nos. ": "EA", "KGS": "KG", "mtr": "M", "Boxes": "BOX", "sets": "SET", "ltr": "L"} {
		if got, ok := NormalizeUnit(in); !ok || got != want {
			t.Errorf("NormalizeUnit(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := NormalizeUnit("furlong"); ok {
		t.Error("unknown unit accepted")
	}
}

func TestFoldAndAlnum(t *testing.T) {
	if got := Fold("  Harbor   VALVE\tCo "); got != "harbor valve co" {
		t.Errorf("Fold = %q", got)
	}
	if a, b := Alnum("hxb-m8 30/zn"), Alnum("HXB-M8-30-ZN"); a != "HXBM830ZN" || a != b {
		t.Errorf("Alnum = %q, %q", a, b)
	}
}
