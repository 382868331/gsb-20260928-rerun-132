// Package server exposes the navigation workspace over HTTP.
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"sourcenav/internal/snapshot"
	"sourcenav/internal/workspace"
)

// Server wires one workspace to HTTP handlers.
type Server struct {
	ws *workspace.Workspace
}

// New creates a server backed by ws.
func New(ws *workspace.Workspace) *Server { return &Server{ws: ws} }

// Mux returns the configured HTTP handler.
func (s *Server) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("POST /open", s.open)
	mux.HandleFunc("POST /edit", s.edit)
	mux.HandleFunc("POST /save", s.save)
	mux.HandleFunc("POST /definition", s.definition)
	mux.HandleFunc("POST /references", s.references)
	mux.HandleFunc("POST /diagnostics", s.diagnostics)
	mux.HandleFunc("POST /rename/prepare", s.renamePrepare)
	mux.HandleFunc("POST /rename/apply", s.renameApply)
	mux.HandleFunc("POST /move/prepare", s.movePrepare)
	mux.HandleFunc("POST /move/apply", s.moveApply)
	mux.HandleFunc("POST /move/undo", s.moveUndo)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ws.Status())
}

type openReq struct {
	URI     string `json:"uri"`
	Text    string `json:"text"`
	Version int    `json:"version"`
	Saved   bool   `json:"saved"`
}

func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	var req openReq
	if !decode(w, r, &req) {
		return
	}
	vi := s.ws.Open(snapshot.URI(req.URI), req.Text, req.Version, req.Saved)
	writeJSON(w, http.StatusOK, vi)
}

type editReq struct {
	URI     string `json:"uri"`
	Text    string `json:"text"`
	Version int    `json:"version"`
}

func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	var req editReq
	if !decode(w, r, &req) {
		return
	}
	vi, err := s.ws.ApplyEdit(snapshot.URI(req.URI), req.Text, req.Version)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, vi)
}

type uriReq struct {
	URI string `json:"uri"`
}

func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	var req uriReq
	if !decode(w, r, &req) {
		return
	}
	vi, err := s.ws.Save(snapshot.URI(req.URI))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, vi)
}

type posReq struct {
	URI    string `json:"uri"`
	Offset int    `json:"offset"`
}

func (s *Server) definition(w http.ResponseWriter, r *http.Request) {
	var req posReq
	if !decode(w, r, &req) {
		return
	}
	res, err := s.ws.Definition(snapshot.URI(req.URI), req.Offset)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	var req uriReq
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uri":         req.URI,
		"diagnostics": s.ws.Diagnostics(snapshot.URI(req.URI)),
	})
}

type renamePrepareReq struct {
	URI     string `json:"uri"`
	Offset  int    `json:"offset"`
	NewName string `json:"newName"`
}

func (s *Server) renamePrepare(w http.ResponseWriter, r *http.Request) {
	var req renamePrepareReq
	if !decode(w, r, &req) {
		return
	}
	plan, err := s.ws.PrepareRename(snapshot.URI(req.URI), req.Offset, req.NewName)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) renameApply(w http.ResponseWriter, r *http.Request) {
	var plan workspace.RenamePlan
	if !decode(w, r, &plan) {
		return
	}
	res, err := s.ws.ApplyRename(plan)
	if err != nil {
		if errors.Is(err, workspace.ErrStaleRenamePlan) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) references(w http.ResponseWriter, r *http.Request) {
	var req posReq
	if !decode(w, r, &req) {
		return
	}
	res, err := s.ws.References(snapshot.URI(req.URI), req.Offset)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type movePrepareReq struct {
	URI        string `json:"uri"`
	Offset     int    `json:"offset"`
	DestURI    string `json:"destUri"`
	NewName    string `json:"newName"`
	KeepCompat bool   `json:"keepCompat"`
}

func (s *Server) movePrepare(w http.ResponseWriter, r *http.Request) {
	var req movePrepareReq
	if !decode(w, r, &req) {
		return
	}
	plan, err := s.ws.PrepareMove(snapshot.URI(req.URI), req.Offset, snapshot.URI(req.DestURI), req.NewName, req.KeepCompat)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) moveApply(w http.ResponseWriter, r *http.Request) {
	var plan workspace.MovePlan
	if !decode(w, r, &plan) {
		return
	}
	res, err := s.ws.ApplyMove(plan)
	if err != nil {
		if errors.Is(err, workspace.ErrStaleMovePlan) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) moveUndo(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	res, err := s.ws.UndoMove()
	if err != nil {
		if errors.Is(err, workspace.ErrMoveUndoConflict) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

type errBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errBody{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
