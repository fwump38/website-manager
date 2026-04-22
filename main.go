package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	SitesDir        string
	CaddyAdminURL   string
	CaddyfilePath   string
	CaddyServiceURL string
	DashboardBind   string
	DashboardPort   string
	CFAPIToken      string
	CFAccountID     string
	CFTunnelID      string
	CFTunnelHost    string
	TemplatesDir    string
	FileUID         int
	FileGID         int
	MailgunAPIKey   string
	MailgunDomain   string
}

func parseEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

func loadConfig() Config {
	cfg := Config{
		SitesDir:        os.Getenv("SITES_DIR"),
		CaddyAdminURL:   os.Getenv("CADDY_ADMIN_URL"),
		CaddyfilePath:   os.Getenv("CADDYFILE_OUTPUT"),
		CaddyServiceURL: os.Getenv("CADDY_SERVICE_URL"),
		DashboardBind:   os.Getenv("DASHBOARD_BIND"),
		DashboardPort:   os.Getenv("DASHBOARD_PORT"),
		CFAPIToken:      os.Getenv("CF_API_TOKEN"),
		CFAccountID:     os.Getenv("CF_ACCOUNT_ID"),
		CFTunnelID:      os.Getenv("CF_TUNNEL_ID"),
		CFTunnelHost:    os.Getenv("CF_TUNNEL_HOSTNAME"),
		TemplatesDir:    "templates",
		FileUID:         parseEnvInt("PUID", 99),
		FileGID:         parseEnvInt("PGID", 100),
		MailgunAPIKey:   os.Getenv("MAILGUN_API_KEY"),
		MailgunDomain:   os.Getenv("MAILGUN_DOMAIN"),
	}

	if cfg.SitesDir == "" {
		cfg.SitesDir = "/sites"
	}
	if cfg.CaddyAdminURL == "" {
		cfg.CaddyAdminURL = "http://caddy:2019"
	}
	if cfg.CaddyfilePath == "" {
		cfg.CaddyfilePath = "/etc/caddy/Caddyfile"
	}
	if cfg.DashboardPort == "" {
		cfg.DashboardPort = "8080"
	}
	if cfg.CFTunnelHost == "" && cfg.CFTunnelID != "" {
		cfg.CFTunnelHost = cfg.CFTunnelID + ".cfargotunnel.com"
	}
	if cfg.CaddyServiceURL == "" {
		cfg.CaddyServiceURL = "http://caddy:80"
	}
	return cfg
}

func main() {
	cfg := loadConfig()
	logger := log.New(os.Stdout, "site-manager: ", log.LstdFlags|log.Lmsgprefix)

	state := NewState(cfg.SitesDir)
	state.FileUID = cfg.FileUID
	state.FileGID = cfg.FileGID

	reconcileCh := make(chan struct{}, 1)
	cfReconcileCh := make(chan struct{}, 1)
	cfClient := NewCloudflareClient(cfg, logger)
	caddy := &CaddyManager{
		SitesDir:     cfg.SitesDir,
		TemplatePath: filepath.Join(cfg.TemplatesDir, "Caddyfile.tmpl"),
		OutputPath:   cfg.CaddyfilePath,
		AdminURL:     cfg.CaddyAdminURL,
	}

	migrateSiteConfigs(cfg.SitesDir)

	if err := StartWatcher(cfg.SitesDir, state, reconcileCh, logger); err != nil {
		logger.Fatalf("failed to start watcher: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// Clear the preview cookie when returning to the dashboard.
			http.SetCookie(w, &http.Cookie{
				Name:     previewCookieName,
				Value:    "",
				Path:     "/",
				MaxAge:   -1,
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			http.ServeFile(w, r, filepath.Join(cfg.TemplatesDir, "dashboard.html"))
			return
		}
		// Fallback: if a preview cookie is set, proxy absolute-path requests
		// (e.g. /assets/...) to Caddy for the previewed site.
		handlePreviewFallback(state, cfg.CaddyServiceURL, w, r)
	}))
	mux.HandleFunc("/preview/", func(w http.ResponseWriter, r *http.Request) {
		handlePreview(state, cfg.CaddyServiceURL, w, r)
	})
	mux.HandleFunc("/api/sites", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleSites(state, cfClient, w, r)
		case http.MethodPost:
			handleCreateSite(state, cfg.SitesDir, cfClient, reconcileCh, cfg.FileUID, cfg.FileGID, logger, w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/sites/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			handleSitePatch(state, cfg.SitesDir, cfg.FileUID, cfg.FileGID, reconcileCh, cfReconcileCh, w, r)
		case http.MethodDelete:
			handleDeleteSite(state, cfg.SitesDir, reconcileCh, cfReconcileCh, logger, w, r)
		case http.MethodPost:
			if strings.HasSuffix(r.URL.Path, "/optimize-images") {
				handleOptimizeImages(state, cfg.SitesDir, cfg.FileUID, cfg.FileGID, logger, w, r)
			} else if strings.HasSuffix(r.URL.Path, "/redownload") {
				handleRedownloadSite(state, cfg.SitesDir, cfg.FileUID, cfg.FileGID, reconcileCh, logger, w, r)
			} else {
				http.Error(w, "not found", http.StatusNotFound)
			}
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/contact", func(w http.ResponseWriter, r *http.Request) {
		handleContact(state, cfg, logger, w, r)
	})
	mux.HandleFunc("/api/domains", func(w http.ResponseWriter, r *http.Request) {
		handleDomains(cfClient, w, r)
	})
	mux.HandleFunc("/api/dns-check", func(w http.ResponseWriter, r *http.Request) {
		handleDNSCheck(state, w, r)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    cfg.DashboardBind + ":" + cfg.DashboardPort,
		Handler: mux,
		// ReadHeaderTimeout guards against slow-header (Slowloris) attacks.
		// ReadTimeout and WriteTimeout are intentionally omitted: this server
		// contains reverse-proxy handlers (preview) that stream responses from
		// Caddy over the same connection; a server-level write deadline would
		// race with the proxy's response copy and close connections prematurely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Printf("dashboard listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("dashboard server failed: %v", err)
		}
	}()

	// initial reconcile
	reconcileOnce(state, cfg, caddy, cfClient, logger)

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-reconcileCh:
				reconcileCaddy(state, cfg, caddy, cfClient, logger)
			case <-cfReconcileCh:
				reconcileCloudflare(state, cfg, cfClient, logger)
			case <-signalCtx.Done():
				return
			}
		}
	}()

	<-signalCtx.Done()
	logger.Println("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	wg.Wait()
}

func reconcileOnce(state *State, cfg Config, caddy *CaddyManager, cf *CloudflareClient, logger *log.Logger) {
	reconcileCaddy(state, cfg, caddy, cf, logger)
	reconcileCloudflare(state, cfg, cf, logger)
}

// apexOf returns the bare apex domain for a hostname (last two dot-separated
// labels), e.g. "my.site.com" → "site.com", "site.com" → "site.com".
func apexOf(hostname string) string {
	parts := strings.Split(hostname, ".")
	if len(parts) <= 2 {
		return hostname
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

func reconcileCaddy(state *State, cfg Config, caddy *CaddyManager, cf *CloudflareClient, logger *log.Logger) {
	allSiteNames := state.AllSiteNames()
	allSiteSet := make(map[string]bool, len(allSiteNames))
	for _, s := range allSiteNames {
		allSiteSet[strings.ToLower(s)] = true
	}

	var siteEntries []siteEntry
	for _, site := range allSiteNames {
		siteCfg, _ := loadSiteConfig(cfg.SitesDir, site)
		entry := siteEntry{Name: site, ContactEnabled: siteCfg.ContactEnabled}
		if siteCfg.Enabled {
			apex := apexOf(site)
			wwwHost := "www." + apex
			if siteCfg.ServeAtWWW && !allSiteSet[strings.ToLower(wwwHost)] {
				entry.ExtraHosts = append(entry.ExtraHosts, wwwHost)
			}
			if siteCfg.ServeAtApex && site != apex && !allSiteSet[strings.ToLower(apex)] {
				entry.ExtraHosts = append(entry.ExtraHosts, apex)
			}
		}
		siteEntries = append(siteEntries, entry)
	}
	if err := caddy.GenerateCaddyfile(siteEntries); err != nil {
		logger.Printf("failed to generate caddyfile: %v", err)
		return
	}
	if err := caddy.Reload(); err != nil {
		logger.Printf("failed to reload caddy: %v", err)
	}
}

func reconcileCloudflare(state *State, cfg Config, cf *CloudflareClient, logger *log.Logger) {
	if cfg.CFAPIToken == "" || cfg.CFAccountID == "" || cfg.CFTunnelID == "" || cfg.CFTunnelHost == "" {
		logger.Println("cloudflare configuration incomplete, skipping Cloudflare sync")
		return
	}
	enabledSites := state.EnabledSites()
	if err := cf.Reconcile(state, cfg.SitesDir, enabledSites); err != nil {
		logger.Printf("failed to reconcile cloudflare: %v", err)
	}
}
