package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type patchRequest struct {
	Enabled        bool   `json:"enabled"`
	ContactEnabled bool   `json:"contact_enabled"`
	ContactTo      string `json:"contact_to"`
	WWWRedirect    bool   `json:"www_redirect"`
}

func handleSites(state *State, cfClient *CloudflareClient, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	apexDomains := map[string]bool{}
	for _, d := range cfClient.AvailableDomains() {
		apexDomains[d] = true
	}
	sites := state.AllSites()
	for i := range sites {
		sites[i].IsApex = apexDomains[sites[i].Name]
		if sites[i].Enabled && sites[i].HasDNS && sites[i].IsApex && sites[i].WWWRedirect {
			sites[i].HasWWW = true
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

func handleSitePatch(state *State, sitesDir string, fileUID, fileGID int, reconcileCh chan<- struct{}, cfReconcileCh chan<- struct{}, w http.ResponseWriter, r *http.Request) {
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

	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
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

	// Validate contact fields when contact form is being enabled.
	if payload.ContactEnabled {
		if !strings.Contains(payload.ContactTo, "@") || !strings.Contains(payload.ContactTo, ".") {
			jsonError(w, "contact_to must be a valid email address", http.StatusBadRequest)
			return
		}
	}

	siteCfg := SiteConfig{
		Enabled:        payload.Enabled,
		ContactEnabled: payload.ContactEnabled,
		ContactTo:      payload.ContactTo,
		WWWRedirect:    payload.WWWRedirect,
	}
	if err := saveSiteConfig(sitesDir, decodedName, siteCfg, fileUID, fileGID); err != nil {
		log.Printf("failed to save site config for %q: %v", decodedName, err)
		jsonError(w, "failed to save site config", http.StatusInternalServerError)
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

func handleDeleteSite(state *State, sitesDir string, reconcileCh, cfReconcileCh chan<- struct{}, logger *log.Logger, w http.ResponseWriter, r *http.Request) {
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

	// Guard: site must exist in state.
	found := false
	for _, sv := range state.AllSites() {
		if sv.Name == decodedName {
			found = true
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

	// Remove the site from the in-memory registry. The directory (including
	// site.json) is deleted below, so no persistent state needs updating.
	state.RemoveSite(decodedName)

	if err := os.RemoveAll(sitePath); err != nil {
		logger.Printf("failed to delete site directory %q: %v", sitePath, err)
		jsonError(w, "failed to delete site directory", http.StatusInternalServerError)
		return
	}

	sendReconcile(reconcileCh)
	sendReconcile(cfReconcileCh)
	w.WriteHeader(http.StatusNoContent)
}

func handleCreateSite(state *State, sitesDir string, cfClient *CloudflareClient, reconcileCh chan<- struct{}, fileUID, fileGID int, logger *log.Logger, w http.ResponseWriter, r *http.Request) {
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
	sendReconcile(reconcileCh)

	view := SiteView{
		Name:    siteName,
		Enabled: false,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(view)
}

func handlePreview(state *State, caddyServiceURL string, w http.ResponseWriter, r *http.Request) {
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

	// Set the preview cookie so that subsequent absolute-path requests
	// (e.g. from JS-generated <link> elements) can be routed correctly.
	http.SetCookie(w, &http.Cookie{
		Name:     previewCookieName,
		Value:    url.PathEscape(decodedSite),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	// Reverse-proxy to Caddy, setting the Host header so Caddy matches the
	// correct virtual host, and stripping the /preview/<site> prefix.
	target, err := url.Parse(caddyServiceURL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	prefix := "/preview/" + url.PathEscape(decodedSite)

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Disable Accept-Encoding so Caddy returns uncompressed responses,
	// allowing us to rewrite HTML content.
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Header.Del("Accept-Encoding")
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		return rewritePreviewResponse(resp, prefix)
	}
	r.URL.Path = filePath
	r.URL.RawPath = ""
	r.Host = decodedSite
	proxy.ServeHTTP(w, r)
}

const previewCookieName = "_preview"

// previewAbsPathRe matches src="/, href="/, action="/ (double or single quotes)
// but NOT src="//", href="//", etc. (protocol-relative URLs).
var previewAbsPathRe = regexp.MustCompile(`((?:src|href|action)\s*=\s*["'])(/[^/"'])`)

// previewSrcsetRe matches a complete srcset attribute value (including surrounding quotes).
var previewSrcsetRe = regexp.MustCompile(`(srcset\s*=\s*["'])([^"']+)(["'])`)

// previewAbsSrcURLRe matches absolute paths (not protocol-relative) within srcset values.
var previewAbsSrcURLRe = regexp.MustCompile(`(/[^/\s,'"<>][^\s,'"<>]*)`)

// previewJSAbsPathRe matches string literals in JavaScript (double, single, or backtick
// quoted) that contain absolute paths under /assets/ — the standard Vite/bundler output
// directory. This covers CSS chunks, JS chunks, images, and fonts injected at runtime.
var previewJSAbsPathRe = regexp.MustCompile("([`\"'])(/assets/[^`\"'<>\\s]+)([`\"'])")

// previewJSRelAssetRe matches relative Vite dep paths of the form "assets/..." in
// JavaScript string literals. Vite's __vite__mapDeps stores deps without a leading
// slash and the preload helper constructs absolute URLs at runtime by prepending "/".
// We insert the preview prefix (minus its own leading slash) so that "/" + dep
// resolves to the correct /preview/<site>/assets/... path.
var previewJSRelAssetRe = regexp.MustCompile(`(["'])(assets/[^"'<>\s]+)(["'])`)

// rewritePreviewResponse rewrites absolute paths in HTML and JavaScript responses to
// include the preview prefix so that assets and navigation work correctly.
func rewritePreviewResponse(resp *http.Response, prefix string) error {
	// Prevent caching of all preview responses so that browsers always fetch
	// fresh content with the correct preview-prefixed paths.
	resp.Header.Set("Cache-Control", "no-store")

	ct := resp.Header.Get("Content-Type")
	isHTML := strings.HasPrefix(ct, "text/html")
	isJS := strings.HasPrefix(ct, "application/javascript") || strings.HasPrefix(ct, "text/javascript")

	if !isHTML && !isJS {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	var rewritten []byte
	if isHTML {
		// Rewrite absolute paths in single-URL HTML attributes (src, href, action).
		rewritten = previewAbsPathRe.ReplaceAll(body, []byte("${1}"+prefix+"${2}"))

		// Rewrite absolute paths within srcset attribute values. The browser
		// prefers srcset over src for responsive images, so every URL entry inside
		// the srcset must also be prefixed.
		rewritten = previewSrcsetRe.ReplaceAllFunc(rewritten, func(match []byte) []byte {
			parts := previewSrcsetRe.FindSubmatch(match)
			if parts == nil {
				return match
			}
			inner := previewAbsSrcURLRe.ReplaceAll(parts[2], []byte(prefix+"$1"))
			out := make([]byte, 0, len(parts[1])+len(inner)+len(parts[3]))
			out = append(out, parts[1]...)
			out = append(out, inner...)
			out = append(out, parts[3]...)
			return out
		})
	} else {
		// For JavaScript, rewrite /assets/ string literals. Vite and similar
		// bundlers embed asset paths as plain string literals; rewriting them
		// here means the JS never makes a request to the wrong origin.
		rewritten = previewJSAbsPathRe.ReplaceAll(body, []byte("${1}"+prefix+"${2}${3}"))

		// Rewrite relative Vite dep paths ("assets/...") stored in __vite__mapDeps.
		// Vite's preload helper constructs absolute URLs at runtime via "/" + dep.
		// By inserting the preview prefix without its leading "/", the runtime
		// concatenation produces "/preview/<site>/assets/..." instead of "/assets/...".
		trimmedPrefix := strings.TrimPrefix(prefix, "/")
		rewritten = previewJSRelAssetRe.ReplaceAll(rewritten, []byte("${1}"+trimmedPrefix+"/${2}${3}"))
	}

	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	return nil
}

// handlePreviewFallback proxies requests that arrive with absolute paths
// (e.g. /assets/...) while a preview session is active. This handles resources
// loaded by JavaScript at runtime that cannot be rewritten in HTML.
func handlePreviewFallback(state *State, caddyServiceURL string, w http.ResponseWriter, r *http.Request) {
	decodedSite := previewSiteFromRequest(r)
	if decodedSite == "" {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}

	// Validate against known sites to prevent arbitrary proxying.
	known := false
	for _, name := range state.AllSiteNames() {
		if name == decodedSite {
			known = true
			break
		}
	}
	if !known {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}

	target, err := url.Parse(caddyServiceURL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set("Cache-Control", "no-store")
		return nil
	}
	r.Host = decodedSite
	proxy.ServeHTTP(w, r)
}

// previewSiteFromRequest returns the previewed site name by checking the
// preview cookie first, then falling back to the Referer header. Using the
// Referer means in-page asset requests (JS, CSS, images with hardcoded
// absolute paths) still proxy correctly even when the cookie has been cleared
// (e.g. by opening the dashboard in another tab).
func previewSiteFromRequest(r *http.Request) string {
	// Primary: cookie set by handlePreview.
	if cookie, err := r.Cookie(previewCookieName); err == nil && cookie.Value != "" {
		if decoded, err := url.PathUnescape(cookie.Value); err == nil && decoded != "" {
			return decoded
		}
	}

	// Secondary: Referer header — browsers always include it for sub-resource
	// requests (script, link, img) originating from a same-origin page.
	ref := r.Referer()
	if ref == "" {
		return ""
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	rest := strings.TrimPrefix(refURL.Path, "/preview/")
	if rest == refURL.Path {
		return "" // Referer is not a /preview/ path.
	}
	var siteName string
	if idx := strings.Index(rest, "/"); idx >= 0 {
		siteName = rest[:idx]
	} else {
		siteName = rest
	}
	decoded, err := url.PathUnescape(siteName)
	if err != nil || decoded == "" {
		return ""
	}
	return decoded
}

func handleOptimizeImages(state *State, sitesDir string, fileUID, fileGID int, logger *log.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract site name from: /api/sites/<name>/optimize-images
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/sites/")
	name := strings.TrimSuffix(trimmed, "/optimize-images")
	if name == "" || name == trimmed {
		http.Error(w, "site name required", http.StatusBadRequest)
		return
	}
	decodedName, err := url.PathUnescape(name)
	if err != nil {
		http.Error(w, "invalid site name", http.StatusBadRequest)
		return
	}

	// Validate site exists in state.
	known := false
	for _, s := range state.AllSiteNames() {
		if s == decodedName {
			known = true
			break
		}
	}
	if !known {
		jsonError(w, "site not found", http.StatusNotFound)
		return
	}

	// Path traversal guard.
	safeBase := filepath.Clean(sitesDir)
	sitePath := filepath.Join(safeBase, decodedName)
	if sitePath == safeBase || !strings.HasPrefix(sitePath, safeBase+string(filepath.Separator)) {
		logger.Printf("optimize images: computed path %q is outside sitesDir %q", sitePath, safeBase)
		http.Error(w, "invalid site path", http.StatusBadRequest)
		return
	}

	result, err := optimizeSiteImages(sitesDir, decodedName, fileUID, fileGID)
	if err != nil {
		logger.Printf("optimize images for %q failed: %v", decodedName, err)
		jsonError(w, fmt.Sprintf("image optimization failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
