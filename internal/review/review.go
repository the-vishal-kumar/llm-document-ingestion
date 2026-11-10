// Package review is the human-in-the-loop queue. It stores one JSON file per
// document under a root directory:
//
//	queue/<id>.json     waiting for a reviewer
//	accepted/<id>.json  final records (auto-accepted, approved or corrected)
//	labeled/<id>.json   reviewer decisions as labeled data for evals and tuning
//
// Plain files keep the reference dependency-free and easy to inspect. Writes
// are atomic (temp file + rename). There is no locking, so run one reviewer
// per store; a real deployment would put this behind a database.
package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/pipeline"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/schema"
)

// Store is a directory-backed queue.
type Store struct {
	Root string
	Now  func() time.Time
}

// Label is one reviewed document: what the pipeline predicted and what a
// human confirmed. Approvals are labels too (prediction == truth).
type Label struct {
	ID         string       `json:"id"`
	SHA256     string       `json:"sha256"`
	Source     string       `json:"source"`
	Format     string       `json:"format"`
	Extractor  string       `json:"extractor"`
	Predicted  *po.Document `json:"predicted"`
	Corrected  po.Document  `json:"corrected"`
	Changed    []string     `json:"changed_fields"`
	Reviewer   string       `json:"reviewer"`
	ReviewedAt time.Time    `json:"reviewed_at"`
}

var idRe = regexp.MustCompile(`^[a-f0-9]{16}$`)

// ErrNotFound means no queued item has that id.
var ErrNotFound = errors.New("no such item in the review queue")

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) path(dir, id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", fmt.Errorf("invalid id %q", id)
	}
	return filepath.Join(s.Root, dir, id+".json"), nil
}

// ErrAlreadyProcessed means the same input was accepted or labelled before.
var ErrAlreadyProcessed = errors.New("this input was already accepted or reviewed; not overwriting it")

// Save files a pipeline result as accepted or queued, by its decision. It
// never overwrites an accepted or human-labelled result: resubmitting the
// same bytes (a retried upload, a resent email) must not undo a correction.
func (s *Store) Save(r *pipeline.Result) (string, error) {
	if r.ID == "" {
		return "", errors.New("result has no id; nothing to store")
	}
	for _, dir := range []string{"accepted", "labeled"} {
		p, err := s.path(dir, r.ID)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(p); err == nil {
			return p, ErrAlreadyProcessed
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	dir := "queue"
	if r.Decision == pipeline.AutoAccept {
		dir = "accepted"
	}
	p, err := s.path(dir, r.ID)
	if err != nil {
		return "", err
	}
	return p, writeJSON(p, r)
}

// List returns queued items, oldest id first.
func (s *Store) List() ([]*pipeline.Result, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "queue"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*pipeline.Result
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idRe.MatchString(id) {
			continue
		}
		r, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

// Get loads a queued item.
func (s *Store) Get(id string) (*pipeline.Result, error) {
	p, err := s.path("queue", id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var r pipeline.Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("queue item %s: %w", id, err)
	}
	return &r, nil
}

// Approve accepts the extraction as-is.
func (s *Store) Approve(id, reviewer string) (*Label, error) {
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if r.Document == nil {
		return nil, errors.New("nothing was extracted; use correct with a full document")
	}
	return s.finish(r, *r.Document, reviewer)
}

// Correct replaces the extraction with doc, which must pass the schema.
func (s *Store) Correct(id string, doc po.Document, reviewer string) (*Label, error) {
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(doc)
	if err := schema.PO.ValidateJSON(raw); err != nil {
		return nil, err
	}
	return s.finish(r, doc, reviewer)
}

func (s *Store) finish(r *pipeline.Result, doc po.Document, reviewer string) (*Label, error) {
	if strings.TrimSpace(reviewer) == "" {
		return nil, errors.New("reviewer is required")
	}
	l := &Label{
		ID: r.ID, SHA256: r.SHA256, Source: r.Source, Format: r.Format, Extractor: r.Extractor,
		Predicted: r.Document, Corrected: doc, Changed: Diff(r.Document, doc),
		Reviewer: reviewer, ReviewedAt: s.now(),
	}
	lp, _ := s.path("labeled", r.ID)
	if err := writeJSON(lp, l); err != nil {
		return nil, err
	}
	final := *r
	final.Document = &doc
	final.Decision = "approved"
	if len(l.Changed) > 0 {
		final.Decision = "corrected"
	}
	ap, _ := s.path("accepted", r.ID)
	if err := writeJSON(ap, &final); err != nil {
		return nil, err
	}
	qp, _ := s.path("queue", r.ID)
	return l, os.Remove(qp)
}

// Diff lists the fields that differ between a prediction and a correction.
func Diff(pred *po.Document, got po.Document) []string {
	if pred == nil {
		return []string{"*"}
	}
	var out []string
	a, b := flatten(*pred), flatten(got)
	for k, v := range b {
		if a[k] != v {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func flatten(d po.Document) map[string]string {
	m := map[string]string{}
	raw, _ := json.Marshal(d)
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	for k, x := range v {
		if k == "line_items" {
			continue
		}
		m[k] = fmt.Sprint(x)
	}
	for i, ln := range d.LineItems {
		raw, _ := json.Marshal(ln)
		var lm map[string]any
		_ = json.Unmarshal(raw, &lm)
		for k, x := range lm {
			m[fmt.Sprintf("line_items[%d].%s", i, k)] = fmt.Sprint(x)
		}
	}
	return m
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
