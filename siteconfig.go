package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// SiteConfig holds all per-site configuration persisted as {sitesDir}/{siteName}/site.json.
// This is the single source of truth for both operational state (enabled, has_dns)
// and user-managed settings (contact form, alias serving).
type SiteConfig struct {
	Enabled        bool   `json:"enabled"`
	HasDNS         bool   `json:"has_dns,omitempty"`
	ContactEnabled bool   `json:"contact_enabled"`
	ContactTo      string `json:"contact_to,omitempty"`
	// ServeAtWWW makes the site also serve at www.{apex} (no redirect — same content).
	// Applies to apex domains and subdomains alike; www always refers to the apex's www.
	ServeAtWWW bool `json:"serve_at_www"`
	// ServeAtApex makes a non-apex site also serve at its bare apex domain.
	ServeAtApex bool `json:"serve_at_apex"`
	// FramerURL is the original Framer site URL used to download this site.
	// Non-empty only for sites created with the framer-download template.
	FramerURL string `json:"framer_url,omitempty"`
}

// migrateSiteConfigs rewrites any site.json that still uses the legacy
// "www_redirect" key, replacing it with "serve_at_www". Safe to call on
// every startup; it is a no-op when no migration is needed.
func migrateSiteConfigs(sitesDir string) {
	entries, err := os.ReadDir(sitesDir)
	if err != nil {
		log.Printf("migrate: cannot read sites dir: %v", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(sitesDir, e.Name(), "site.json")
		raw, err := os.ReadFile(p)
		if err != nil {
			continue // file may not exist yet
		}
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		v, hasPrev := m["www_redirect"]
		if !hasPrev {
			continue // already migrated or never had the key
		}
		delete(m, "www_redirect")
		if b, ok := v.(bool); ok && b {
			m["serve_at_www"] = true
		}
		out, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			log.Printf("migrate: marshal error for %s: %v", e.Name(), err)
			continue
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, out, 0o644); err != nil {
			log.Printf("migrate: write error for %s: %v", e.Name(), err)
			continue
		}
		if err := os.Rename(tmp, p); err != nil {
			log.Printf("migrate: rename error for %s: %v", e.Name(), err)
			continue
		}
		log.Printf("migrate: %s migrated www_redirect → serve_at_www", e.Name())
	}
}

// loadSiteConfig reads the site.json for the given site. Returns a zero-value
// SiteConfig (all fields false/empty) if the file does not exist.
func loadSiteConfig(sitesDir, siteName string) (SiteConfig, error) {
	p := filepath.Join(sitesDir, siteName, "site.json")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return SiteConfig{}, nil
		}
		return SiteConfig{}, err
	}
	var cfg SiteConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return SiteConfig{}, err
	}
	return cfg, nil
}

// saveSiteConfig atomically writes site.json for the given site, applying the
// provided PUID/PGID ownership (Unraid compatibility).
func saveSiteConfig(sitesDir, siteName string, cfg SiteConfig, fileUID, fileGID int) error {
	dir := filepath.Join(sitesDir, siteName)
	p := filepath.Join(dir, "site.json")
	tmp := p + ".tmp"

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if fileUID != 0 || fileGID != 0 {
		_ = os.Chown(p, fileUID, fileGID)
	}
	return nil
}
