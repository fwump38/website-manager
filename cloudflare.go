package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/option"
)

type CloudflareConfig struct {
	APIToken          string
	AccountID         string
	ZoneMap           map[string]string
	TunnelID          string
	TunnelHost        string
	EnableWWWRedirect bool
}

type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

type CloudflareClient struct {
	client     *cloudflare.Client
	cfg        CloudflareConfig
	httpClient *http.Client
}

func NewCloudflareClient(cfg Config, logger *log.Logger) *CloudflareClient {
	zoneMap, _ := parseZoneMap(cfg.CFZoneMap, cfg.CFZoneID, cfg.CFZoneDomain)
	client := cloudflare.NewClient(
		option.WithAPIToken(cfg.CFAPIToken),
	)
	return &CloudflareClient{
		client:     client,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		cfg: CloudflareConfig{
			APIToken:          cfg.CFAPIToken,
			AccountID:         cfg.CFAccountID,
			ZoneMap:           zoneMap,
			TunnelID:          cfg.CFTunnelID,
			TunnelHost:        cfg.CFTunnelHost,
			EnableWWWRedirect: cfg.CFEnableWWWRedirect,
		},
	}
}

func parseZoneMap(rawMap, zoneID, zoneDomain string) (map[string]string, error) {
	zones := map[string]string{}
	if rawMap != "" {
		for _, entry := range strings.Split(rawMap, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			parts := strings.SplitN(entry, "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("invalid CF_ZONE_MAP entry: %s", entry)
			}
			domain := strings.ToLower(strings.TrimSpace(parts[0]))
			zones[domain] = strings.TrimSpace(parts[1])
		}
		return zones, nil
	}

	if zoneID != "" {
		if zoneDomain == "" {
			return nil, fmt.Errorf("CF_ZONE_DOMAIN is required when CF_ZONE_ID is set without CF_ZONE_MAP")
		}
		domain := strings.ToLower(strings.TrimSpace(zoneDomain))
		zones[domain] = strings.TrimSpace(zoneID)
	}
	return zones, nil
}

func (c *CloudflareClient) zoneIDForHostname(hostname string) (string, bool) {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	var bestMatch string
	for domain := range c.cfg.ZoneMap {
		if hostname == domain || strings.HasSuffix(hostname, "."+domain) {
			if len(domain) > len(bestMatch) {
				bestMatch = domain
			}
		}
	}
	if bestMatch == "" {
		return "", false
	}
	return c.cfg.ZoneMap[bestMatch], true
}

func (c *CloudflareClient) Reconcile(enabledSites, knownSites []string) error {
	enabledMap := map[string]bool{}
	for _, site := range enabledSites {
		enabledMap[site] = true
	}

	managedHostnames := c.getManagedHostnames(enabledSites)
	for _, site := range managedHostnames {
		if err := c.ensureDNS(site); err != nil {
			return err
		}
	}

	for _, site := range knownSites {
		if !enabledMap[site] {
			if err := c.deleteDNS(site); err != nil {
				return err
			}
		}
	}

	if err := c.reconcileIngress(enabledSites); err != nil {
		return err
	}
	return nil
}

func (c *CloudflareClient) ensureDNS(hostname string) error {
	zoneID, ok := c.zoneIDForHostname(hostname)
	if !ok {
		return errors.New("no zone configured for hostname")
	}
	record, err := c.getDNSRecord(hostname)
	if err != nil {
		return err
	}
	if record != nil {
		return nil
	}

	payload := dnsRecord{
		Type:    "CNAME",
		Name:    hostname,
		Content: c.cfg.TunnelHost,
		Proxied: true,
		TTL:     1,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", url.PathEscape(zoneID))
	var result dnsRecord
	if err := c.doRequest(http.MethodPost, endpoint, bytes.NewReader(body), &result); err != nil {
		return err
	}
	if result.ID == "" {
		return fmt.Errorf("failed to create dns record for %s", hostname)
	}
	return nil
}


func (c *CloudflareClient) getDNSRecord(hostname string) (*dnsRecord, error) {
	zoneID, ok := c.zoneIDForHostname(hostname)
	if !ok {
		return nil, nil
	}
	apiURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=CNAME&name=%s", url.PathEscape(zoneID), url.QueryEscape(hostname))
	var response struct {
		Success bool        `json:"success"`
		Errors  []struct{}  `json:"errors"`
		Result  []dnsRecord `json:"result"`
	}
	if err := c.doRequest(http.MethodGet, apiURL, nil, &response); err != nil {
		return nil, err
	}
	if len(response.Result) == 0 {
		return nil, nil
	}
	return &response.Result[0], nil
}

func (c *CloudflareClient) getTunnelConfig() (*TunnelConfigResult, error) {
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/configurations", url.PathEscape(c.cfg.AccountID), url.PathEscape(c.cfg.TunnelID))
	var response struct {
		Success bool               `json:"success"`
		Errors  []interface{}      `json:"errors"`
		Result  TunnelConfigResult `json:"result"`
	}
	if err := c.doRequest(http.MethodGet, endpoint, nil, &response); err != nil {
		return nil, err
	}
	return &response.Result, nil
}

type TunnelConfigResult struct {
	Config TunnelConfig `json:"config"`
}

type TunnelConfig struct {
	Ingress     []map[string]interface{} `json:"ingress"`
	WarpRouting map[string]interface{}   `json:"warp-routing"`
}

func (c *CloudflareClient) getManagedHostnames(enabledSites []string) []string {
	hostnames := make([]string, 0, len(enabledSites)*2)
	hostSet := make(map[string]bool)
	for _, site := range enabledSites {
		if !hostSet[site] {
			hostnames = append(hostnames, site)
			hostSet[site] = true
		}
		if c.cfg.EnableWWWRedirect && !strings.HasPrefix(site, "www.") {
			www := "www." + site
			if !hostSet[www] {
				hostnames = append(hostnames, www)
				hostSet[www] = true
			}
		}
	}
	return hostnames
}

func (c *CloudflareClient) reconcileIngress(enabledSites []string) error {
	current, err := c.getTunnelConfig()
	if err != nil {
		return err
	}
	managedHostnames := c.getManagedHostnames(enabledSites)
	managedSet := make(map[string]bool)
	for _, h := range managedHostnames {
		managedSet[h] = true
	}
	unmanaged := []map[string]interface{}{}
	for _, rule := range current.Config.Ingress {
		if hostname, ok := rule["hostname"].(string); ok {
			if managedSet[hostname] {
				continue
			}
		} else if service, ok := rule["service"].(string); ok && service == "http_status:404" {
			continue // skip existing catch-all
		}
		unmanaged = append(unmanaged, rule)
	}
	newIngress := make([]map[string]interface{}, 0, len(unmanaged) + len(managedHostnames) + 1)
	newIngress = append(newIngress, unmanaged...)
	for _, hostname := range managedHostnames {
		newIngress = append(newIngress, map[string]interface{}{
			"hostname":      hostname,
			"service":       "http://caddy:80",
			"originRequest": map[string]interface{}{},
		})
	}
	newIngress = append(newIngress, map[string]interface{}{"service": "http_status:404"})

	config := map[string]interface{}{
		"ingress":      newIngress,
		"warp-routing": current.Config.WarpRouting,
	}
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/configurations", url.PathEscape(c.cfg.AccountID), url.PathEscape(c.cfg.TunnelID))
	var result interface{}
	if err := c.doRequest(http.MethodPut, endpoint, bytes.NewReader(body), &result); err != nil {
		return err
	}
	return nil
}

func (c *CloudflareClient) doRequest(method, url string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				respBody, _ := io.ReadAll(resp.Body)
				lastErr = errors.New(fmt.Sprintf("cloudflare api error status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody))))
			} else if out != nil {
				decoder := json.NewDecoder(resp.Body)
				if err := decoder.Decode(out); err != nil {
					lastErr = err
				} else {
					return nil
				}
			} else {
				return nil
			}
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	return lastErr
}



func (c *CloudflareClient) deleteDNS(hostname string) error {
	zoneID, ok := c.zoneIDForHostname(hostname)
	if !ok {
		return nil
	}
	record, err := c.getDNSRecord(hostname)
	if err != nil {
		return err
	}
	if record == nil {
		return nil
	}
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones//dns_records/", url.PathEscape(zoneID), url.PathEscape(record.ID))
	return c.doRequest(http.MethodDelete, endpoint, nil, nil)
}
