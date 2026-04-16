package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type patchRequest struct {
	Enabled bool `json:"enabled"`
}

func handleSites(state *State, stateFile string, reconcileCh chan<- struct{}, cfClient *CloudflareClient, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sites := state.AllSites()
	for i := range sites {
		if sites[i].Enabled && sites[i].HasDNS {
			sites[i].HasWWW = cfClient.HasWWWForSite(sites[i].Name)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sites)
}

// handleDNSCheck resolves a site hostname using an external public resolver
// and reports whether the domain has live DNS. Only sites known to the state
// are permitted to prevent arbitrary DNS lookups.
func handleDNSCheck(state *State, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	site := r.URL.Query().Get("site")
	if site == "" {
		jsonError(w, "site query param required", http.StatusBadRequest)
		return
	}
	// Only allow lookups for sites registered in state.
	known := false
	for _, name := range state.AllSiteNames() {
		if name == site {
			known = true
			break
		}
	}
	if !known {
		jsonError(w, "site not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"live": dnsIsLive(site)})
}

func dnsIsLive(hostname string) bool {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "udp", "1.1.1.1:53")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := r.LookupHost(ctx, hostname)
	return err == nil && len(addrs) > 0
}

func handleSitePatch(state *State, stateFile string, reconcileCh chan<- struct{}, cfReconcileCh chan<- struct{}, w http.ResponseWriter, r *http.Request) {
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
	sendReconcile(cfReconcileCh)
	w.WriteHeader(http.StatusNoContent)
}

// subdomainRe validates a normalised subdomain label: lowercase alphanumeric
// with interior hyphens allowed, no leading/trailing hyphens.
var subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type createSiteRequest struct {
	Subdomain string `json:"subdomain"`
	Domain    string `json:"domain"`
	Template  string `json:"template"`
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func handleDomains(cfClient *CloudflareClient, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	domains := cfClient.AvailableDomains()
	if domains == nil {
		domains = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(domains)
}

func handleDeleteSite(state *State, stateFile, sitesDir string, reconcileCh, cfReconcileCh chan<- struct{}, logger *log.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
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

	// Guard: site must exist in state and be disabled.
	found := false
	for _, sv := range state.AllSites() {
		if sv.Name == decodedName {
			found = true
			if sv.Enabled {
				jsonError(w, "site must be disabled before deleting", http.StatusConflict)
				return
			}
			break
		}
	}
	if !found {
		http.Error(w, "site not found", http.StatusNotFound)
		return
	}

	// Build the site path and verify it is strictly under sitesDir (path traversal guard).
	safeBase := filepath.Clean(sitesDir)
	sitePath := filepath.Join(safeBase, decodedName)
	if sitePath == safeBase || !strings.HasPrefix(sitePath, safeBase+string(filepath.Separator)) {
		logger.Printf("delete site: computed path %q is outside sitesDir %q", sitePath, safeBase)
		http.Error(w, "invalid site path", http.StatusBadRequest)
		return
	}

	// Remove from state first, then delete the folder.
	state.RemoveSite(decodedName)
	if err := state.Save(stateFile); err != nil {
		logger.Printf("failed to save state after removing site %q: %v", decodedName, err)
		http.Error(w, "failed to save state", http.StatusInternalServerError)
		return
	}

	if err := os.RemoveAll(sitePath); err != nil {
		logger.Printf("failed to delete site directory %q: %v", sitePath, err)
		jsonError(w, "failed to delete site directory", http.StatusInternalServerError)
		return
	}

	sendReconcile(reconcileCh)
	sendReconcile(cfReconcileCh)
	w.WriteHeader(http.StatusNoContent)
}

func handleCreateSite(state *State, stateFile, sitesDir string, cfClient *CloudflareClient, reconcileCh chan<- struct{}, fileUID, fileGID int, logger *log.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var payload createSiteRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		jsonError(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}

	// Validate domain.
	if payload.Domain == "" {
		jsonError(w, "domain is required", http.StatusBadRequest)
		return
	}
	validDomains := cfClient.AvailableDomains()
	domainValid := false
	for _, d := range validDomains {
		if d == payload.Domain {
			domainValid = true
			break
		}
	}
	if !domainValid {
		jsonError(w, "domain is not in the configured zone map", http.StatusBadRequest)
		return
	}

	// Validate template.
	if payload.Template == "" {
		jsonError(w, "template is required", http.StatusBadRequest)
		return
	}
	templateValid := false
	for _, t := range availableTemplates {
		if t == payload.Template {
			templateValid = true
			break
		}
	}
	if !templateValid {
		jsonError(w, "unknown template", http.StatusBadRequest)
		return
	}

	// Normalise and validate subdomain.
	subdomain := strings.ToLower(strings.TrimSpace(payload.Subdomain))
	if subdomain != "" && !subdomainRe.MatchString(subdomain) {
		jsonError(w, "invalid subdomain: use lowercase letters, numbers, and hyphens only (no leading/trailing hyphens)", http.StatusBadRequest)
		return
	}

	// Build the site name (e.g. "blog.example.com" or "example.com").
	siteName := payload.Domain
	if subdomain != "" {
		siteName = subdomain + "." + payload.Domain
	}

	// Check for duplicates.
	for _, existing := range state.AllSiteNames() {
		if existing == siteName {
			jsonError(w, "site already exists", http.StatusBadRequest)
			return
		}
	}

	// Create the site folder from the template.
	if err := createSiteFromTemplate(sitesDir, siteName, payload.Template, fileUID, fileGID, logger); err != nil {
		logger.Printf("failed to create site %q from template: %v", siteName, err)
		jsonError(w, "failed to create site folder", http.StatusInternalServerError)
		return
	}

	// Register the site in state immediately (watcher will no-op on the
	// existing directory when it fires shortly after).
	state.AddSite(siteName)
	if err := state.Save(stateFile); err != nil {
		logger.Printf("failed to save state after creating site %q: %v", siteName, err)
		jsonError(w, "site created but failed to save state", http.StatusInternalServerError)
		return
	}
	sendReconcile(reconcileCh)

	view := SiteView{
		Name:    siteName,
		Enabled: false,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(view)
}

func handlePreview(state *State, sitesDir string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract site name: /preview/<sitename>/optional/path
	rest := strings.TrimPrefix(r.URL.Path, "/preview/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}

	// The site name is the first path segment (e.g. "blog.example.com").
	var siteName, filePath string
	if idx := strings.Index(rest, "/"); idx >= 0 {
		siteName = rest[:idx]
		filePath = rest[idx:] // includes leading /
	} else {
		siteName = rest
		filePath = "/"
	}

	decodedSite, err := url.PathUnescape(siteName)
	if err != nil {
		http.Error(w, "invalid site name", http.StatusBadRequest)
		return
	}

	// Only allow previews for sites registered in state.
	known := false
	for _, name := range state.AllSiteNames() {
		if name == decodedSite {
			known = true
			break
		}
	}
	if !known {
		http.NotFound(w, r)
		return
	}

	// Path-traversal guard: resolved path must be strictly under sitesDir.
	safeBase := filepath.Clean(sitesDir)
	siteRoot := filepath.Join(safeBase, decodedSite)
	if siteRoot == safeBase || !strings.HasPrefix(siteRoot, safeBase+string(filepath.Separator)) {
		http.Error(w, "invalid site path", http.StatusBadRequest)
		return
	}

	// Block dotfiles.
	for _, seg := range strings.Split(filePath, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != "" {
			http.NotFound(w, r)
			return
		}
	}

	// try_files: {path}, {path}.html, {path}/index.html (mirrors Caddy config).
	cleanPath := filepath.Clean(filePath)
	candidates := []string{
		filepath.Join(siteRoot, cleanPath),
		filepath.Join(siteRoot, cleanPath+".html"),
		filepath.Join(siteRoot, cleanPath, "index.html"),
	}

	var resolved string
	for _, c := range candidates {
		abs := filepath.Clean(c)
		if !strings.HasPrefix(abs, siteRoot+string(filepath.Separator)) && abs != siteRoot {
			continue // traversal attempt
		}
		info, err := os.Stat(abs)
		if err == nil && !info.IsDir() {
			resolved = abs
			break
		}
	}
	if resolved == "" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

	// Set explicit MIME types for common web assets whose types may be
	// absent or wrong in the Alpine Linux MIME database.
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js", ".mjs":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".jpg", ".jpeg":
		w.Header().Set("Content-Type", "image/jpeg")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".gif":
		w.Header().Set("Content-Type", "image/gif")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".webp":
		w.Header().Set("Content-Type", "image/webp")
	case ".ico":
		w.Header().Set("Content-Type", "image/x-icon")
	case ".woff":
		w.Header().Set("Content-Type", "font/woff")
	case ".woff2":
		w.Header().Set("Content-Type", "font/woff2")
	}

	http.ServeFile(w, r, resolved)
}
