package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

type patchRequest struct {
	Enabled bool `json:"enabled"`
}

func handleSites(state *State, stateFile string, reconcileCh chan<- struct{}, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sites := state.AllSites()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sites)
}

func handleSitePatch(state *State, stateFile string, reconcileCh chan<- struct{}, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/api/sites/")
	if name == "" {
		http.Error(w, "site name required", http.StatusBadRequest)
		return
	}
	decodedName, err := url.PathUnescape(name)
	if err != nil {
		http.Error(w, "invalid site name", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var payload patchRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	state.SetEnabled(decodedName, payload.Enabled)
	if err := state.Save(stateFile); err != nil {
		log.Printf("failed to save state: %v", err)
		http.Error(w, "failed to save state", http.StatusInternalServerError)
		return
	}
	sendReconcile(reconcileCh)
	w.WriteHeader(http.StatusNoContent)
}
