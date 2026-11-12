# Every target runs with a local Go toolchain. Without one, `make docker-ci`
# runs the same checks inside the official Go image.
GO          ?= go
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0
GO_IMAGE    := golang:1.27.4-bookworm
IMAGE       ?= llm-document-ingestion
COVERPROFILE ?= coverage.out

.PHONY: build test fmt-check vet staticcheck vuln lint eval eval-check eval-llm demo docker docker-ci ci clean

build:
	$(GO) build -trimpath -o bin/ingest ./cmd/ingest

test:
	$(GO) test -race -coverprofile=$(COVERPROFILE) ./...
	@$(GO) tool cover -func=$(COVERPROFILE) | tail -1

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

staticcheck:
	$(GO) run $(STATICCHECK) ./...

vuln:
	$(GO) run $(GOVULNCHECK) ./...

lint: fmt-check vet staticcheck

# Deterministic: same seed, same report. Needs pdftotext (poppler-utils).
# Seed 42 is the development set the rules were tuned on; seed 2026 is a
# larger held-out set that was only ever scored.
eval:
	$(GO) run ./cmd/ingest eval -extractor rules -n 300 -seed 42 -out results/baseline.md
	$(GO) run ./cmd/ingest eval -extractor rules -n 1000 -seed 2026 -out results/baseline-heldout.md

# Fails if the committed baselines no longer match what the code produces.
eval-check: eval
	git diff --exit-code results/

# Calls the model for every document: costs money and needs ANTHROPIC_API_KEY
# (or INGEST_LLM_PROVIDER=local with a claude-local-api endpoint).
eval-llm:
	$(GO) run ./cmd/ingest eval -extractor llm -n 300 -seed 42 -workers 4 -out results/llm.md
	$(GO) run ./cmd/ingest eval -extractor cascade -n 300 -seed 42 -workers 4 -out results/cascade.md

demo: build
	./bin/ingest run -store var examples/doc-*.txt examples/doc-*.eml examples/doc-*.csv examples/doc-*.xlsx examples/doc-*.pdf
	./bin/ingest review list -store var

docker:
	docker build -t $(IMAGE) .

docker-ci:
	docker run --rm -v "$$PWD":/src -w /src -e GOFLAGS=-buildvcs=false $(GO_IMAGE) sh -c \
	  'apt-get update -qq && apt-get install -y -qq --no-install-recommends poppler-utils >/dev/null && git config --global --add safe.directory /src && make ci COVERPROFILE=/tmp/coverage.out'

ci: lint test vuln eval-check

clean:
	rm -rf bin var coverage.out
