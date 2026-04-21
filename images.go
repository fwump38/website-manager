package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/chai2010/webp"
	"golang.org/x/image/draw"
)

// variantSuffixRe matches filenames that are already generated size variants
// (e.g. photo-800.png, photo-1200.webp) so they are not treated as sources.
var variantSuffixRe = regexp.MustCompile(`-(800|1200)\.(png|jpg|jpeg|webp)$`)

var optimizeSizes = []int{800, 1200}

const (
	webpQuality      = 85
	maxImageFileSize = 50 * 1024 * 1024 // 50 MB
)

// OptimizeResult is the JSON response for the optimize-images endpoint.
type OptimizeResult struct {
	Optimized int           `json:"optimized"`
	Images    []ImageResult `json:"images"`
}

// ImageResult describes the optimization output for a single source image.
type ImageResult struct {
	Source   string   `json:"source"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Variants []string `json:"variants"`
	HTML     string   `json:"html"`
}

// optimizeSiteImages scans the site's assets/images directory and generates
// resized PNG + WebP variants for every source image found.
func optimizeSiteImages(sitesDir, siteName string, uid, gid int) (*OptimizeResult, error) {
	imagesDir := filepath.Join(sitesDir, siteName, "assets", "images")

	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &OptimizeResult{Images: []ImageResult{}}, nil
		}
		return nil, fmt.Errorf("reading images directory: %w", err)
	}

	hasOwnership := uid != 0 || gid != 0
	result := &OptimizeResult{Images: []ImageResult{}}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		lower := strings.ToLower(name)

		// Delete macOS AppleDouble metadata files (._<name>).
		if strings.HasPrefix(name, "._") {
			_ = os.Remove(filepath.Join(imagesDir, name))
			continue
		}

		// Skip FUSE temporary files (e.g. .fuse_hidden* from UnRaid/FUSE mounts).
		if strings.HasPrefix(lower, ".fuse_hidden") {
			continue
		}

		// Only process PNG, JPG, JPEG source files.
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
			continue
		}

		// Skip files that are already generated variants.
		if variantSuffixRe.MatchString(lower) {
			continue
		}

		imgResult, err := processSourceImage(imagesDir, name, uid, gid, hasOwnership)
		if err != nil {
			return nil, fmt.Errorf("processing %s: %w", name, err)
		}
		result.Images = append(result.Images, *imgResult)
		result.Optimized++
	}

	return result, nil
}

// processSourceImage generates all variants for a single source image file.
func processSourceImage(imagesDir, filename string, uid, gid int, hasOwnership bool) (*ImageResult, error) {
	srcPath := filepath.Join(imagesDir, filename)
	ext := strings.ToLower(filepath.Ext(filename))
	baseName := strings.TrimSuffix(filename, filepath.Ext(filename))

	// Reject files that exceed the size limit before decoding to prevent
	// memory exhaustion from crafted images (e.g. decompression bombs).
	info, err := os.Stat(srcPath)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxImageFileSize {
		return nil, fmt.Errorf("source image %s exceeds maximum allowed size (%d MB)", filename, maxImageFileSize/(1024*1024))
	}

	// Decode the source image.
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var src image.Image
	switch ext {
	case ".png":
		src, err = png.Decode(f)
	case ".jpg", ".jpeg":
		src, err = jpeg.Decode(f)
	default:
		return nil, fmt.Errorf("unsupported format: %s", ext)
	}
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", filename, err)
	}

	bounds := src.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()

	var variants []string

	// If source is JPEG, create a full-size PNG version.
	if ext == ".jpg" || ext == ".jpeg" {
		pngName := baseName + ".png"
		if err := savePNG(filepath.Join(imagesDir, pngName), src, uid, gid, hasOwnership); err != nil {
			return nil, err
		}
		variants = append(variants, pngName)
	}

	// Generate full-size WebP.
	webpName := baseName + ".webp"
	if err := saveWebP(filepath.Join(imagesDir, webpName), src, uid, gid, hasOwnership); err != nil {
		return nil, err
	}
	variants = append(variants, webpName)

	// Generate resized variants.
	for _, targetWidth := range optimizeSizes {
		if targetWidth >= srcWidth {
			continue // Skip if image is already smaller than or equal to target.
		}

		resized := resizeImage(src, targetWidth)
		suffix := fmt.Sprintf("-%d", targetWidth)

		// Resized PNG.
		pngName := baseName + suffix + ".png"
		if err := savePNG(filepath.Join(imagesDir, pngName), resized, uid, gid, hasOwnership); err != nil {
			return nil, err
		}
		variants = append(variants, pngName)

		// Resized WebP.
		webpName := baseName + suffix + ".webp"
		if err := saveWebP(filepath.Join(imagesDir, webpName), resized, uid, gid, hasOwnership); err != nil {
			return nil, err
		}
		variants = append(variants, webpName)
	}

	html := generatePictureHTML(baseName, srcWidth, srcHeight)

	return &ImageResult{
		Source:   filename,
		Width:    srcWidth,
		Height:   srcHeight,
		Variants: variants,
		HTML:     html,
	}, nil
}

// resizeImage scales an image to the given width, maintaining aspect ratio.
func resizeImage(src image.Image, targetWidth int) image.Image {
	bounds := src.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	targetHeight := (srcH * targetWidth) / srcW

	dst := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)
	return dst
}

// savePNG encodes an image as PNG and writes it to disk.
func savePNG(path string, img image.Image, uid, gid int, hasOwnership bool) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return fmt.Errorf("encoding PNG: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing PNG: %w", err)
	}
	if hasOwnership {
		_ = os.Chown(path, uid, gid)
	}
	return nil
}

// saveWebP encodes an image as WebP (lossy, quality 85) and writes it to disk.
func saveWebP(path string, img image.Image, uid, gid int, hasOwnership bool) error {
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, &webp.Options{Quality: webpQuality}); err != nil {
		return fmt.Errorf("encoding WebP: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing WebP: %w", err)
	}
	if hasOwnership {
		_ = os.Chown(path, uid, gid)
	}
	return nil
}

// generatePictureHTML builds a copy-pastable <picture> element with srcset
// for WebP and PNG sources, matching the site's existing conventions.
func generatePictureHTML(baseName string, width, height int) string {
	pathPrefix := "/assets/images/"
	pngFull := pathPrefix + baseName + ".png"
	webpFull := pathPrefix + baseName + ".webp"

	// Build srcset entries for each size that would have been generated.
	var webpSrcset, pngSrcset []string
	for _, w := range optimizeSizes {
		if w >= width {
			continue
		}
		webpSrcset = append(webpSrcset, fmt.Sprintf("%s%s-%d.webp %dw", pathPrefix, baseName, w, w))
		pngSrcset = append(pngSrcset, fmt.Sprintf("%s%s-%d.png %dw", pathPrefix, baseName, w, w))
	}
	webpSrcset = append(webpSrcset, fmt.Sprintf("%s %dw", webpFull, width))
	pngSrcset = append(pngSrcset, fmt.Sprintf("%s %dw", pngFull, width))

	return fmt.Sprintf(`<picture>
  <source type="image/webp"
    srcset="%s"
    sizes="100vw" />
  <source
    srcset="%s"
    sizes="100vw" />
  <img alt="" width="%d" height="%d"
    src="%s"
    decoding="async" loading="lazy" />
</picture>`,
		strings.Join(webpSrcset, ", "),
		strings.Join(pngSrcset, ", "),
		width, height, pngFull,
	)
}
