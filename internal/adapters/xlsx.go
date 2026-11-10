package adapters

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// maxXLSXPart bounds the decompressed size of any one part, which defuses
// zip bombs.
const maxXLSXPart = 32 << 20

// parseXLSX reads the first worksheet of an Office Open XML workbook using
// only archive/zip and encoding/xml. It handles shared strings, inline
// strings, numbers and sparse rows; it ignores styles, formulas (it reads the
// cached value) and every sheet after the first.
func parseXLSX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("xlsx: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	read := func(name string) ([]byte, error) {
		f, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("xlsx: missing part %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxXLSXPart+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxXLSXPart {
			return nil, ErrTooLarge
		}
		return b, nil
	}

	var shared []string
	if _, ok := files["xl/sharedStrings.xml"]; ok {
		b, err := read("xl/sharedStrings.xml")
		if err != nil {
			return "", err
		}
		var sst struct {
			SI []struct {
				T string `xml:"t"`
				R []struct {
					T string `xml:"t"`
				} `xml:"r"`
			} `xml:"si"`
		}
		if err := xml.Unmarshal(b, &sst); err != nil {
			return "", fmt.Errorf("xlsx shared strings: %w", err)
		}
		for _, si := range sst.SI {
			s := si.T
			for _, r := range si.R {
				s += r.T
			}
			shared = append(shared, s)
		}
	}

	sheetPath, err := firstSheetPath(read)
	if err != nil {
		return "", err
	}
	b, err := read(sheetPath)
	if err != nil {
		return "", err
	}
	var ws struct {
		Rows []struct {
			Cells []struct {
				Ref  string `xml:"r,attr"`
				Type string `xml:"t,attr"`
				V    string `xml:"v"`
				IS   struct {
					T string `xml:"t"`
				} `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(b, &ws); err != nil {
		return "", fmt.Errorf("xlsx sheet: %w", err)
	}
	var rows [][]string
	for _, r := range ws.Rows {
		var row []string
		for i, c := range r.Cells {
			col := i
			if c.Ref != "" {
				if n, ok := columnIndex(c.Ref); ok {
					col = n
				}
			}
			if col > 16383 {
				return "", errors.New("xlsx: column out of range")
			}
			for len(row) <= col {
				row = append(row, "")
			}
			var val string
			switch c.Type {
			case "s":
				idx, err := strconv.Atoi(strings.TrimSpace(c.V))
				if err != nil || idx < 0 || idx >= len(shared) {
					return "", errors.New("xlsx: bad shared string index")
				}
				val = shared[idx]
			case "inlineStr":
				val = c.IS.T
			case "str", "b", "e":
				val = c.V
			default: // number
				val = c.V
				if f, err := strconv.ParseFloat(c.V, 64); err == nil {
					val = strconv.FormatFloat(f, 'f', -1, 64)
				}
			}
			row[col] = val
		}
		rows = append(rows, row)
	}
	return renderRows(rows), nil
}

// firstSheetPath resolves the first <sheet> in workbook.xml through the
// workbook relationships, falling back to the conventional path.
func firstSheetPath(read func(string) ([]byte, error)) (string, error) {
	const fallback = "xl/worksheets/sheet1.xml"
	wb, err := read("xl/workbook.xml")
	if err != nil {
		return "", err
	}
	var w struct {
		Sheets []struct {
			ID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(wb, &w); err != nil || len(w.Sheets) == 0 {
		return fallback, nil
	}
	rb, err := read("xl/_rels/workbook.xml.rels")
	if err != nil {
		return fallback, nil
	}
	var rels struct {
		R []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(rb, &rels); err != nil {
		return fallback, nil
	}
	for _, r := range rels.R {
		if r.ID == w.Sheets[0].ID {
			t := strings.TrimPrefix(r.Target, "/")
			if !strings.HasPrefix(t, "xl/") {
				t = path.Join("xl", t)
			}
			return path.Clean(t), nil
		}
	}
	return fallback, nil
}

// columnIndex converts the letters of a cell reference ("C7") to a 0-based
// column index.
func columnIndex(ref string) (int, bool) {
	n := 0
	i := 0
	for ; i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z'; i++ {
		n = n*26 + int(ref[i]-'A'+1)
	}
	if i == 0 {
		return 0, false
	}
	return n - 1, true
}

func base64Reader(r io.Reader) io.Reader {
	b, err := io.ReadAll(io.LimitReader(r, MaxFileBytes))
	if err != nil {
		return bytes.NewReader(nil)
	}
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, string(b))
	return base64.NewDecoder(base64.StdEncoding, strings.NewReader(clean))
}
