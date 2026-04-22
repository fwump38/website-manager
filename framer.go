package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// framerCDNHosts lists the CDN hosts whose assets will be downloaded locally.
var framerCDNHosts = []string{
	"framerusercontent.com",
	"framer.com",
	"framercanvas.com",
	"framerstatic.com",
}

// framerCDNExcludeHosts lists subdomains that should NOT be downloaded even
// though they are under a framerCDNHosts domain. These are analytics/tracking
// endpoints whose content is irrelevant to a static site mirror.
var framerCDNExcludeHosts = []string{
	"events.framer.com",
}

// FramerDownloader crawls a Framer-hosted website, downloads all pages and
// Framer CDN assets, rewrites URLs to be self-hosted, and writes the result
// into SiteDir. It is designed to run in a background goroutine.
type FramerDownloader struct {
	SiteDir  string
	BaseURL  *url.URL
	SiteName string
	State    *State
	FileUID  int
	FileGID  int
	Logger   *log.Logger

	client *http.Client
	// assetCache maps original asset URL → local public path, guarding against
	// duplicate downloads during a single run.
	assetCache map[string]string
	// localNameCounter tracks how many CDN assets have been assigned each flat
	// local filename (dir/basename) for collision-safe dedup within the
	// js/, css/, images/, media/ directories.
	localNameCounter map[string]int
}

// Download performs the full crawl-and-rewrite pipeline. It calls
// state.SetDownloadStatus when done.
func (d *FramerDownloader) Download() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	d.client = &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	d.assetCache = map[string]string{}
	d.localNameCounter = map[string]int{}

	if err := d.crawl(ctx); err != nil {
		d.Logger.Printf("framer download %q: %v", d.SiteName, err)
		d.State.SetDownloadStatus(d.SiteName, DownloadStatus{Running: false, Error: err.Error()})
		return
	}
	// Recursively download JS modules referenced via relative imports inside
	// downloaded JS files — these are not discoverable from HTML alone.
	d.downloadMissingJSModules(ctx)
	d.Logger.Printf("framer download %q: complete", d.SiteName)
	d.State.SetDownloadStatus(d.SiteName, DownloadStatus{Running: false})
}

// crawl performs a BFS of all pages reachable from BaseURL within the same origin.
func (d *FramerDownloader) crawl(ctx context.Context) error {
	origin := d.BaseURL.Scheme + "://" + d.BaseURL.Host

	visited := map[string]bool{}
	queue := []string{d.normalisePageURL(d.BaseURL.String())}

	// Seed the BFS queue from sitemap.xml so deep nested routes
	// (/news/slug, /program/slug, etc.) are discovered without depending
	// solely on link traversal.
	for _, u := range d.fetchSitemapURLs(ctx) {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Host != d.BaseURL.Host {
			continue
		}
		norm := d.normalisePageURL(u)
		if !visited[norm] {
			queue = append(queue, norm)
		}
	}

	for len(queue) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pageURL := queue[0]
		queue = queue[1:]

		if visited[pageURL] {
			continue
		}
		visited[pageURL] = true

		d.Logger.Printf("framer download %q: fetching page %s", d.SiteName, pageURL)
		links, err := d.downloadPage(ctx, pageURL)
		if err != nil {
			d.Logger.Printf("framer download %q: page %s: %v", d.SiteName, pageURL, err)
			continue
		}

		for _, link := range links {
			abs := d.resolveURL(pageURL, link)
			if abs == "" {
				continue
			}
			parsed, err := url.Parse(abs)
			if err != nil {
				continue
			}
			// Only enqueue same-origin pages.
			if parsed.Scheme+"://"+parsed.Host != origin {
				continue
			}
			norm := d.normalisePageURL(abs)
			if !visited[norm] {
				queue = append(queue, norm)
			}
		}
	}
	return nil
}

// downloadPage fetches one HTML page, rewrites it, writes it to disk, and
// returns the list of in-page <a href> links.
func (d *FramerDownloader) downloadPage(ctx context.Context, pageURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; site-manager/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		return nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
	if err != nil {
		return nil, err
	}

	parsed, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}

	rewritten, links, err := d.rewriteHTML(ctx, body, parsed)
	if err != nil {
		return nil, err
	}

	destPath := d.pageURLToFilePath(parsed)
	if err := d.writeFile(destPath, rewritten); err != nil {
		return nil, err
	}
	return links, nil
}

// Regex patterns applied to the serialised HTML string after tree rendering,
// targeting Framer badge/editorbar CSS rules and injected HTML comments.
var (
	framerBadgeCSSRe    = regexp.MustCompile(`\s*#__framer-badge-container\s*\{[\s\S]*?\}\s*`)
	framerEditorCSSRe   = regexp.MustCompile(`\s*#__framer-editorbar[a-zA-Z-]*\s*\{[\s\S]*?\}\s*`)
	framerHTMLCommentRe = regexp.MustCompile(`<!--\s*(?:Made in Framer|Published)[^>]*-->`)
)

// postCleanHTML removes Framer badge/editorbar inline CSS rules and injected
// HTML comments from a serialised HTML string.
func postCleanHTML(h string) string {
	h = framerBadgeCSSRe.ReplaceAllLiteralString(h, "\n")
	h = framerEditorCSSRe.ReplaceAllLiteralString(h, "\n")
	h = framerHTMLCommentRe.ReplaceAllLiteralString(h, "")
	return strings.ReplaceAll(h, "notranslate editorbar ", "notranslate ")
}

// sitemapLocRe matches <loc>...</loc> entries in sitemap XML.
var sitemapLocRe = regexp.MustCompile(`<loc>\s*(https?://[^<\s]+)\s*</loc>`)

// jsRelImportRe matches relative module paths in JS import statements,
// capturing the filename. Handles static (from "./path.mjs") and dynamic
// (import("./path.mjs"), import('./...'), import(`./...`)) forms.
var jsRelImportRe = regexp.MustCompile(`(?:from|import\s*\()\s*["'` + "`" + `]\./([^"'` + "`" + `\s>]+\.m?js)["'` + "`" + `]`)

// fetchSitemapURLs fetches /sitemap.xml and returns all <loc> URLs.
// Returns nil on any error (sitemap is treated as optional).
func (d *FramerDownloader) fetchSitemapURLs(ctx context.Context) []string {
	sitemapURL := d.BaseURL.Scheme + "://" + d.BaseURL.Host + "/sitemap.xml"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sitemapURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; site-manager/1.0)")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return nil
	}

	var urls []string
	for _, m := range sitemapLocRe.FindAllSubmatch(body, -1) {
		if len(m) >= 2 {
			urls = append(urls, string(m[1]))
		}
	}
	d.Logger.Printf("framer download %q: sitemap: found %d URLs", d.SiteName, len(urls))
	return urls
}

// downloadMissingJSModules scans downloaded JS/MJS files for relative import
// statements and downloads any referenced modules not yet in the asset cache.
// The process repeats until no new modules are found (fixed-point iteration),
// capturing transitively-referenced modules that are invisible from HTML alone.
func (d *FramerDownloader) downloadMissingJSModules(ctx context.Context) {
	type jsEntry struct {
		cdnURL      string
		localPublic string
	}
	for {
		var jsAssets []jsEntry
		for cdnURL, localPublic := range d.assetCache {
			lo := strings.ToLower(cdnURL)
			if strings.HasSuffix(lo, ".js") || strings.HasSuffix(lo, ".mjs") {
				jsAssets = append(jsAssets, jsEntry{cdnURL: cdnURL, localPublic: localPublic})
			}
		}

		added := 0
		for _, js := range jsAssets {
			localFS := filepath.Join(d.SiteDir, filepath.FromSlash(strings.TrimPrefix(js.localPublic, "/")))
			data, err := os.ReadFile(localFS)
			if err != nil {
				continue
			}

			for _, m := range jsRelImportRe.FindAllSubmatch(data, -1) {
				if len(m) < 2 {
					continue
				}
				abs := d.resolveURL(js.cdnURL, "./"+string(m[1]))
				if abs == "" {
					continue
				}
				if _, cached := d.assetCache[abs]; !cached {
					if local := d.downloadAssetCtx(ctx, abs, d.BaseURL); local != "" {
						d.Logger.Printf("framer download %q: dynamic module %s", d.SiteName, string(m[1]))
						added++
					}
				}
			}
		}

		if added == 0 {
			break
		}
	}
}

// cleanFramerNoise removes Framer editor/branding artifacts from the HTML tree:
//   - data-framer-hydrate-v2 attribute (baked-in routeId causes wrong hydration
//     on sub-pages; stripping forces URL-based SPA routing instead)
//   - "Made in Framer" badge, editor bar, canvas sandbox, drag overlay elements
//   - Editor bar <link>/<script> tags and modulepreload hints
//   - Analytics and editor-only inline scripts
//   - <meta name="framer-search-index">
func (d *FramerDownloader) cleanFramerNoise(doc *html.Node) {
	removeByID := map[string]bool{
		"__framer-badge-container":     true,
		"__framer-editorbar-container": true,
		"__framer-editorbar":           true,
		"canvas_sandbox":               true,
		"drag-overlay":                 true,
	}

	var toRemove []*html.Node

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			// Remove whole element if its id matches.
			if id := attrVal(n, "id"); id != "" && removeByID[id] {
				toRemove = append(toRemove, n)
				return
			}

			// Strip data-framer-hydrate-v2 attribute from every element.
			for i := len(n.Attr) - 1; i >= 0; i-- {
				if n.Attr[i].Key == "data-framer-hydrate-v2" {
					n.Attr = append(n.Attr[:i], n.Attr[i+1:]...)
					break
				}
			}

			switch n.Data {
			case "script":
				src := attrVal(n, "src")
				typ := attrVal(n, "type")
				if strings.Contains(src, "editorbar") {
					toRemove = append(toRemove, n)
					return
				}
				if (typ == "" || typ == "text/javascript") && src == "" {
					if c := n.FirstChild; c != nil && c.Type == html.TextNode {
						txt := c.Data
						if strings.Contains(txt, "__framer_force_showing_editorbar") ||
							strings.Contains(txt, "events.framer.com") ||
							strings.Contains(txt, "cssBundleURL") ||
							strings.Contains(txt, "deferredJsFiles") {
							toRemove = append(toRemove, n)
							return
						}
					}
				}
			case "link":
				if strings.Contains(attrVal(n, "href"), "editorbar") {
					toRemove = append(toRemove, n)
					return
				}
			case "meta":
				if attrVal(n, "name") == "framer-search-index" {
					toRemove = append(toRemove, n)
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	for _, n := range toRemove {
		if n.Parent != nil {
			n.Parent.RemoveChild(n)
		}
	}
}

// rewriteHTML parses HTML, strips analytics/editor scripts, rewrites asset
// references (downloading Framer CDN assets locally), collects internal page
// links, and serialises the mutated tree. The Framer React runtime is
// preserved so that the page hydrates with full animations and interactions.
func (d *FramerDownloader) rewriteHTML(ctx context.Context, raw []byte, pageURL *url.URL) ([]byte, []string, error) {
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}

	// Remove Framer editor/branding noise; keep the React hydration bundle.
	d.cleanFramerNoise(doc)

	var links []string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "a":
				for i, a := range n.Attr {
					if a.Key == "href" {
						abs := d.resolveURL(pageURL.String(), a.Val)
						if abs != "" && d.isSameOrigin(abs) {
							links = append(links, a.Val)
							// Rewrite to root-relative.
							u, err := url.Parse(abs)
							if err == nil {
								n.Attr[i].Val = u.RequestURI()
							}
						}
					}
				}
			case "script":
				for i, a := range n.Attr {
					if a.Key == "src" && a.Val != "" {
						if local := d.downloadAssetCtx(ctx, a.Val, pageURL); local != "" {
							n.Attr[i].Val = local
						}
					}
				}
				// Rewrite inline JS.
				if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
					n.FirstChild.Data = d.rewriteJS(ctx, n.FirstChild.Data)
				}
			case "style":
				// Rewrite url() references inside inline <style> blocks (e.g. @font-face src).
				if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
					rewritten := d.rewriteCSS(ctx, []byte(n.FirstChild.Data), pageURL)
					n.FirstChild.Data = string(rewritten)
				}
			case "link":
				rel := attrVal(n, "rel")
				for i, a := range n.Attr {
					if a.Key == "href" && a.Val != "" {
						switch {
						case rel == "canonical":
							n.Attr[i].Val = d.rewriteOrigin(a.Val)
						case strings.Contains(rel, "modulepreload"):
							if local := d.downloadAssetCtx(ctx, a.Val, pageURL); local != "" {
								n.Attr[i].Val = local
							} else if (strings.HasPrefix(a.Val, "./") || strings.HasPrefix(a.Val, "../")) &&
								(strings.HasSuffix(a.Val, ".mjs") || strings.HasSuffix(a.Val, ".js")) {
								// Relative JS module hint that isn't on a CDN — all JS
								// lives in /js/ so rewrite to the flat path.
								n.Attr[i].Val = "/js/" + path.Base(a.Val)
							}
						case strings.Contains(rel, "stylesheet") ||
							strings.Contains(rel, "preload") ||
							rel == "icon" || rel == "shortcut icon" || rel == "apple-touch-icon":
							if local := d.downloadAssetCtx(ctx, a.Val, pageURL); local != "" {
								n.Attr[i].Val = local
							}
						}
					}
				}
			case "img":
				for i, a := range n.Attr {
					switch a.Key {
					case "src":
						if local := d.downloadAssetCtx(ctx, a.Val, pageURL); local != "" {
							n.Attr[i].Val = local
						}
					case "srcset":
						n.Attr[i].Val = d.rewriteSrcset(ctx, a.Val, pageURL)
					}
				}
			case "source":
				for i, a := range n.Attr {
					switch a.Key {
					case "src":
						if local := d.downloadAssetCtx(ctx, a.Val, pageURL); local != "" {
							n.Attr[i].Val = local
						}
					case "srcset":
						n.Attr[i].Val = d.rewriteSrcset(ctx, a.Val, pageURL)
					}
				}
			case "meta":
				prop := attrVal(n, "property")
				metaName := attrVal(n, "name")
				for i, a := range n.Attr {
					if a.Key != "content" || a.Val == "" {
						continue
					}
					switch {
					case prop == "og:image" || metaName == "twitter:image":
						if local := d.downloadAssetCtx(ctx, a.Val, pageURL); local != "" {
							n.Attr[i].Val = local
						}
					case prop == "og:url" || prop == "og:site_name" || metaName == "twitter:url":
						n.Attr[i].Val = d.rewriteOrigin(a.Val)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return nil, nil, err
	}
	// Post-render regex cleanup for inline CSS rules and HTML comments that
	// are awkward to remove via tree manipulation.
	return []byte(postCleanHTML(buf.String())), links, nil
}

// cssURLRe matches url(...) references in CSS.
// Note: RE2 (used by Go) does not support backreferences, so we accept any
// optional closing quote rather than requiring it to match the opening one.
var cssURLRe = regexp.MustCompile(`url\(\s*(['"]?)([^'"\)\s]+)['"]?\s*\)`)

// rewriteCSS rewrites Framer CDN url() references in CSS content to local paths.
func (d *FramerDownloader) rewriteCSS(ctx context.Context, content []byte, cssURL *url.URL) []byte {
	return cssURLRe.ReplaceAllFunc(content, func(match []byte) []byte {
		sub := cssURLRe.FindSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		raw := string(sub[2])
		if local := d.downloadAssetCtx(ctx, raw, cssURL); local != "" {
			return []byte(fmt.Sprintf("url(%s%s%s)", string(sub[1]), local, string(sub[1])))
		}
		return match
	})
}

// jsFramerURLRe matches string literals that contain a Framer CDN hostname.
var jsFramerURLRe = regexp.MustCompile(`(["` + "`" + `'])(https?://(?:[a-zA-Z0-9-]+\.)*(?:framerusercontent\.com|framer\.com|framercanvas\.com|framerstatic\.com)/[^"` + "`" + `'<>\s]*)(["` + "`" + `'])`)

// rewriteJS rewrites Framer CDN URLs embedded in JavaScript string literals.
func (d *FramerDownloader) rewriteJS(ctx context.Context, src string) string {
	return jsFramerURLRe.ReplaceAllStringFunc(src, func(match string) string {
		sub := jsFramerURLRe.FindStringSubmatch(match)
		if len(sub) < 4 {
			return match
		}
		rawURL := sub[2]
		if local := d.downloadAssetCtx(ctx, rawURL, d.BaseURL); local != "" {
			return sub[1] + local + sub[3]
		}
		return match
	})
}

// rewriteSrcset rewrites all URLs within a srcset attribute value.
func (d *FramerDownloader) rewriteSrcset(ctx context.Context, srcset string, base *url.URL) string {
	parts := strings.Split(srcset, ",")
	for i, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 {
			continue
		}
		if local := d.downloadAssetCtx(ctx, fields[0], base); local != "" {
			fields[0] = local
		}
		parts[i] = strings.Join(fields, " ")
	}
	return strings.Join(parts, ", ")
}

// downloadAssetCtx resolves rawURL relative to base, downloads it if it is a
// Framer CDN asset, and returns the local public path. Returns "" if the URL
// is not a Framer CDN asset or is empty/invalid.
func (d *FramerDownloader) downloadAssetCtx(ctx context.Context, rawURL string, base *url.URL) string {
	if rawURL == "" || strings.HasPrefix(rawURL, "data:") || strings.HasPrefix(rawURL, "#") {
		return ""
	}
	abs := d.resolveURL(base.String(), rawURL)
	if abs == "" {
		return ""
	}
	parsed, err := url.Parse(abs)
	if err != nil || !d.isFramerCDN(parsed.Host) {
		return ""
	}

	if cached, ok := d.assetCache[abs]; ok {
		return cached
	}

	localPath, localPublic, err := d.assetURLToPath(parsed)
	if err != nil {
		d.Logger.Printf("framer download %q: asset path error %s: %v", d.SiteName, abs, err)
		return ""
	}

	if _, statErr := os.Stat(localPath); os.IsNotExist(statErr) {
		if err := d.fetchAndWrite(ctx, abs, localPath, parsed); err != nil {
			d.Logger.Printf("framer download %q: asset fetch error %s: %v", d.SiteName, abs, err)
			return ""
		}
	}
	d.assetCache[abs] = localPublic
	return localPublic
}

// fetchAndWrite downloads url and saves it to destPath. For CSS files it
// rewrites internal url() references; for JS files it rewrites Framer CDN
// URLs embedded in string literals so that the React bundle and its chunks
// reference locally downloaded assets rather than the Framer CDN.
func (d *FramerDownloader) fetchAndWrite(ctx context.Context, rawURL, destPath string, parsed *url.URL) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; site-manager/1.0)")

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, rawURL)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024))
	if err != nil {
		return err
	}

	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "text/css") || strings.HasSuffix(parsed.Path, ".css"):
		// Rewrite CSS url() references to local paths.
		body = d.rewriteCSS(ctx, body, parsed)
	case strings.Contains(ct, "javascript") ||
		strings.HasSuffix(parsed.Path, ".js") ||
		strings.HasSuffix(parsed.Path, ".mjs"):
		// Rewrite Framer CDN URLs embedded as string literals in the React
		// bundle and its chunks. This ensures dynamic imports and asset
		// references inside the JS resolve to locally served files, enabling
		// the full hydrated experience without any outbound CDN requests.
		body = []byte(d.rewriteJS(ctx, string(body)))
	}

	return d.writeFile(destPath, body)
}

// writeFile writes data to destPath, creating parent directories as needed,
// and applies the configured uid/gid ownership.
func (d *FramerDownloader) writeFile(destPath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return err
	}
	if d.FileUID != 0 || d.FileGID != 0 {
		_ = os.Chown(destPath, d.FileUID, d.FileGID)
	}
	return nil
}

// pageURLToFilePath maps a page URL path to a file path under SiteDir.
// "/" → index.html, "/about" → about/index.html, "/about.html" → about.html
func (d *FramerDownloader) pageURLToFilePath(pageURL *url.URL) string {
	p := path.Clean(pageURL.Path)
	if p == "/" || p == "" || p == "." {
		return filepath.Join(d.SiteDir, "index.html")
	}
	// If the last segment already has a file extension, use it directly.
	base := path.Base(p)
	if strings.Contains(base, ".") {
		return filepath.Join(d.SiteDir, filepath.FromSlash(strings.TrimPrefix(p, "/")))
	}
	// Otherwise treat as a directory page: /about → about/index.html
	return filepath.Join(d.SiteDir, filepath.FromSlash(strings.TrimPrefix(p, "/")), "index.html")
}

// assetSubdir returns the flat local subdirectory for a CDN asset based on
// file extension and CDN path patterns: js, css, images, media, or assets.
func (d *FramerDownloader) assetSubdir(assetURL *url.URL) string {
	p := assetURL.Path
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".mjs":
		return "js"
	case ".css":
		return "css"
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".ico", ".avif", ".bmp", ".tiff":
		return "images"
	case ".mp4", ".webm", ".ogg", ".mov", ".avi", ".mkv",
		".mp3", ".wav", ".flac", ".aac", ".opus",
		".pdf", ".zip", ".tar", ".gz",
		".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx":
		return "media"
	default:
		if strings.Contains(p, "/images/") {
			return "images"
		}
		if strings.Contains(p, "/files/") {
			return "media"
		}
		return "assets"
	}
}

// uniqueLocalName returns a collision-safe flat filename within subdir.
// The first CDN URL to claim a given basename keeps it as-is; subsequent
// URLs sharing the same basename receive a numeric suffix (_2, _3, …).
func (d *FramerDownloader) uniqueLocalName(subdir, basename string) string {
	key := subdir + "/" + basename
	if _, taken := d.localNameCounter[key]; !taken {
		d.localNameCounter[key] = 1
		return basename
	}
	d.localNameCounter[key]++
	n := d.localNameCounter[key]
	ext := path.Ext(basename)
	stem := strings.TrimSuffix(basename, ext)
	return fmt.Sprintf("%s_%d%s", stem, n, ext)
}

// assetURLToPath maps a CDN asset URL to (localFilesystemPath, localPublicPath).
// Assets are stored in flat category directories: js/, css/, images/, media/.
func (d *FramerDownloader) assetURLToPath(assetURL *url.URL) (string, string, error) {
	cleanPath := assetURL.Path
	if cleanPath == "" || cleanPath == "/" {
		return "", "", fmt.Errorf("asset URL has empty path: %s", assetURL)
	}
	basename := path.Base(cleanPath)
	if basename == "" || basename == "." {
		return "", "", fmt.Errorf("asset URL has no filename: %s", assetURL)
	}
	// Strip any query string that may have leaked into the path segment.
	if i := strings.IndexByte(basename, '?'); i >= 0 {
		basename = basename[:i]
	}
	subdir := d.assetSubdir(assetURL)
	localName := d.uniqueLocalName(subdir, basename)
	rel := subdir + "/" + localName
	localPath := filepath.Join(d.SiteDir, filepath.FromSlash(rel))
	localPublic := "/" + rel
	return localPath, localPublic, nil
}

// resolveURL resolves rawURL relative to baseStr. Returns "" on error or if
// the result is not an http/https URL.
func (d *FramerDownloader) resolveURL(baseStr, rawURL string) string {
	if rawURL == "" {
		return ""
	}
	if strings.HasPrefix(rawURL, "//") {
		parsed, _ := url.Parse(d.BaseURL.Scheme + ":" + rawURL)
		if parsed != nil {
			return parsed.String()
		}
		return ""
	}
	base, err := url.Parse(baseStr)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return ""
	}
	// Strip fragment — fragments don't affect the page fetched.
	resolved.Fragment = ""
	return resolved.String()
}

// normalisePageURL strips fragment and normalises trailing slashes for use as
// a visited-map key.
func (d *FramerDownloader) normalisePageURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.Fragment = ""
	// Normalise /path/ and /path to the same key.
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

// isSameOrigin reports whether rawURL belongs to the same origin as BaseURL.
func (d *FramerDownloader) isSameOrigin(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Host == d.BaseURL.Host
}

// rewriteOrigin rewrites the host of rawURL from the source Framer origin to
// the target site name (e.g. for canonical / og:url). If rawURL cannot be
// parsed or does not share the source origin, it is returned unchanged.
func (d *FramerDownloader) rewriteOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host != d.BaseURL.Host {
		return rawURL
	}
	u.Scheme = "https"
	u.Host = d.SiteName
	return u.String()
}

// isFramerCDN reports whether host is a Framer CDN host that should be localised.
func (d *FramerDownloader) isFramerCDN(host string) bool {
	for _, exc := range framerCDNExcludeHosts {
		if host == exc {
			return false
		}
	}
	for _, cdn := range framerCDNHosts {
		if host == cdn || strings.HasSuffix(host, "."+cdn) {
			return true
		}
	}
	return false
}

// attrVal returns the value of the named attribute on n, or "".
func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
