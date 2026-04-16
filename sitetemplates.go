package main

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

//go:embed site-templates
var siteTemplatesFS embed.FS

var availableTemplates = []string{"static-html"}

func createSiteFromTemplate(sitesDir, siteName, templateName string, uid, gid int, logger *log.Logger) error {
	siteRoot := filepath.Join(sitesDir, siteName)

	hasOwnership := uid != 0 || gid != 0

	// Walk the embedded template and replicate it under siteRoot.
	templateRoot := filepath.Join("site-templates", templateName)
	err := fs.WalkDir(siteTemplatesFS, templateRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Compute destination path relative to siteRoot.
		rel, err := filepath.Rel(templateRoot, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(siteRoot, rel)

		if d.IsDir() {
			if mkErr := os.MkdirAll(dest, 0o755); mkErr != nil {
				return mkErr
			}
			if hasOwnership {
				if chErr := os.Chown(dest, uid, gid); chErr != nil {
					logger.Printf("warning: could not chown %s: %v", dest, chErr)
				}
			}
			return nil
		}

		// It's a file — copy it.
		if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
			return mkErr
		}
		if err := copyEmbeddedFile(path, dest); err != nil {
			return err
		}
		if hasOwnership {
			if chErr := os.Chown(dest, uid, gid); chErr != nil {
				logger.Printf("warning: could not chown %s: %v", dest, chErr)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("creating site from template %q: %w", templateName, err)
	}

	// Create empty subdirectories not present in the template.
	extraDirs := []string{
		filepath.Join(siteRoot, "assets", "js"),
		filepath.Join(siteRoot, "assets", "images"),
	}
	for _, dir := range extraDirs {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return mkErr
		}
		if hasOwnership {
			if chErr := os.Chown(dir, uid, gid); chErr != nil {
				logger.Printf("warning: could not chown %s: %v", dir, chErr)
			}
		}
	}

	return nil
}

func copyEmbeddedFile(src, dest string) error {
	in, err := siteTemplatesFS.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// resolveNobodyIDs is intentionally removed; use PUID/PGID env vars instead.
