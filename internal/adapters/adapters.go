// Package adapters turns an input file into something an extractor can read:
// plain text for text-bearing formats, raw bytes for images.
//
// Tables (CSV, XLSX) are rendered as text rows with " | " between cells, so a
// single text-oriented extractor can handle every non-image format and the LLM
// sees the same representation the rules do.
package adapters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Formats.
const (
	FormatText  = "txt"
	FormatEmail = "eml"
	FormatCSV   = "csv"
	FormatXLSX  = "xlsx"
	FormatPDF   = "pdf"
	FormatImage = "image"
)

// MaxFileBytes bounds how much of any input file is read.
const MaxFileBytes = 20 << 20

// Sentinel errors. Callers route these to human review with a clear reason.
var (
	ErrUnsupported  = errors.New("unsupported file type")
	ErrNoPDFTool    = errors.New("pdftotext not found on PATH (install poppler-utils)")
	ErrNoTextLayer  = errors.New("PDF has no text layer (scanned?); needs a vision extractor")
	ErrTooLarge     = errors.New("file exceeds size limit")
	ErrEmptyContent = errors.New("no readable content")
)

// Input is a loaded document.
type Input struct {
	Path      string
	Format    string
	SHA256    string
	Text      string // empty for images
	Image     []byte // only for images
	MediaType string // only for images
}

// Load reads path and converts it according to its extension (and, for
// images, its sniffed content type).
func Load(ctx context.Context, path string) (*Input, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, ErrTooLarge
	}
	return FromBytes(ctx, filepath.Base(path), data)
}

// FromBytes converts data, using name only for its extension.
func FromBytes(ctx context.Context, name string, data []byte) (*Input, error) {
	sum := sha256.Sum256(data)
	in := &Input{Path: name, SHA256: hex.EncodeToString(sum[:])}
	var err error
	switch ext := strings.ToLower(filepath.Ext(name)); ext {
	case ".txt", ".text", ".md":
		in.Format, in.Text = FormatText, cleanText(data)
	case ".eml":
		in.Format = FormatEmail
		in.Text, err = parseEmail(data)
	case ".csv":
		in.Format = FormatCSV
		in.Text, err = parseCSV(data)
	case ".xlsx":
		in.Format = FormatXLSX
		in.Text, err = parseXLSX(data)
	case ".pdf":
		in.Format = FormatPDF
		in.Text, err = pdfToText(ctx, data)
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		mt := http.DetectContentType(data)
		if !strings.HasPrefix(mt, "image/") {
			return nil, fmt.Errorf("%w: extension %s but content is %s", ErrUnsupported, ext, mt)
		}
		in.Format, in.Image, in.MediaType = FormatImage, data, mt
		return in, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, ext)
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, ErrEmptyContent
	}
	return in, nil
}

func cleanText(b []byte) string {
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimPrefix(s, "\ufeff")
}

// --- email ---

func parseEmail(data []byte) (string, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("eml: %w", err)
	}
	dec := new(mime.WordDecoder)
	subject, _ := dec.DecodeHeader(msg.Header.Get("Subject"))
	from, _ := dec.DecodeHeader(msg.Header.Get("From"))
	body, err := textBody(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body)
	if err != nil {
		return "", err
	}
	return "Subject: " + subject + "\nFrom: " + from + "\n\n" + body, nil
}

func textBody(contentType, encoding string, r io.Reader) (string, error) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || contentType == "" {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(r, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				return "", fmt.Errorf("eml: no text/plain part")
			}
			if err != nil {
				return "", fmt.Errorf("eml: %w", err)
			}
			pt := p.Header.Get("Content-Type")
			if pt == "" || strings.HasPrefix(pt, "text/plain") || strings.HasPrefix(pt, "multipart/") {
				return textBody(pt, p.Header.Get("Content-Transfer-Encoding"), p)
			}
		}
	}
	if mt != "text/plain" {
		return "", fmt.Errorf("eml: unsupported body type %s", mt)
	}
	switch strings.ToLower(encoding) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64Reader(r)
	}
	b, err := io.ReadAll(io.LimitReader(r, MaxFileBytes))
	if err != nil {
		return "", fmt.Errorf("eml: %w", err)
	}
	return cleanText(b), nil
}

// --- CSV ---

func parseCSV(data []byte) (string, error) {
	text := cleanText(data)
	first, _, _ := strings.Cut(text, "\n")
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	if strings.Count(first, ";") > strings.Count(first, ",") {
		r.Comma = ';'
	}
	rows, err := r.ReadAll()
	if err != nil {
		return "", fmt.Errorf("csv: %w", err)
	}
	return renderRows(rows), nil
}

// renderRows joins cells with " | " and drops trailing empty cells.
func renderRows(rows [][]string) string {
	var b strings.Builder
	for _, row := range rows {
		end := len(row)
		for end > 0 && strings.TrimSpace(row[end-1]) == "" {
			end--
		}
		cells := make([]string, end)
		for i := range end {
			cells[i] = strings.TrimSpace(row[i])
		}
		b.WriteString(strings.Join(cells, " | "))
		b.WriteByte('\n')
	}
	return b.String()
}

// --- PDF ---

func pdfToText(ctx context.Context, data []byte) (string, error) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", ErrNoPDFTool
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Read the PDF from stdin and write text to stdout: no temp files, and
	// argv is fixed, so nothing from the document reaches a command line.
	cmd := exec.CommandContext(ctx, bin, "-layout", "-enc", "UTF-8", "-q", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: MaxFileBytes}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %w", err)
	}
	text := cleanText(out.Bytes())
	if strings.TrimSpace(strings.ReplaceAll(text, "\f", "")) == "" {
		return "", ErrNoTextLayer
	}
	return strings.ReplaceAll(text, "\f", "\n"), nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, ErrTooLarge
	}
	l.n -= len(p)
	return l.w.Write(p)
}
