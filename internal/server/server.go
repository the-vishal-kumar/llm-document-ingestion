// Package server exposes the pipeline over HTTP:
//
//	POST /v1/ingest?filename=order.csv   body: the raw file
//	GET  /healthz
//
// It returns the pipeline result as JSON and files it in the review store.
// There is no authentication: bind it to localhost or put it behind your
// gateway.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"time"

	"github.com/the-vishal-kumar/llm-document-ingestion/internal/adapters"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/pipeline"
	"github.com/the-vishal-kumar/llm-document-ingestion/internal/review"
)

// Server handles ingestion requests.
type Server struct {
	Pipeline *pipeline.Pipeline
	Store    *review.Store // optional
	Logger   *slog.Logger  // optional; slog.Default() when nil
	Timeout  time.Duration
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /v1/ingest", s.ingest)
	return mux
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("filename")
	if name != "" {
		name = filepath.Base(name)
	}
	if !safeName.MatchString(name) || name == "." || name == ".." {
		httpError(w, http.StatusBadRequest, "filename query parameter is required ([A-Za-z0-9._-], max 128)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, adapters.MaxFileBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpError(w, http.StatusRequestEntityTooLarge, "file too large")
			return
		}
		httpError(w, http.StatusBadRequest, "could not read body")
		return
	}
	ctx := r.Context()
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	in, err := adapters.FromBytes(ctx, name, body)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	res := s.Pipeline.Run(ctx, in)
	if s.Store != nil {
		if _, err := s.Store.Save(res); errors.Is(err, review.ErrAlreadyProcessed) {
			// A retried upload of the same bytes: report it, keep the earlier result.
			res.Reasons = append(res.Reasons, "already accepted or reviewed earlier; not stored again")
		} else if err != nil {
			s.logger().Error("store", "id", res.ID, "err", err)
			httpError(w, http.StatusInternalServerError, "could not store result")
			return
		}
	}
	// Log metadata only; document contents never reach the logs.
	s.logger().Info("ingested", "id", res.ID, "format", res.Format, "decision", res.Decision, "confidence", res.Confidence)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
