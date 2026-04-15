package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/option"
)

type CloudflareConfig struct {
	APIToken          string
	AccountID         string
	ZoneMap           map[string]string
	TunnelID          string
	TunnelHost        string
	EnableWWWRedirect bool
	CaddyServiceURL   string
}

type CloudflareClient struct {
	client *cloudflare.Client
	cfg    CloudflareConfig
	logger *log.Logger
}

func NewCloudflareClient(cfg Config, logger *log.Logger) *CloudflareClient {
	zoneMap, _ := parseZoneMap(cfg.CFZoneMap, cfg.CFZoneID, cfg.CFZoneDomain)
	client := cloudflare.NewClient(
		option.WithAPIToken(cfg.CFAPIToken),
	)
	return &CloudflareClient{
		client: client,
		logger: logger,
		cfg: CloudflareConfig{
			APIToken:          cfg.CFAPIToken,
			AccountID:         cfg.CFAccountID,
			ZoneMap:           zoneMap,
			TunnelID:          cfg.CFTunnelID,
			TunnelHost:        cfg.CFTunnelHost,
			EnableWWWRedirect: cfg.CFEnableWWWRedirect,
			CaddyServiceURL:   cfg.CaddyServiceURL,
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

func (c *CloudflareClient) Reconcile(state *State, stateFile string, enabledSites []string) error {
	c.logger.Printf("Starting Cloudflare reconciliation for enabled sites: %v", enabledSites)
	enabledMap := map[string]bool{}
	for _, site := range enabledSites {
		enabledMap[site] = true
	}

	// Ensure DNS for each enabled site and its derived hostnames (e.g. www variant).
	// HasDNS is tracked on the base site name only so it survives directory sync.
	for _, site := range enabledSites {
		for _, hostname := range c.getManagedHostnames([]string{site}) {
			c.logger.Printf("Ensuring DNS for %s", hostname)
			if err := c.ensureDNS(hostname); err != nil {
				return err
			}
		}
		state.SetHasDNS(site, true)
	}
	if err := state.Save(stateFile); err != nil {
		return fmt.Errorf("failed to save state after DNS ensure: %w", err)
	}

	// Delete DNS only for sites that were previously managed (HasDNS=true) and are now disabled.
	for _, site := range state.DNSManagedSites() {
		if !enabledMap[site] {
			for _, hostname := range c.getManagedHostnames([]string{site}) {
				c.logger.Printf("Deleting DNS for %s", hostname)
				if err := c.deleteDNS(hostname); err != nil {
					return err
				}
			}
			state.SetHasDNS(site, false)
		}
	}
	if err := state.Save(stateFile); err != nil {
		return fmt.Errorf("failed to save state after DNS delete: %w", err)
	}

	c.logger.Printf("Reconciling tunnel ingress")
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
	recordID, err := c.getDNSRecordID(zoneID, hostname)
	if err != nil {
		return err
	}
	if recordID != "" {
		return nil
	}

	ctx := context.Background()
	_, err = c.client.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.F(zoneID),
		Body: dns.CNAMERecordParam{
			Name:    cloudflare.F(hostname),
			Content: cloudflare.F(c.cfg.TunnelHost),
			Proxied: cloudflare.F(true),
			TTL:     cloudflare.F(dns.TTL(1)),
			Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
		},
	})
	return err
}

func (c *CloudflareClient) getDNSRecordID(zoneID, hostname string) (string, error) {
	ctx := context.Background()
	page, err := c.client.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(zoneID),
		Type:   cloudflare.F(dns.RecordListParamsTypeCNAME),
		Name:   cloudflare.F(dns.RecordListParamsName{Exact: cloudflare.F(hostname)}),
	})
	if err != nil {
		return "", err
	}
	if len(page.Result) == 0 {
		return "", nil
	}
	return page.Result[0].ID, nil
}

type TunnelConfigResult struct {
	Config TunnelConfig `json:"config"`
}

type TunnelConfig struct {
	Ingress     []map[string]interface{} `json:"ingress"`
	WarpRouting map[string]interface{}   `json:"warp-routing"`
}

func (c *CloudflareClient) getTunnelConfig() (*TunnelConfigResult, error) {
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", c.cfg.AccountID, c.cfg.TunnelID)
	var response struct {
		Result TunnelConfigResult `json:"result"`
	}
	if err := c.client.Get(context.Background(), path, nil, &response); err != nil {
		return nil, err
	}
	return &response.Result, nil
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
	newIngress := make([]map[string]interface{}, 0, len(unmanaged)+len(managedHostnames)+1)
	newIngress = append(newIngress, unmanaged...)
	for _, hostname := range managedHostnames {
		newIngress = append(newIngress, map[string]interface{}{
			"hostname":      hostname,
			"service":       c.cfg.CaddyServiceURL,
			"originRequest": map[string]interface{}{},
		})
	}
	newIngress = append(newIngress, map[string]interface{}{"service": "http_status:404"})

	body := map[string]interface{}{
		"config": map[string]interface{}{
			"ingress":      newIngress,
			"warp-routing": current.Config.WarpRouting,
		},
	}
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", c.cfg.AccountID, c.cfg.TunnelID)
	return c.client.Put(context.Background(), path, body, nil)
}

func (c *CloudflareClient) deleteDNS(hostname string) error {
	zoneID, ok := c.zoneIDForHostname(hostname)
	if !ok {
		return nil
	}
	recordID, err := c.getDNSRecordID(zoneID, hostname)
	if err != nil {
		return err
	}
	if recordID == "" {
		return nil
	}
	ctx := context.Background()
	_, err = c.client.DNS.Records.Delete(ctx, recordID, dns.RecordDeleteParams{
		ZoneID: cloudflare.F(zoneID),
	})
	return err
}
