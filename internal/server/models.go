package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/not-lucky/conflux/internal/config"
)

func (s *Server) handleModelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	infos := s.liveSnapshot().Registry.Enumerate()
	data := make([]any, 0, len(infos))
	for _, mi := range infos {
		data = append(data, map[string]any{
			"id":       mi.ID,
			"provider": mi.OwnedBy,
		})
	}
	out := map[string]any{"object": "list", "data": data}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func (s *Server) handleModelDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Path
	id = strings.TrimPrefix(id, "/v1/models/")
	id = strings.TrimPrefix(id, "models/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	reg := s.liveSnapshot().Registry
	if !reg.Lookup(id) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
		return
	}
	prov, _ := reg.Match(id)
	resp := map[string]any{"id": id, "provider": prov}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

func modelStrings(ms []config.ModelEntry) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Literal)
	}
	return out
}
