package main

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
)

type SiteInfo struct {
	Enabled bool `json:"enabled"`
	HasDNS  bool `json:"has_dns,omitempty"`
}

type State struct {
	mu      sync.RWMutex
	Sites   map[string]SiteInfo `json:"sites"`
	FileUID int                 `json:"-"`
	FileGID int                 `json:"-"`
}

type SiteView struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	HasDNS  bool   `json:"has_dns,omitempty"`
	HasWWW  bool   `json:"has_www,omitempty"`
}

func LoadState(path string) (*State, error) {
	st := &State{Sites: map[string]SiteInfo{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := SaveState(path, st); err != nil {
				return nil, err
			}
			return st, nil
		}
		return nil, err
	}

	if len(data) == 0 {
		st.Sites = map[string]SiteInfo{}
		return st, nil
	}

	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	if st.Sites == nil {
		st.Sites = map[string]SiteInfo{}
	}
	return st, nil
}

func SaveState(path string, s *State) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	payload, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	tempFile := path + ".tmp"
	if err := os.WriteFile(tempFile, payload, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tempFile, path); err != nil {
		return err
	}
	// Best-effort chown using configured PUID/PGID (Unraid compatibility).
	if s.FileUID != 0 || s.FileGID != 0 {
		_ = os.Chown(path, s.FileUID, s.FileGID)
	}
	return nil
}

func (s *State) AddSite(site string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Sites[site]; ok {
		return
	}
	s.Sites[site] = SiteInfo{
		Enabled: false,
	}
}

func (s *State) RemoveSite(site string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Sites, site)
}

func (s *State) SetEnabled(site string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.Sites[site]
	info.Enabled = enabled
	s.Sites[site] = info
}

func (s *State) SetHasDNS(site string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.Sites[site]
	info.HasDNS = v
	s.Sites[site] = info
}

func (s *State) DNSManagedSites() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sites []string
	for name, info := range s.Sites {
		if info.HasDNS {
			sites = append(sites, name)
		}
	}
	sort.Strings(sites)
	return sites
}

func (s *State) EnabledSites() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sites []string
	for name, info := range s.Sites {
		if info.Enabled {
			sites = append(sites, name)
		}
	}
	sort.Strings(sites)
	return sites
}

func (s *State) AllSiteNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sites []string
	for name := range s.Sites {
		sites = append(sites, name)
	}
	sort.Strings(sites)
	return sites
}

func (s *State) AllSites() []SiteView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var output []SiteView
	for name, info := range s.Sites {
		output = append(output, SiteView{Name: name, Enabled: info.Enabled, HasDNS: info.HasDNS})
	}
	sort.Slice(output, func(i, j int) bool {
		return output[i].Name < output[j].Name
	})
	return output
}

func (s *State) Save(path string) error {
	return SaveState(path, s)
}
