// Package avatar renders a deterministic hosted face for an agent: a small
// identicon derived from the agent's pubkey. The same key always renders the
// same image, so the CP can publish a stable HTTPS picture URL without storing
// or uploading user media.
package avatar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
)

const (
	// Size is the rendered square edge in pixels. Small enough for a profile
	// avatar, large enough to stay crisp in a profile header.
	Size = 256
	// grid is the identicon matrix edge (mirrored horizontally).
	grid = 5
)

// PNG renders the agent's deterministic identicon. The pubkey must be a
// 64-character lowercase hex key; the image is content-addressed by it.
func PNG(pubkey string) ([]byte, error) {
	if len(pubkey) != 64 {
		return nil, fmt.Errorf("avatar pubkey must be 64 hex characters")
	}
	if _, err := hex.DecodeString(pubkey); err != nil {
		return nil, fmt.Errorf("avatar pubkey is not hex: %w", err)
	}
	sum := sha256.Sum256([]byte(pubkey))
	bg := color.RGBA{R: sum[0], G: sum[1], B: sum[2], A: 255}
	fg := contrast(bg)
	img := image.NewRGBA(image.Rect(0, 0, Size, Size))
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			img.Set(x, y, bg)
		}
	}
	cell := Size / grid
	for gy := 0; gy < grid; gy++ {
		for gx := 0; gx < (grid+1)/2; gx++ {
			bit := int(sum[3+gy*grid+gx]) & 1
			if bit == 0 {
				continue
			}
			drawCell(img, gx, gy, cell, fg)
			// Mirror the identicon so it reads as a face-like glyph.
			if mirror := grid - 1 - gx; mirror != gx {
				drawCell(img, mirror, gy, cell, fg)
			}
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("encode avatar: %w", err)
	}
	return out.Bytes(), nil
}

// URL is the public HTTPS picture URL for an agent on the CP host.
func URL(cpHost, pubkey string) string {
	host := strings.TrimSpace(cpHost)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" || len(pubkey) != 64 {
		return ""
	}
	return "https://" + host + "/avatars/" + pubkey + ".png"
}

func drawCell(img *image.RGBA, gx, gy, cell int, c color.RGBA) {
	x0, y0 := gx*cell, gy*cell
	for y := y0; y < y0+cell; y++ {
		for x := x0; x < x0+cell; x++ {
			img.Set(x, y, c)
		}
	}
}

// contrast picks a foreground that stays visible on the hash-derived
// background.
func contrast(bg color.RGBA) color.RGBA {
	lum := 0.299*float64(bg.R) + 0.587*float64(bg.G) + 0.114*float64(bg.B)
	if lum > 140 {
		return color.RGBA{R: 20, G: 20, B: 20, A: 255}
	}
	return color.RGBA{R: 245, G: 245, B: 245, A: 255}
}
