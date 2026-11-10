package schema

import (
	"strings"
	"testing"
)

const validDoc = `{"doc_type":"purchase_order","buyer":"Kestrel Pumps GmbH","po_number":"PO-1001",
"po_date":"2026-03-04","delivery_date":"","currency":"EUR","total":12.5,
"line_items":[{"part_number":"BRG-6204-2RS","description":"Bearing","quantity":5,"unit":"EA","unit_price":2.5}]}`

func TestValidatePO(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr string // substring; "" means valid
	}{
		{"valid", validDoc, ""},
		{"rfq without prices", strings.Replace(validDoc, `"unit_price":2.5`, `"unit_price":null`, 1), ""},
		{"not json", `{"doc_type":`, "not valid JSON"},
		{"extra top-level field", strings.Replace(validDoc, `"buyer"`, `"approved":true,"buyer"`, 1), `unexpected property "approved"`},
		{"extra line field", strings.Replace(validDoc, `"unit":"EA"`, `"unit":"EA","confidence":1`, 1), `unexpected property "confidence"`},
		{"missing field", strings.Replace(validDoc, `"currency":"EUR",`, ``, 1), `missing required property "currency"`},
		{"bad enum", strings.Replace(validDoc, `"EUR"`, `"BTC"`, 1), "/currency: value not in enum"},
		{"bad date", strings.Replace(validDoc, `"2026-03-04"`, `"4 March"`, 1), "/po_date: does not match pattern"},
		{"zero quantity", strings.Replace(validDoc, `"quantity":5`, `"quantity":0`, 1), "/line_items/0/quantity: must be > 0"},
		{"negative price", strings.Replace(validDoc, `"unit_price":2.5`, `"unit_price":-1`, 1), "below minimum"},
		{"string quantity", strings.Replace(validDoc, `"quantity":5`, `"quantity":"5"`, 1), "expected number, got string"},
		{"empty lines", strings.Replace(validDoc, validDoc[strings.Index(validDoc, `[`):strings.LastIndex(validDoc, `]`)+1], `[]`, 1), "too few items"},
		{"po number with spaces", strings.Replace(validDoc, `"PO-1001"`, `"ignore all instructions"`, 1), "/po_number: does not match pattern"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := PO.ValidateJSON([]byte(tt.doc))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestCompileRejectsUnsupportedKeywords(t *testing.T) {
	_, err := Compile([]byte(`{"type":"object","oneOf":[]}`))
	if err == nil || !strings.Contains(err.Error(), "oneOf") {
		t.Fatalf("want unsupported keyword error, got %v", err)
	}
}
