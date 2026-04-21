package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/template"
)

type CaddyManager struct {
	SitesDir     string
	TemplatePath string
	OutputPath   string
	AdminURL     string
}

type caddyData struct {
	Sites      []siteEntry
	SitesDir   string
	WWWEntries []wwwEntry
}

type siteEntry struct {
	Name           string
	ContactEnabled bool
}

// wwwEntry represents an apex domain that should have a www→apex redirect block.
type wwwEntry struct {
	WWWName string // e.g. "www.example.com"
	Apex    string // e.g. "example.com"
}

// GenerateCaddyfile writes the Caddyfile from the template. sites includes all
// known site entries (enabled + disabled). wwwRedirects is a list of apex
// domain names for which a www.APEX → APEX redirect block should be emitted.
func (c *CaddyManager) GenerateCaddyfile(sites []siteEntry, wwwRedirects []string) error {
	tmpl, err := template.ParseFiles(c.TemplatePath)
	if err != nil {
		return err
	}

	data := caddyData{SitesDir: c.SitesDir, Sites: sites}
	for _, apex := range wwwRedirects {
		data.WWWEntries = append(data.WWWEntries, wwwEntry{
			WWWName: "www." + apex,
			Apex:    apex,
		})
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}

	if err := os.WriteFile(c.OutputPath, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return nil
}

func (c *CaddyManager) Reload() error {
	data, err := os.ReadFile(c.OutputPath)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/load", c.AdminURL), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("caddy reload failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}
