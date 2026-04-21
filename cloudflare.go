package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	"github.com/cloudflare/cloudflare-go/v6/zones"
)

type CloudflareConfig struct {
	APIToken        string
	AccountID       string
	TunnelID        string
	TunnelHost      string
	CaddyServiceURL string
}

type CloudflareClient struct {
	client    *cloudflare.Client
	cfg       CloudflareConfig
	logger    *log.Logger
	zoneMu    sync.RWMutex
	zoneCache map[string]string // domain -> zone ID, nil = not yet loaded
}

func NewCloudflareClient(cfg Config, logger *log.Logger) *CloudflareClient {
	client := cloudflare.NewClient(
		option.WithAPIToken(cfg.CFAPIToken),
	)
	return &CloudflareClient{
		client: client,
		logger: logger,
		cfg: CloudflareConfig{
			APIToken:        cfg.CFAPIToken,
			AccountID:       cfg.CFAccountID,
			TunnelID:        cfg.CFTunnelID,
			TunnelHost:      cfg.CFTunnelHost,
			CaddyServiceURL: cfg.CaddyServiceURL,
		},
	}
}

// getZoneMap returns a domain→zoneID map, fetching it from the Cloudflare API
// on the first successful call and caching the result for the lifetime of the
// process. Returns an empty map if the API token is absent or a call fails
// (the next call will retry).
func (c *CloudflareClient) getZoneMap(ctx context.Context) map[string]string {
	if c.cfg.APIToken == "" {
		return map[string]string{}
	}

	c.zoneMu.RLock()
	if c.zoneCache != nil {
		m := c.zoneCache
		c.zoneMu.RUnlock()
		return m
	}
	c.zoneMu.RUnlock()

	c.zoneMu.Lock()
	defer c.zoneMu.Unlock()
	if c.zoneCache != nil { // double-check after acquiring write lock
		return c.zoneCache
	}

	m := map[string]string{}
	pager := c.client.Zones.ListAutoPaging(ctx, zones.ZoneListParams{})
	for pager.Next() {
		z := pager.Current()
		m[strings.ToLower(z.Name)] = z.ID
	}
	if err := pager.Err(); err != nil {
		c.logger.Printf("failed to list Cloudflare zones: %v", err)
		return m // don't cache on error so the next call retries
	}
	c.zoneCache = m
	c.logger.Printf("loaded %d Cloudflare zone(s)", len(m))
	return m
}

// HasWWWForSite reports whether the www redirect is enabled for the given
// apex-domain site based on its site.json configuration.
func (c *CloudflareClient) HasWWWForSite(siteName, sitesDir string) bool {
	if strings.HasPrefix(siteName, "www.") {
		return false
	}
	zoneMap := c.getZoneMap(context.Background())
	if _, isApex := zoneMap[strings.ToLower(siteName)]; !isApex {
		return false
	}
	siteCfg, _ := loadSiteConfig(sitesDir, siteName)
	return siteCfg.WWWRedirect
}

// AvailableDomains returns the sorted list of domain names this client has
// zone access to, discovered via the Cloudflare API.
func (c *CloudflareClient) AvailableDomains() []string {
	zoneMap := c.getZoneMap(context.Background())
	domains := make([]string, 0, len(zoneMap))
	for domain := range zoneMap {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	return domains
}

func (c *CloudflareClient) zoneIDForHostname(ctx context.Context, hostname string) (string, bool) {
	zoneMap := c.getZoneMap(ctx)
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	var bestMatch string
	for domain := range zoneMap {
		if hostname == domain || strings.HasSuffix(hostname, "."+domain) {
			if len(domain) > len(bestMatch) {
				bestMatch = domain
			}
		}
	}
	if bestMatch == "" {
		return "", false
	}
	return zoneMap[bestMatch], true
}

func (c *CloudflareClient) Reconcile(state *State, sitesDir string, enabledSites []string) error {
	ctx := context.Background()
	c.logger.Printf("Starting Cloudflare reconciliation for enabled sites: %v", enabledSites)
	enabledMap := map[string]bool{}
	for _, site := range enabledSites {
		enabledMap[site] = true
	}
	allSites := state.AllSiteNames()

	// Capture previously managed sites BEFORE modifying HasDNS flags so that
	// reconcileIngress can identify (and remove) tunnel rules for sites that
	// are being disabled in this reconciliation pass.
	previouslyManagedSites := state.DNSManagedSites()

	// Ensure DNS for each enabled site and its derived hostnames (e.g. www variant).
	// HasDNS is tracked in each site's site.json.
	for _, site := range enabledSites {
		for _, hostname := range c.getManagedHostnames([]string{site}, allSites, sitesDir) {
			c.logger.Printf("Ensuring DNS for %s", hostname)
			if err := c.ensureDNS(ctx, hostname); err != nil {
				return err
			}
		}
		if err := state.SetHasDNS(site, true); err != nil {
			return fmt.Errorf("failed to save has_dns for %s: %w", site, err)
		}
	}

	c.logger.Printf("Reconciling tunnel ingress")
	if err := c.reconcileIngress(ctx, sitesDir, enabledSites, allSites, previouslyManagedSites); err != nil {
		return err
	}
	return nil
}

func (c *CloudflareClient) ensureDNS(ctx context.Context, hostname string) error {
	zoneID, ok := c.zoneIDForHostname(ctx, hostname)
	if !ok {
		return fmt.Errorf("no zone configured for hostname %s", hostname)
	}
	recordID, err := c.getDNSRecordID(ctx, zoneID, hostname)
	if err != nil {
		return err
	}
	if recordID != "" {
		return nil
	}
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

func (c *CloudflareClient) getDNSRecordID(ctx context.Context, zoneID, hostname string) (string, error) {
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

func (c *CloudflareClient) getManagedHostnames(enabledSites []string, allSites []string, sitesDir string) []string {
	zoneMap := c.getZoneMap(context.Background())
	hostnames := make([]string, 0, len(enabledSites)*2)
	hostSet := make(map[string]bool)
	allSiteSet := make(map[string]bool)
	for _, s := range allSites {
		allSiteSet[strings.ToLower(s)] = true
	}
	for _, site := range enabledSites {
		if !hostSet[site] {
			hostnames = append(hostnames, site)
			hostSet[site] = true
		}
		// Only add a www. entry for apex domains whose site.json has WWWRedirect=true.
		// Subdomains like foo.example.com must not get a www.foo.example.com entry.
		// Also skip if www.SITE already exists as a managed folder.
		if !strings.HasPrefix(site, "www.") {
			if _, isApex := zoneMap[strings.ToLower(site)]; isApex {
				siteCfg, _ := loadSiteConfig(sitesDir, site)
				if siteCfg.WWWRedirect {
					www := "www." + site
					if !allSiteSet[strings.ToLower(www)] && !hostSet[www] {
						hostnames = append(hostnames, www)
						hostSet[www] = true
					}
				}
			}
		}
	}
	return hostnames
}

func (c *CloudflareClient) getTunnelConfig(ctx context.Context) (*zero_trust.TunnelCloudflaredConfigurationGetResponse, error) {
	return c.client.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(ctx, c.cfg.TunnelID,
		zero_trust.TunnelCloudflaredConfigurationGetParams{
			AccountID: cloudflare.F(c.cfg.AccountID),
		},
	)
}

func (c *CloudflareClient) reconcileIngress(ctx context.Context, sitesDir string, enabledSites []string, allSites []string, previouslyManagedSites []string) error {
	current, err := c.getTunnelConfig(ctx)
	if err != nil {
		return err
	}
	managedHostnames := c.getManagedHostnames(enabledSites, allSites, sitesDir)

	// Build the set of all hostnames this tool owns: currently enabled and
	// previously managed (HasDNS=true). Rules for owned hostnames are removed
	// so disabled sites get cleaned from the tunnel, not left behind.
	ownedHostnames := c.getManagedHostnames(append(enabledSites, previouslyManagedSites...), allSites, sitesDir)
	ownedSet := make(map[string]bool)
	for _, h := range ownedHostnames {
		ownedSet[h] = true
	}

	// Preserve ingress rules not managed by this tool.
	var unmanaged []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
	for _, rule := range current.Config.Ingress {
		if ownedSet[rule.Hostname] {
			continue // drop: will be re-added only if still enabled
		}
		if rule.Hostname == "" && rule.Service == "http_status:404" {
			continue // skip existing catch-all; we append a fresh one below
		}
		unmanaged = append(unmanaged, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Hostname: cloudflare.F(rule.Hostname),
			Service:  cloudflare.F(rule.Service),
		})
	}

	newIngress := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(unmanaged)+len(managedHostnames)+1)
	newIngress = append(newIngress, unmanaged...)
	for _, hostname := range managedHostnames {
		newIngress = append(newIngress, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Hostname: cloudflare.F(hostname),
			Service:  cloudflare.F(c.cfg.CaddyServiceURL),
		})
	}
	// Catch-all rule (no hostname).
	newIngress = append(newIngress, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
		Service: cloudflare.F("http_status:404"),
	})

	_, err = c.client.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, c.cfg.TunnelID,
		zero_trust.TunnelCloudflaredConfigurationUpdateParams{
			AccountID: cloudflare.F(c.cfg.AccountID),
			Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
				Ingress: cloudflare.F(newIngress),
			}),
		},
	)
	return err
}

func (c *CloudflareClient) deleteDNS(ctx context.Context, hostname string) error {
	zoneID, ok := c.zoneIDForHostname(ctx, hostname)
	if !ok {
		return nil
	}
	recordID, err := c.getDNSRecordID(ctx, zoneID, hostname)
	if err != nil {
		return err
	}
	if recordID == "" {
		return nil
	}
	_, err = c.client.DNS.Records.Delete(ctx, recordID, dns.RecordDeleteParams{
		ZoneID: cloudflare.F(zoneID),
	})
	return err
}
