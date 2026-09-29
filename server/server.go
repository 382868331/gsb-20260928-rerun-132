// Package server 暴露导航服务的 JSON/HTTP 接口。
package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"nav/service"
)

type Server struct{ svc *service.Service }

func New(svc *service.Service) *Server { return &Server{svc: svc} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/files", s.handleFiles)
	mux.HandleFunc("/change", s.handleChange)
	mux.HandleFunc("/save", s.handleSave)
	mux.HandleFunc("/close", s.handleClose)
	mux.HandleFunc("/definition", s.handleDefinition)
	mux.HandleFunc("/references", s.handleReferences)
	return mux
}

type fileReq struct {
	URI     string `json:"uri"`
	Content string `json:"content"`
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req fileReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.svc.PutSaved(req.URI, req.Content)
	writeJSON(w, map[string]string{"status": "ok"})
}

type openReq struct {
	URI     string `json:"uri"`
	Content string `json:"content"`
}

func (s *Server) handleChange(w http.ResponseWriter, r *http.Request) {
	var req openReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.svc.Change(req.URI, req.Content))
}

type uriReq struct {
	URI string `json:"uri"`
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	var req uriReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.svc.Save(req.URI)
	writeJSON(w, map[string]string{"status": "saved"})
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	var req uriReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.svc.Close(req.URI)
	writeJSON(w, map[string]string{"status": "closed"})
}

type posReq struct {
	URI    string `json:"uri"`
	Offset int    `json:"offset"`
}

func (s *Server) handleDefinition(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePos(w, r)
	if !ok {
		return
	}
	res, found := s.svc.Definition(req.URI, req.Offset)
	if !found {
		writeJSON(w, map[string]any{"found": false, "snapshot_id": res.SnapshotID})
		return
	}
	writeJSON(w, map[string]any{"found": true, "result": res})
}

func (s *Server) handleReferences(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePos(w, r)
	if !ok {
		return
	}
	res, found := s.svc.References(req.URI, req.Offset)
	if !found {
		writeJSON(w, map[string]any{"found": false, "snapshot_id": res.SnapshotID})
		return
	}
	writeJSON(w, map[string]any{"found": true, "result": res})
}

func decodePos(w http.ResponseWriter, r *http.Request) (posReq, bool) {
	var req posReq
	if r.Method == http.MethodGet {
		req.URI = r.URL.Query().Get("uri")
		off, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil {
			http.Error(w, "bad offset", http.StatusBadRequest)
			return req, false
		}
		req.Offset = off
		return req, true
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
