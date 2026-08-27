//go:build ignore

// gen.go renders tray icons to template PNGs.
// Run: go run ./internal/tray/assets/gen.go
// Outputs idle.png and alert.png (44x44). Pure-Go.
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
)

const (
	size   = 44
	stroke = 5.5
)

func main() {
	_, thisFile, _, _ := runtime.Caller(0)
	outDir := filepath.Dir(thisFile)
	writePNG(filepath.Join(outDir, "idle.png"), drawIdle())
	writePNG(filepath.Join(outDir, "alert.png"), drawAlert())
}

// drawIdle: bold serif-ish "T" for Trebi (template black).
func drawIdle() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	// Top bar of T.
	strokeLine(img, s*0.25, s*0.28, s*0.75, s*0.28, stroke)
	// Stem.
	strokeLine(img, s*0.50, s*0.28, s*0.50, s*0.78, stroke)
	return img
}

var attentionOrange = color.NRGBA{R: 0xFF, G: 0x8C, B: 0x00, A: 0xFF}

func drawAlert() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	thick := stroke + 2
	c := attentionOrange
	strokeLineC(img, s*0.22, s*0.26, s*0.78, s*0.26, thick, c)
	strokeLineC(img, s*0.50, s*0.26, s*0.50, s*0.78, thick, c)
	fillCircleC(img, s*0.82, s*0.22, s*0.16, color.NRGBA{R: 0xFF, G: 0x3B, B: 0x30, A: 0xFF})
	return img
}

func strokeLine(img *image.NRGBA, x0, y0, x1, y1, thick float64) {
	strokeLineC(img, x0, y0, x1, y1, thick, color.NRGBA{0, 0, 0, 255})
}

func strokeLineC(img *image.NRGBA, x0, y0, x1, y1, thick float64, c color.NRGBA) {
	dx, dy := x1-x0, y1-y0
	length := math.Hypot(dx, dy)
	if length == 0 {
		return
	}
	minX := int(math.Floor(math.Min(x0, x1) - thick))
	maxX := int(math.Ceil(math.Max(x0, x1) + thick))
	minY := int(math.Floor(math.Min(y0, y1) - thick))
	maxY := int(math.Ceil(math.Max(y0, y1) + thick))
	for py := minY; py <= maxY; py++ {
		for px := minX; px <= maxX; px++ {
			if px < 0 || py < 0 || px >= size || py >= size {
				continue
			}
			// Distance from point to segment.
			t := ((float64(px)-x0)*dx + (float64(py)-y0)*dy) / (length * length)
			if t < 0 {
				t = 0
			} else if t > 1 {
				t = 1
			}
			cx, cy := x0+t*dx, y0+t*dy
			d := math.Hypot(float64(px)-cx, float64(py)-cy)
			if d <= thick/2 {
				a := uint8(255)
				if d > thick/2-1 {
					a = uint8(255 * (thick/2 - d))
				}
				setBlend(img, px, py, c, a)
			}
		}
	}
}

func fillCircleC(img *image.NRGBA, cx, cy, r float64, c color.NRGBA) {
	minX := int(math.Floor(cx - r - 1))
	maxX := int(math.Ceil(cx + r + 1))
	minY := int(math.Floor(cy - r - 1))
	maxY := int(math.Ceil(cy + r + 1))
	for py := minY; py <= maxY; py++ {
		for px := minX; px <= maxX; px++ {
			if px < 0 || py < 0 || px >= size || py >= size {
				continue
			}
			d := math.Hypot(float64(px)-cx, float64(py)-cy)
			if d <= r {
				a := uint8(255)
				if d > r-1 {
					a = uint8(255 * (r - d))
				}
				setBlend(img, px, py, c, a)
			}
		}
	}
}

func setBlend(img *image.NRGBA, x, y int, c color.NRGBA, a uint8) {
	i := img.PixOffset(x, y)
	if a == 255 {
		img.Pix[i] = c.R
		img.Pix[i+1] = c.G
		img.Pix[i+2] = c.B
		img.Pix[i+3] = c.A
		return
	}
	// Over transparent.
	fa := float64(a) / 255
	img.Pix[i] = uint8(float64(c.R)*fa + float64(img.Pix[i])*(1-fa))
	img.Pix[i+1] = uint8(float64(c.G)*fa + float64(img.Pix[i+1])*(1-fa))
	img.Pix[i+2] = uint8(float64(c.B)*fa + float64(img.Pix[i+2])*(1-fa))
	img.Pix[i+3] = uint8(math.Min(255, float64(img.Pix[i+3])+float64(a)))
}

func writePNG(path string, img *image.NRGBA) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}
