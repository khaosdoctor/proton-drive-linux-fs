// Command gen draws the tray icons and writes them next to the tray package. Run it with
// "go generate ./internal/tray"; the PNGs it produces are committed.
//
// All four icons share one cloud glyph so they read as the same application: a solid glyph
// when the mount is online, a corner dot when data is moving, two corner bars when polling
// is paused, and a hollow outline when nothing is mounted.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

const (
	size    = 64 // icon edge in pixels
	samples = 4  // supersampling factor per axis, for smooth edges
)

// region reports whether the point (x, y) in icon coordinates is part of a shape.
type region func(x, y float64) bool

func circle(cx, cy, r float64) region {
	return func(x, y float64) bool {
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r*r
	}
}

// roundRect is the rectangle x0..x1 by y0..y1 with corners of radius r.
func roundRect(x0, y0, x1, y1, r float64) region {
	return func(x, y float64) bool {
		if x < x0 || x > x1 || y < y0 || y > y1 {
			return false
		}
		cx := min(max(x, x0+r), x1-r)
		cy := min(max(y, y0+r), y1-r)
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r*r
	}
}

func union(regions ...region) region {
	return func(x, y float64) bool {
		for _, r := range regions {
			if r(x, y) {
				return true
			}
		}
		return false
	}
}

// cloud is the base glyph shrunk by inset on every side, so an inset copy subtracted from
// the full one leaves an outline.
func cloud(inset float64) region {
	return union(
		roundRect(10+inset, 32+inset, 54-inset, 47-inset, 5),
		circle(23, 33, 11-inset),
		circle(37, 28, 15-inset),
		circle(47, 35, 9-inset),
	)
}

// badgeHole is the hole punched into the glyph so a corner badge stays readable against it.
var badgeHole = circle(50, 50, 11)

func solid() region { return cloud(0) }

func outline() region {
	full, inner := cloud(0), cloud(3.5)
	return func(x, y float64) bool { return full(x, y) && !inner(x, y) }
}

// withBadge subtracts the hole from base and draws mark in it.
func withBadge(base, mark region) region {
	return func(x, y float64) bool {
		if mark(x, y) {
			return true
		}
		return base(x, y) && !badgeHole(x, y)
	}
}

func render(r region) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	step := 1.0 / float64(samples)

	for py := range size {
		for px := range size {
			hits := 0
			for sy := range samples {
				for sx := range samples {
					x := float64(px) + (float64(sx)+0.5)*step
					y := float64(py) + (float64(sy)+0.5)*step
					if r(x, y) {
						hits++
					}
				}
			}
			if hits == 0 {
				continue
			}
			// White with per-pixel alpha; tray implementations recolour or mask as they see fit.
			a := uint8(hits * 255 / (samples * samples))
			img.SetRGBA(px, py, color.RGBA{R: a, G: a, B: a, A: a})
		}
	}

	return img
}

func write(path string, img *image.RGBA) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if err := png.Encode(f, img); err != nil {
		return err
	}
	return f.Close()
}

func main() {
	dot := circle(50, 50, 6.5)
	bars := union(
		roundRect(45.5, 44.5, 48.5, 55.5, 1),
		roundRect(51.5, 44.5, 54.5, 55.5, 1),
	)

	icons := map[string]region{
		"online.png":    solid(),
		"syncing.png":   withBadge(solid(), dot),
		"paused.png":    withBadge(solid(), bars),
		"loggedout.png": outline(),
	}

	for name, r := range icons {
		if err := write(name, render(r)); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}

	// The desktop entry needs the same glyph as a regular application icon.
	if err := write(filepath.Join("..", "..", "contrib", "icons", "proton-drive-fs.png"), render(solid())); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
