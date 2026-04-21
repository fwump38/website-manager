package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ContactSiteConfig is kept as a named alias for the relevant fields of SiteConfig
// so callers inside this file remain readable.
type ContactSiteConfig struct {
	To string
}

type contactRequest struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Type    string `json:"engagement_type"`
	Message string `json:"message"`
	Website string `json:"website"` // honeypot — must remain empty
}

// Per-IP rate limiter: max 3 submissions per 10-minute window.
type ipRateEntry struct {
	count     int
	windowEnd time.Time
}

var (
	rateMu  sync.Mutex
	rateMap = map[string]*ipRateEntry{}
)

const (
	contactRateLimit  = 3
	contactRateWindow = 10 * time.Minute
)

func checkContactRateLimit(ip string) bool {
	rateMu.Lock()
	defer rateMu.Unlock()
	now := time.Now()
	e, ok := rateMap[ip]
	if !ok || now.After(e.windowEnd) {
		rateMap[ip] = &ipRateEntry{count: 1, windowEnd: now.Add(contactRateWindow)}
		return true
	}
	if e.count >= contactRateLimit {
		return false
	}
	e.count++
	return true
}

func handleContact(cfg Config, logger *log.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// X-Site-Name is injected by Caddy's reverse_proxy header_up — it is trusted.
	siteName := r.Header.Get("X-Site-Name")
	if siteName == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Validate that the browser's Origin header matches the expected site (defence-in-depth).
	origin := r.Header.Get("Origin")
	if origin != "" {
		originHost := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
		if h, _, err := net.SplitHostPort(originHost); err == nil {
			originHost = h
		}
		if originHost != siteName {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	// Rate-limit by IP.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !checkContactRateLimit(ip) {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"too many requests, please try again later"}`, http.StatusTooManyRequests)
		return
	}

	// Parse and size-cap the request body.
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var req contactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}

	// Honeypot: bots fill the hidden "website" field; silently accept to avoid revealing the check.
	if req.Website != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"ok":true}`)
		return
	}

	// Field validation.
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Email) == "" || strings.TrimSpace(req.Message) == "" {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"name, email, and message are required"}`, http.StatusBadRequest)
		return
	}
	if len(req.Name) > 200 || len(req.Email) > 320 || len(req.Message) > 5000 || len(req.Type) > 200 {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"input too long"}`, http.StatusBadRequest)
		return
	}
	if !strings.Contains(req.Email, "@") || !strings.Contains(req.Email, ".") {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"invalid email address"}`, http.StatusBadRequest)
		return
	}

	// Load per-site config from site.json; 404 if contact form not enabled.
	siteCfg, err := loadSiteConfig(cfg.SitesDir, siteName)
	if err != nil {
		logger.Printf("contact: failed to load site config for %s: %v", siteName, err)
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"contact not configured for this site"}`, http.StatusNotFound)
		return
	}
	if !siteCfg.ContactEnabled || siteCfg.ContactTo == "" {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"contact not configured for this site"}`, http.StatusNotFound)
		return
	}
	contactCfg := &ContactSiteConfig{To: siteCfg.ContactTo}

	if err := sendMailgun(cfg.MailgunAPIKey, cfg.MailgunDomain, contactCfg, req); err != nil {
		logger.Printf("contact: mailgun error for site %s: %v", siteName, err)
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"failed to send message, please try again"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `{"ok":true}`)
}

func sendMailgun(apiKey, mgDomain string, contactCfg *ContactSiteConfig, req contactRequest) error {
	subject := fmt.Sprintf("Contact from %s", req.Name)
	if req.Type != "" && req.Type != "ENGAGEMENT_TYPE_" {
		subject = fmt.Sprintf("Contact from %s — %s", req.Name, req.Type)
	}

	body := fmt.Sprintf("Name: %s\nEmail: %s\nType: %s\n\n%s",
		req.Name, req.Email, req.Type, req.Message)

	from := fmt.Sprintf("contact-form@%s", mgDomain)

	form := url.Values{}
	form.Set("from", from)
	form.Set("to", contactCfg.To)
	form.Set("subject", subject)
	form.Set("text", body)
	form.Set("h:Reply-To", req.Email)

	endpoint := fmt.Sprintf("https://api.mailgun.net/v3/%s/messages", mgDomain)
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	httpReq.SetBasicAuth("api", apiKey)
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mailgun %d: %s", resp.StatusCode, respBody)
	}
	return nil
}
