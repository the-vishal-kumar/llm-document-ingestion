// Command ingest turns purchase orders and RFQs into validated JSON, matches
// their lines to a part catalog, and routes each document to auto-accept or
// human review.
//
//	ingest run [flags] <file>...          process documents
//	ingest review list|show|approve|correct  work the review queue
//	ingest eval [flags]                   score against synthetic ground truth
//	ingest gen [flags]                    write a synthetic dataset to disk
//	ingest serve [flags]                  HTTP endpoint (POST /v1/ingest)
//	ingest healthcheck [-addr host:port]  probe a running serve (for Docker)
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
	"slices"
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
  healthcheck                   exit 0 if a local serve answers GET /healthz

Run "ingest <command> -h" for flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	os.Exit(exitCode(run(ctx, os.Args[1:], os.Stdout), os.Stderr))
}

// Exit codes: 0 success (including -h), 1 the command failed, 2 the command
// line was wrong.
func exitCode(err error, stderr io.Writer) int {
	var ue usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ue):
		fmt.Fprintln(stderr, "ingest:", err)
		fmt.Fprintln(stderr, `run "ingest help" or "ingest <command> -h" for usage`)
		return 2
	default:
		fmt.Fprintln(stderr, "ingest:", err)
		return 1
	}
}

// usageError marks a mistake in the command line rather than a failure.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// newFlagSet returns a flag set whose -h output starts with a synopsis.
func newFlagSet(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: ingest %s\n\nflags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse wraps flag errors as usage errors; -h stays flag.ErrHelp.
func parse(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	return usageError{err.Error()}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return usagef("no command")
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
	case "healthcheck":
		return cmdHealthcheck(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(out, usage)
		return nil
	}
	return usagef("unknown command %q", args[0])
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
		err = usagef("unknown extractor %q (want rules, llm or cascade)", *f.extractor)
	}
	return p, err
}

func cmdRun(ctx context.Context, args []string, out io.Writer) error {
	fs := newFlagSet("run", "run [flags] <file>...")
	pf := addPipelineFlags(fs)
	storeDir := fs.String("store", "var", "review store directory")
	dry := fs.Bool("dry-run", false, "do not write to the store")
	asJSON := fs.Bool("json", false, "print full results as JSON")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return usagef("run: no files given")
	}
	p, err := pf.build()
	if err != nil {
		return err
	}
	store := &review.Store{Root: *storeDir}
	for _, path := range fs.Args() {
		res := p.RunFile(ctx, path)
		if !*dry && res.ID != "" {
			if _, err := store.Save(res); errors.Is(err, review.ErrAlreadyProcessed) {
				res.Reasons = append(res.Reasons, "already accepted or reviewed earlier; not stored again")
			} else if err != nil {
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
	if len(args) == 0 || !slices.Contains([]string{"list", "show", "approve", "correct"}, args[0]) {
		return usagef("review: want list, show, approve or correct")
	}
	sub := args[0]
	synopsis := map[string]string{
		"list":    "review list [-store dir]",
		"show":    "review show [-store dir] <id>",
		"approve": "review approve [-store dir] [-reviewer name] <id>",
		"correct": "review correct [-store dir] [-reviewer name] [-file doc.json] [-set field=value ...] <id>",
	}[sub]
	fs := newFlagSet("review "+sub, synopsis)
	storeDir := fs.String("store", "var", "review store directory")
	// Each subcommand shows only the flags it reads.
	reviewer, file := new(string), new(string)
	var sets multiFlag
	if sub == "approve" || sub == "correct" {
		fs.StringVar(reviewer, "reviewer", os.Getenv("USER"), "reviewer name recorded with the label")
	}
	if sub == "correct" {
		fs.StringVar(file, "file", "", "corrected document JSON; must pass the schema")
		fs.Var(&sets, "set", "field=value to change, e.g. po_number=PO-1 or line_items[0].quantity=5 (repeatable)")
	}
	if err := parse(fs, args[1:]); err != nil {
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
		return usagef("review %s: want exactly one id", sub)
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
	fs := newFlagSet("eval", "eval [flags]")
	pf := addPipelineFlags(fs)
	n := fs.Int("n", 300, "number of synthetic documents")
	seed := fs.Uint64("seed", 42, "dataset seed")
	report := fs.String("out", "", "write the markdown report here (default: stdout)")
	dir := fs.String("dir", "", "write the dataset here (default: a temp dir, removed afterwards)")
	workers := fs.Int("workers", 1, "documents processed concurrently")
	listErrors := fs.Bool("list-errors", false, "print every document that is not fully correct to stderr")
	priceIn := fs.Float64("usd-per-mtok-in", 2, "input list price in USD per million tokens, for the cost estimate (default: claude-sonnet-5-5)")
	priceOut := fs.Float64("usd-per-mtok-out", 10, "output list price in USD per million tokens, for the cost estimate")
	if err := parse(fs, args); err != nil {
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
	rep.PriceIn, rep.PriceOut = *priceIn, *priceOut
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
	fs := newFlagSet("gen", "gen -out <dir> [flags]")
	n := fs.Int("n", 20, "number of documents")
	seed := fs.Uint64("seed", 7, "dataset seed")
	dir := fs.String("out", "", "output directory (required)")
	truth := fs.Bool("truth", true, "also write <name>.truth.json files")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *dir == "" {
		return usagef("gen: -out is required")
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
	fs := newFlagSet("serve", "serve [flags]")
	pf := addPipelineFlags(fs)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	storeDir := fs.String("store", "var", "review store directory")
	if err := parse(fs, args); err != nil {
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

// cmdHealthcheck probes GET /healthz. The runtime image has no curl, so the
// Docker HEALTHCHECK calls the binary itself.
func cmdHealthcheck(ctx context.Context, args []string) error {
	fs := newFlagSet("healthcheck", "healthcheck [-addr host:port]")
	addr := fs.String("addr", "127.0.0.1:8080", "address serve listens on")
	if err := parse(fs, args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+*addr+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: HTTP %d", resp.StatusCode)
	}
	return nil
}
