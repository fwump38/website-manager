package main

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

//go:embed site-templates
var siteTemplatesFS embed.FS

var availableTemplates = []string{"static-html"}

func createSiteFromTemplate(sitesDir, siteName, templateName string, logger *log.Logger) error {
	siteRoot := filepath.Join(sitesDir, siteName)

	// Determine nobody UID/GID for Unraid compatibility.
	uid, gid, hasOwnership := resolveNobodyIDs(logger)

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

// resolveNobodyIDs returns the UID and GID for the "nobody" user/group.
// If lookup fails (e.g. macOS dev environment), hasOwnership is false and
// chown operations are skipped.
func resolveNobodyIDs(logger *log.Logger) (uid, gid int, hasOwnership bool) {
	u, err := user.Lookup("nobody")
	if err != nil {
		logger.Printf("warning: could not look up 'nobody' user, skipping chown: %v", err)
		return 0, 0, false
	}
	g, err := user.LookupGroup("users")
	if err != nil {
		logger.Printf("warning: could not look up 'users' group, skipping chown: %v", err)
		return 0, 0, false
	}
	uidInt, err := strconv.Atoi(u.Uid)
	if err != nil {
		logger.Printf("warning: invalid nobody UID %q, skipping chown: %v", u.Uid, err)
		return 0, 0, false
	}
	gidInt, err := strconv.Atoi(g.Gid)
	if err != nil {
		logger.Printf("warning: invalid nobody GID %q, skipping chown: %v", g.Gid, err)
		return 0, 0, false
	}
	return uidInt, gidInt, true
}
