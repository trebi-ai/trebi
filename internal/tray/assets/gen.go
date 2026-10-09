//go:build ignore

// gen.go renders the tray icons: the favicon hexagon as template PNGs.
// Run: go run ./internal/tray/assets/gen.go
// Outputs running.png (filled) and stopped.png (outline), 36x36 for 18pt @2x.
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
	size    = 36
	apothem = 13.2 // half the width between the flat sides
	corner  = 2.0  // corner radius, as in the favicon
	stroke  = 3.0
	samples = 4 // per axis, for anti-aliasing
)

func main() {
	_, thisFile, _, _ := runtime.Caller(0)
	outDir := filepath.Dir(thisFile)
	writePNG(filepath.Join(outDir, "running.png"), render(func(d float64) bool { return d < 0 }))
	writePNG(filepath.Join(outDir, "stopped.png"), render(func(d float64) bool { return d < 0 && d > -stroke }))
}

func render(inside func(d float64) bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	c := float64(size) / 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			hits := 0
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					px := float64(x) + (float64(sx)+0.5)/samples - c
					py := float64(y) + (float64(sy)+0.5)/samples - c
					if inside(hexagon(px, py)) {
						hits++
					}
				}
			}
			a := uint8(255 * hits / (samples * samples))
			img.SetNRGBA(x, y, color.NRGBA{A: a})
		}
	}
	return img
}

// hexagon is the signed distance to a pointy-top hexagon with round corners.
func hexagon(x, y float64) float64 {
	// Swap the axes: the base shape has flat top and bottom sides.
	px, py := math.Abs(y), math.Abs(x)
	const kx, ky, kz = -0.866025404, 0.5, 0.577350269
	r := apothem - corner
	dot := math.Min(kx*px+ky*py, 0)
	px -= 2 * dot * kx
	py -= 2 * dot * ky
	px -= math.Max(-kz*r, math.Min(px, kz*r))
	py -= r
	return math.Hypot(px, py)*math.Copysign(1, py) - corner
}

func writePNG(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}
