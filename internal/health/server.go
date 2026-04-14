// Package health provides a minimal HTTP health-check server.
//
// Usage:
//
//	srv := health.NewServer(":8080", collectorID, logger)
//	srv.Start()
//	// later…
//	srv.Stop(ctx)
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// Server is a lightweight HTTP server that exposes a /health endpoint.
type Server struct {
	addr        string
	collectorID string
	logger      *slog.Logger
	srv         *http.Server
}

// NewServer constructs a Server. addr is the listen address (e.g. ":8080").
func NewServer(addr, collectorID string, logger *slog.Logger) *Server {
	return &Server{
		addr:        addr,
		collectorID: collectorID,
		logger:      logger,
	}
}

// Start registers the /health handler and begins listening in a background
// goroutine. It returns immediately.
func (s *Server) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)

	s.srv = &http.Server{Addr: s.addr, Handler: mux}

	go func() {
		s.logger.Info("health: server listening", "addr", s.addr)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("health: server error", "error", err.Error())
		}
	}()
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop(ctx context.Context) {
	if s.srv == nil {
		return
	}
	if err := s.srv.Shutdown(ctx); err != nil {
		s.logger.Error("health: shutdown error", "error", err.Error())
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":       "ok",
		"collector_id": s.collectorID,
	})
}
