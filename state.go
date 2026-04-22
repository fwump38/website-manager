package main

import (
	"sort"
	"sync"
)

// DownloadStatus tracks the progress of an async Framer site download.
type DownloadStatus struct {
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
}

// State is a thread-safe in-memory registry of site names discovered from the
// sites directory. All persistent data (enabled flag, DNS state, contact
// settings, etc.) lives in each site's own site.json via siteconfig.go.
type State struct {
	mu               sync.RWMutex
	sites            map[string]struct{}
	sitesDir         string
	FileUID          int
	FileGID          int
	dsMu             sync.RWMutex
	downloadStatuses map[string]DownloadStatus
}

// SiteView is the API representation of a site returned by /api/sites.
type SiteView struct {
	Name           string `json:"name"`
	Enabled        bool   `json:"enabled"`
	HasDNS         bool   `json:"has_dns,omitempty"`
	HasWWW         bool   `json:"has_www,omitempty"`
	HasApexAlias   bool   `json:"has_apex_alias,omitempty"`
	IsApex         bool   `json:"is_apex"`
	ContactEnabled bool   `json:"contact_enabled"`
	ContactTo      string `json:"contact_to,omitempty"`
	ServeAtWWW     bool   `json:"serve_at_www"`
	ServeAtApex    bool   `json:"serve_at_apex"`
	Downloading    bool   `json:"downloading,omitempty"`
	DownloadError  string `json:"download_error,omitempty"`
	FramerURL      string `json:"framer_url,omitempty"`
}

// NewState returns an empty State rooted at sitesDir.
func NewState(sitesDir string) *State {
	return &State{
		sites:            map[string]struct{}{},
		sitesDir:         sitesDir,
		downloadStatuses: map[string]DownloadStatus{},
	}
}

// SetDownloadStatus stores the download status for a site.
func (s *State) SetDownloadStatus(site string, ds DownloadStatus) {
	s.dsMu.Lock()
	defer s.dsMu.Unlock()
	s.downloadStatuses[site] = ds
}

// GetDownloadStatus returns the download status for a site.
func (s *State) GetDownloadStatus(site string) DownloadStatus {
	s.dsMu.RLock()
	defer s.dsMu.RUnlock()
	return s.downloadStatuses[site]
}

func (s *State) AddSite(site string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sites[site] = struct{}{}
}

func (s *State) RemoveSite(site string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sites, site)
}

// SetEnabled persists the enabled flag for site by doing a read-modify-write
// on its site.json. Other fields already stored in site.json are preserved.
func (s *State) SetEnabled(site string, enabled bool) error {
	cfg, err := loadSiteConfig(s.sitesDir, site)
	if err != nil {
		return err
	}
	cfg.Enabled = enabled
	return saveSiteConfig(s.sitesDir, site, cfg, s.FileUID, s.FileGID)
}

// SetHasDNS persists the has_dns flag for site by doing a read-modify-write
// on its site.json.
func (s *State) SetHasDNS(site string, v bool) error {
	cfg, err := loadSiteConfig(s.sitesDir, site)
	if err != nil {
		return err
	}
	cfg.HasDNS = v
	return saveSiteConfig(s.sitesDir, site, cfg, s.FileUID, s.FileGID)
}

// DNSManagedSites returns the sorted list of sites whose site.json reports
// has_dns = true.
func (s *State) DNSManagedSites() []string {
	names := s.AllSiteNames()
	var out []string
	for _, name := range names {
		cfg, _ := loadSiteConfig(s.sitesDir, name)
		if cfg.HasDNS {
			out = append(out, name)
		}
	}
	return out // already sorted by AllSiteNames
}

// EnabledSites returns the sorted list of sites whose site.json reports
// enabled = true.
func (s *State) EnabledSites() []string {
	names := s.AllSiteNames()
	var out []string
	for _, name := range names {
		cfg, _ := loadSiteConfig(s.sitesDir, name)
		if cfg.Enabled {
			out = append(out, name)
		}
	}
	return out // already sorted by AllSiteNames
}

func (s *State) AllSiteNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.sites))
	for name := range s.sites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *State) AllSites() []SiteView {
	names := s.AllSiteNames()
	out := make([]SiteView, 0, len(names))
	for _, name := range names {
		cfg, _ := loadSiteConfig(s.sitesDir, name)
		ds := s.GetDownloadStatus(name)
		out = append(out, SiteView{
			Name:           name,
			Enabled:        cfg.Enabled,
			HasDNS:         cfg.HasDNS,
			ContactEnabled: cfg.ContactEnabled,
			ContactTo:      cfg.ContactTo,
			ServeAtWWW:     cfg.ServeAtWWW,
			ServeAtApex:    cfg.ServeAtApex,
			Downloading:    ds.Running,
			DownloadError:  ds.Error,
			FramerURL:      cfg.FramerURL,
		})
	}
	return out
}
