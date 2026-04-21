package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// SiteConfig holds all per-site configuration persisted as {sitesDir}/{siteName}/site.json.
// This is the single source of truth for both operational state (enabled, has_dns)
// and user-managed settings (contact form, www redirect).
type SiteConfig struct {
	Enabled        bool   `json:"enabled"`
	HasDNS         bool   `json:"has_dns,omitempty"`
	ContactEnabled bool   `json:"contact_enabled"`
	ContactTo      string `json:"contact_to,omitempty"`
	WWWRedirect    bool   `json:"www_redirect"`
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
