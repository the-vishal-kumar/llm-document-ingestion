// Command ingest turns purchase orders and RFQs into validated JSON, matches
// their lines to a part catalog, and routes each document to auto-accept or
// human review.
//
//	ingest run [flags] <file>...          process documents
//	ingest review list|show|approve|correct  work the review queue
//	ingest eval [flags]                   score against synthetic ground truth
//	ingest gen [flags]                    write a synthetic dataset to disk
//	ingest serve [flags]                  HTTP endpoint (POST /v1/ingest)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/catalog"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/eval"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/extract"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/pipeline"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/po"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/review"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/server"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/synth"
)

const usage = `usage: ingest <command> [flags]

commands:
  run <file>...                 extract, validate, match and route documents
  review list                   list queued documents
  review show <id>              show one queued document
  review approve <id>           accept the extraction as-is
  review correct <id> [-file doc.json] [-set field=value ...]
  eval                          score the pipeline on a synthetic dataset
  gen -out <dir>                write a synthetic dataset and catalog
  serve                         HTTP endpoint: POST /v1/ingest?filename=...

Run "ingest <command> -h" for flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ingest:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command")
	}
	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:], out)
	case "review":
		return cmdReview(args[1:], out)
	case "eval":
		return cmdEval(ctx, args[1:], out)
	case "gen":
		return cmdGen(args[1:], out)
	case "serve":
		return cmdServe(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(out, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

type pipelineFlags struct {
	extractor *string
	catalog   *string
	threshold *float64
}

func addPipelineFlags(fs *flag.FlagSet) pipelineFlags {
	return pipelineFlags{
		extractor: fs.String("extractor", "rules", "rules | llm | cascade (rules first, LLM when unsure)"),
		catalog:   fs.String("catalog", "", "catalog JSON (default: built-in synthetic catalog)"),
		threshold: fs.Float64("threshold", pipeline.DefaultThreshold, "document confidence needed to auto-accept"),
	}
}

func (f pipelineFlags) build() (*pipeline.Pipeline, error) {
	cat := catalog.New(synth.Catalog())
	if *f.catalog != "" {
		var err error
		if cat, err = catalog.Load(*f.catalog); err != nil {
			return nil, err
		}
	}
	p := &pipeline.Pipeline{Catalog: cat, Threshold: *f.threshold}
	llm := func() (extract.Extractor, error) {
		cfg, err := extract.LLMConfigFromEnv()
		if err != nil {
			return nil, err
		}
		return extract.NewLLM(cfg), nil
	}
	var err error
	switch *f.extractor {
	case "rules":
		p.Primary = extract.Rules{}
	case "llm":
		p.Primary, err = llm()
	case "cascade":
		p.Primary = extract.Rules{}
		p.Fallback, err = llm()
	default:
		err = fmt.Errorf("unknown extractor %q", *f.extractor)
	}
	return p, err
}

func cmdRun(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	pf := addPipelineFlags(fs)
	storeDir := fs.String("store", "var", "review store directory")
	dry := fs.Bool("dry-run", false, "do not write to the store")
	asJSON := fs.Bool("json", false, "print full results as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("run: no files given")
	}
	p, err := pf.build()
	if err != nil {
		return err
	}
	store := &review.Store{Root: *storeDir}
	for _, path := range fs.Args() {
		res := p.RunFile(ctx, path)
		if !*dry && res.ID != "" {
			if _, err := store.Save(res); err != nil {
				return err
			}
		}
		if *asJSON {
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Fprintln(out, string(b))
			continue
		}
		fmt.Fprintf(out, "%-12s %-40s %.2f  %s\n", res.Decision, filepath.Base(path), res.Confidence, res.ID)
		for _, r := range res.Reasons {
			fmt.Fprintf(out, "    - %s\n", r)
		}
	}
	return nil
}

func cmdReview(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("review: want list, show, approve or correct")
	}
	fs := flag.NewFlagSet("review "+args[0], flag.ContinueOnError)
	storeDir := fs.String("store", "var", "review store directory")
	reviewer := fs.String("reviewer", os.Getenv("USER"), "reviewer name recorded with the label")
	file := fs.String("file", "", "corrected document JSON (correct)")
	var sets multiFlag
	fs.Var(&sets, "set", "field=value to change, e.g. po_number=PO-1 or line_items[0].quantity=5 (repeatable)")
	sub := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	store := &review.Store{Root: *storeDir}
	switch sub {
	case "list":
		items, err := store.List()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Fprintln(out, "review queue is empty")
		}
		for _, r := range items {
			reason := ""
			if len(r.Reasons) > 0 {
				reason = r.Reasons[0]
			}
			fmt.Fprintf(out, "%s  %.2f  %-28s %s\n", r.ID, r.Confidence, filepath.Base(r.Source), reason)
		}
		return nil
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("review %s: want exactly one id", sub)
	}
	id := fs.Arg(0)
	switch sub {
	case "show":
		r, err := store.Get(id)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(out, string(b))
		return nil
	case "approve":
		l, err := store.Approve(id, *reviewer)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "approved %s; label saved\n", l.ID)
		return nil
	case "correct":
		r, err := store.Get(id)
		if err != nil {
			return err
		}
		var doc po.Document
		switch {
		case *file != "":
			b, err := os.ReadFile(*file)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(b, &doc); err != nil {
				return fmt.Errorf("%s: %w", *file, err)
			}
		case r.Document != nil:
			doc = *r.Document
		default:
			return errors.New("nothing was extracted; pass -file with a full document")
		}
		for _, s := range sets {
			if err := applySet(&doc, s); err != nil {
				return err
			}
		}
		l, err := store.Correct(id, doc, *reviewer)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "corrected %s (%s); label saved\n", l.ID, strings.Join(l.Changed, ", "))
		return nil
	}
	return fmt.Errorf("unknown review command %q", sub)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// applySet edits one field through the document's JSON form, so field names
// are the same ones the schema and the output use.
func applySet(doc *po.Document, kv string) error {
	key, val, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("-set %q: want field=value", kv)
	}
	raw, _ := json.Marshal(doc)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	target, field := m, key
	if rest, ok := strings.CutPrefix(key, "line_items["); ok {
		idxStr, f, ok := strings.Cut(rest, "].")
		i, err := strconv.Atoi(idxStr)
		lines, _ := m["line_items"].([]any)
		if !ok || err != nil || i < 0 || i >= len(lines) {
			return fmt.Errorf("-set %q: bad line index", kv)
		}
		target, field = lines[i].(map[string]any), f
	}
	if _, ok := target[field]; !ok {
		return fmt.Errorf("-set %q: unknown field %q", kv, field)
	}
	switch field {
	case "quantity", "unit_price", "total":
		if val == "" || val == "null" {
			target[field] = nil
			break
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return fmt.Errorf("-set %q: not a number", kv)
		}
		target[field] = f
	default:
		target[field] = val
	}
	raw, _ = json.Marshal(m)
	var out po.Document
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	*doc = out
	return nil
}

func cmdEval(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	pf := addPipelineFlags(fs)
	n := fs.Int("n", 300, "number of synthetic documents")
	seed := fs.Uint64("seed", 42, "dataset seed")
	report := fs.String("out", "", "write the markdown report here (default: stdout)")
	dir := fs.String("dir", "", "write the dataset here (default: a temp dir, removed afterwards)")
	workers := fs.Int("workers", 1, "documents processed concurrently")
	listErrors := fs.Bool("list-errors", false, "print every document that is not fully correct to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := pf.build()
	if err != nil {
		return err
	}
	work := *dir
	if work == "" {
		if work, err = os.MkdirTemp("", "ingest-eval-*"); err != nil {
			return err
		}
		defer os.RemoveAll(work)
	}
	rep, err := eval.Run(ctx, p, synth.Generate(*seed, *n), work, *workers)
	if err != nil {
		return err
	}
	rep.Label = map[string]string{"rules": "rule-based baseline", "llm": "LLM extractor", "cascade": "cascade (rules, then LLM)"}[*pf.extractor]
	rep.Command = fmt.Sprintf("ingest eval -extractor %s -n %d -seed %d -threshold %.2f", *pf.extractor, *n, *seed, *pf.threshold)
	if *listErrors {
		for _, d := range rep.Docs {
			if !d.Correct {
				fmt.Fprintf(os.Stderr, "%s %-11s %.2f wrong: %s\n", d.Name, d.Decision, d.Confidence, strings.Join(d.Mismatches, ", "))
			}
		}
	}
	md := rep.Markdown()
	if *report == "" {
		_, err = io.WriteString(out, md)
		return err
	}
	return os.WriteFile(*report, []byte(md), 0o644)
}

func cmdGen(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	n := fs.Int("n", 20, "number of documents")
	seed := fs.Uint64("seed", 7, "dataset seed")
	dir := fs.String("out", "", "output directory (required)")
	truth := fs.Bool("truth", true, "also write <name>.truth.json files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("gen: -out is required")
	}
	if err := os.MkdirAll(*dir, 0o750); err != nil {
		return err
	}
	for _, s := range synth.Generate(*seed, *n) {
		if err := os.WriteFile(filepath.Join(*dir, s.Name), s.Data, 0o644); err != nil {
			return err
		}
		if *truth {
			b, _ := json.MarshalIndent(s.Truth, "", "  ")
			if err := os.WriteFile(filepath.Join(*dir, strings.TrimSuffix(s.Name, filepath.Ext(s.Name))+".truth.json"), append(b, '\n'), 0o644); err != nil {
				return err
			}
		}
	}
	b, _ := json.MarshalIndent(synth.Catalog(), "", "  ")
	if err := os.WriteFile(filepath.Join(*dir, "catalog.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %d documents and catalog.json to %s\n", *n, *dir)
	return nil
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	pf := addPipelineFlags(fs)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	storeDir := fs.String("store", "var", "review store directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := pf.build()
	if err != nil {
		return err
	}
	s := &server.Server{Pipeline: p, Store: &review.Store{Root: *storeDir}, Logger: slog.Default(), Timeout: 5 * time.Minute}
	srv := &http.Server{Addr: *addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "addr", *addr, "extractor", *pf.extractor)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
