package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type CloudflareConfig struct {
	APIToken   string
	AccountID  string
	ZoneMap    map[string]string
	TunnelID   string
	TunnelHost string
}

type CloudflareClient struct {
	cfg        CloudflareConfig
	httpClient *http.Client
}

type cfAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiResponse[T any] struct {
	Success  bool         `json:"success"`
	Errors   []cfAPIError `json:"errors"`
	Messages []string     `json:"messages"`
	Result   T            `json:"result"`
}

type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

type tunnelConfigResult struct {
	Ingress []map[string]any `json:"ingress"`
}

func NewCloudflareClient(cfg Config) *CloudflareClient {
	zoneMap, _ := parseZoneMap(cfg.CFZoneMap, cfg.CFZoneID, cfg.CFZoneDomain)
	return &CloudflareClient{
		cfg: CloudflareConfig{
			APIToken:   cfg.CFAPIToken,
			AccountID:  cfg.CFAccountID,
			ZoneMap:    zoneMap,
			TunnelID:   cfg.CFTunnelID,
			TunnelHost: cfg.CFTunnelHost,
		},
		httpClient: &http.Client{Timeout: 15 * time.Second},
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

	for _, site := range enabledSites {
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
		return fmt.Errorf("no zone configured for hostname %s", hostname)
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
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", url.PathEscape(zoneID), url.PathEscape(record.ID))
	return c.doRequest(http.MethodDelete, endpoint, nil, nil)
}

func (c *CloudflareClient) getDNSRecord(hostname string) (*dnsRecord, error) {
	zoneID, ok := c.zoneIDForHostname(hostname)
	if !ok {
		return nil, nil
	}
	apiURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?type=CNAME&name=%s", url.PathEscape(zoneID), url.QueryEscape(hostname))
	var response apiResponse[[]dnsRecord]
	if err := c.doRequest(http.MethodGet, apiURL, nil, &response); err != nil {
		return nil, err
	}
	if len(response.Result) == 0 {
		return nil, nil
	}
	return &response.Result[0], nil
}

func (c *CloudflareClient) reconcileIngress(enabledSites []string) error {
	ingress := make([]map[string]any, 0, len(enabledSites)+1)
	for _, site := range enabledSites {
		ingress = append(ingress, map[string]any{
			"hostname": site,
			"service":  "http://caddy:80",
		})
	}
	ingress = append(ingress, map[string]any{"service": "http_status:404"})

	config := map[string]any{"ingress": ingress}
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/configurations", url.PathEscape(c.cfg.AccountID), url.PathEscape(c.cfg.TunnelID))
	var result apiResponse[tunnelConfigResult]
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
				lastErr = fmt.Errorf("cloudflare api error status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
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
