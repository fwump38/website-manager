package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	SitesDir             string
	StateFile            string
	CaddyAdminURL        string
	CaddyfilePath        string
	DashboardPort        string
	CFAPIToken           string
	CFAccountID          string
	CFTunnelID           string
	CFZoneID             string
	CFZoneDomain         string
	CFZoneMap            string
	CFTunnelHost         string
	CFEnableWWWRedirect  bool
	TemplatesDir         string
}

func loadConfig() Config {
	cfg := Config{
		SitesDir:             os.Getenv("SITES_DIR"),
		StateFile:            os.Getenv("STATE_FILE"),
		CaddyAdminURL:        os.Getenv("CADDY_ADMIN_URL"),
		CaddyfilePath:        os.Getenv("CADDYFILE_OUTPUT"),
		DashboardPort:        os.Getenv("DASHBOARD_PORT"),
		CFAPIToken:           os.Getenv("CF_API_TOKEN"),
		CFAccountID:          os.Getenv("CF_ACCOUNT_ID"),
		CFTunnelID:           os.Getenv("CF_TUNNEL_ID"),
		CFZoneID:             os.Getenv("CF_ZONE_ID"),
		CFZoneDomain:         os.Getenv("CF_ZONE_DOMAIN"),
		CFZoneMap:            os.Getenv("CF_ZONE_MAP"),
		CFTunnelHost:         os.Getenv("CF_TUNNEL_HOSTNAME"),
		CFEnableWWWRedirect:  os.Getenv("CF_ENABLE_WWW_REDIRECT") == "true",
		TemplatesDir:         "templates",
	}

	if cfg.SitesDir == "" {
		cfg.SitesDir = "/sites"
	}
	if cfg.StateFile == "" {
		cfg.StateFile = filepath.Join(cfg.SitesDir, "enabled.json")
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
	return cfg
}

func main() {
	cfg := loadConfig()
	logger := log.New(os.Stdout, "site-manager: ", log.LstdFlags|log.Lmsgprefix)

	state, err := LoadState(cfg.StateFile)
	if err != nil {
		logger.Fatalf("failed to load state: %v", err)
	}

	reconcileCh := make(chan struct{}, 1)
	cfClient := NewCloudflareClient(cfg)
	caddy := &CaddyManager{
		SitesDir:     cfg.SitesDir,
		TemplatePath: filepath.Join(cfg.TemplatesDir, "Caddyfile.tmpl"),
		OutputPath:   cfg.CaddyfilePath,
		AdminURL:     cfg.CaddyAdminURL,
	}

	if err := StartWatcher(cfg.SitesDir, state, cfg.StateFile, reconcileCh, logger); err != nil {
		logger.Fatalf("failed to start watcher: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, filepath.Join(cfg.TemplatesDir, "dashboard.html"))
			return
		}
		http.NotFound(w, r)
	}))
	mux.HandleFunc("/api/sites", func(w http.ResponseWriter, r *http.Request) {
		handleSites(state, cfg.StateFile, reconcileCh, w, r)
	})
	mux.HandleFunc("/api/sites/", func(w http.ResponseWriter, r *http.Request) {
		handleSitePatch(state, cfg.StateFile, reconcileCh, w, r)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    ":" + cfg.DashboardPort,
		Handler: mux,
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
				reconcileOnce(state, cfg, caddy, cfClient, logger)
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
	enabledSites := state.EnabledSites()
	if err := caddy.GenerateCaddyfile(enabledSites); err != nil {
		logger.Printf("failed to generate caddyfile: %v", err)
		return
	}
	if err := caddy.Reload(); err != nil {
		logger.Printf("failed to reload caddy: %v", err)
		return
	}
	if cfg.CFAPIToken == "" || cfg.CFAccountID == "" || cfg.CFTunnelID == "" || (cfg.CFZoneID == "" && cfg.CFZoneMap == "") || cfg.CFTunnelHost == "" {
		logger.Println("cloudflare configuration incomplete, skipping Cloudflare sync")
		return
	}
	if err := cf.Reconcile(enabledSites, state.AllSiteNames()); err != nil {
		logger.Printf("failed to reconcile cloudflare: %v", err)
	}
}
