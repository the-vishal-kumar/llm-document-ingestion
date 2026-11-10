package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/catalog"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/extract"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/pipeline"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/review"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/synth"
)

func TestIngest(t *testing.T) {
	s := &Server{
		Pipeline: &pipeline.Pipeline{Primary: extract.Rules{}, Catalog: catalog.New(synth.Catalog()), Threshold: pipeline.DefaultThreshold},
		Store:    &review.Store{Root: t.TempDir()},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	csv := "Purchase Order\nBuyer,Harbor Valve Co\nPO Number,PO-9\nCurrency,USD\n\nPart No,Description,Qty,Unit,Unit Price\nBRG-6204-2RS,Deep groove ball bearing 6204 2RS,10,pcs,3.20\n"
	tests := []struct {
		name     string
		query    string
		body     string
		status   int
		decision string
	}{
		{"csv", "?filename=order.csv", csv, 200, pipeline.AutoAccept},
		{"missing filename", "", csv, 400, ""},
		{"path in filename is stripped", "?filename=../../x.csv", csv, 200, pipeline.AutoAccept},
		{"unsupported type", "?filename=order.docx", "x", 422, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/v1/ingest"+tt.query, "application/octet-stream", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.status {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tt.status, b)
			}
			if tt.decision != "" {
				var r pipeline.Result
				if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
					t.Fatal(err)
				}
				if r.Decision != tt.decision {
					t.Fatalf("decision = %s, reasons %v", r.Decision, r.Reasons)
				}
			}
		})
	}
}
