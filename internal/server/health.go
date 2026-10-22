package server

import (
	"context"
	"net/http"
)

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	databaseOK := false
	workerReady := false
	if s.database != nil {
		ctx, cancel := context.WithTimeout(r.Context(), pingTimeout)
		databaseOK = s.database.Ping(ctx) == nil
		cancel()
	}
	if s.worker != nil {
		workerReady = s.worker.Ready()
	}
	status := http.StatusOK
	state := "ready"
	if !databaseOK || !workerReady {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	database := "ok"
	if !databaseOK {
		database = "unavailable"
	}
	worker := "ready"
	if !workerReady {
		worker = "stopped"
	}
	writeJSON(w, status, map[string]string{
		"status":   state,
		"database": database,
		"worker":   worker,
	})
}
