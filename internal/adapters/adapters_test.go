package adapters_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os/exec"
	"strings"
	"testing"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/adapters"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/synth"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestFromBytes(t *testing.T) {
	xlsx := synth.WriteXLSX([][]any{
		{"PO Number", "PO-77"},
		{},
		{"Part No", "Qty", "Unit Price"},
		{"BRG-6204-2RS", 10.0, 2.5},
		{nil, nil, 0.1 + 0.2}, // sparse row, float noise
	})
	eml := "From: A <a@x.example>\r\nSubject: =?UTF-8?Q?PO_r=C3=A9f?=\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/html\r\n\r\n<p>html</p>\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nQty: 5 pcs =E2=82=AC\r\n--b1--\r\n"
	tests := []struct {
		name    string
		file    string
		data    []byte
		format  string
		want    []string // substrings of Text
		wantErr error
	}{
		{"text with BOM and CRLF", "a.txt", []byte("\ufeffPO Number: 1\r\nx"), adapters.FormatText, []string{"PO Number: 1\nx"}, nil},
		{"csv comma", "a.csv", []byte("Part,Qty\nA-1,5\n"), adapters.FormatCSV, []string{"Part | Qty", "A-1 | 5"}, nil},
		{"csv semicolon", "a.csv", []byte("Part;Qty;Price\nA-1;5;\"1,50\"\n"), adapters.FormatCSV, []string{"A-1 | 5 | 1,50"}, nil},
		{"xlsx shared strings and numbers", "a.xlsx", xlsx, adapters.FormatXLSX, []string{"PO Number | PO-77", "BRG-6204-2RS | 10 | 2.5", " |  | 0.3"}, nil},
		{"eml multipart quoted-printable", "a.eml", []byte(eml), adapters.FormatEmail, []string{"Subject: PO réf", "Qty: 5 pcs €"}, nil},
		{"image", "a.png", pngBytes(t), adapters.FormatImage, nil, nil},
		{"image extension but not an image", "a.png", []byte("hello"), "", nil, adapters.ErrUnsupported},
		{"unknown extension", "a.docx", []byte("x"), "", nil, adapters.ErrUnsupported},
		{"empty text", "a.txt", []byte("  \n"), "", nil, adapters.ErrEmptyContent},
		{"corrupt xlsx", "a.xlsx", []byte("not a zip"), "", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := adapters.FromBytes(context.Background(), tt.file, tt.data)
			if tt.format == "" {
				if err == nil {
					t.Fatal("expected an error")
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if in.Format != tt.format {
				t.Fatalf("format = %s, want %s", in.Format, tt.format)
			}
			for _, w := range tt.want {
				if !strings.Contains(in.Text, w) {
					t.Errorf("text %q missing %q", in.Text, w)
				}
			}
			if len(in.SHA256) != 64 {
				t.Errorf("sha256 = %q", in.SHA256)
			}
		})
	}
}

func TestPDFTextLayer(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	pdf := synth.WritePDF([]string{"PURCHASE ORDER", "Part No        Qty   Price", "HXB-M8-30-ZN   500   0.13"})
	in, err := adapters.FromBytes(context.Background(), "a.pdf", pdf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in.Text, "HXB-M8-30-ZN   500   0.13") {
		t.Fatalf("layout not preserved: %q", in.Text)
	}

	empty := synth.WritePDF(nil)
	if _, err := adapters.FromBytes(context.Background(), "b.pdf", empty); !errors.Is(err, adapters.ErrNoTextLayer) {
		t.Fatalf("err = %v, want ErrNoTextLayer", err)
	}
}
