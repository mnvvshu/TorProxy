// Package tray provides a Windows notification-area (system tray) icon.
//
// The Windows implementation uses raw Win32 calls (no CGO). The icon is drawn
// at runtime and tinted according to the aggregate proxy status so users can
// see health at a glance. On other platforms New returns ErrUnsupported.
package tray

import (
	"errors"
	"math"
)

// ErrUnsupported is returned by New on platforms without a tray implementation.
var ErrUnsupported = errors.New("tray: not supported on this platform")

// Status selects the tint of the tray icon.
type Status int

const (
	StatusStopped  Status = iota // grey - nothing running
	StatusStarting               // violet - bootstrapping in progress
	StatusRunning                // green - all endpoints healthy
	StatusDegraded               // amber - some endpoints failed
	StatusError                  // red - manager error / nothing healthy
)

// Callbacks are invoked (on their own goroutine) when menu items are chosen.
// Any nil callback hides or disables the corresponding menu item.
type Callbacks struct {
	OnOpenDashboard func()
	OnCopyProxies   func()
	OnStartAll      func()
	OnStopAll       func()
	OnRestartFailed func()
	OnNewIdentity   func()
	OnExit          func()
}

// MenuState controls which lifecycle items are enabled in the context menu.
type MenuState struct {
	Header     string // first, disabled line (e.g. "180 / 200 endpoints running")
	CanStart   bool
	CanStop    bool
	HasFailed  bool
	HasRunning bool
}

type rgb struct{ r, g, b float64 }

func hex(v uint32) rgb {
	return rgb{float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)}
}

// palette returns the (light, dark) gradient endpoints for a status.
func palette(s Status) (rgb, rgb) {
	switch s {
	case StatusRunning:
		return hex(0x6ee7b7), hex(0x059669)
	case StatusStarting:
		return hex(0xc4b5fd), hex(0x7c3aed)
	case StatusDegraded:
		return hex(0xfde68a), hex(0xd97706)
	case StatusError:
		return hex(0xfca5a5), hex(0xdc2626)
	default:
		return hex(0xcbd5e1), hex(0x475569)
	}
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// RenderIcon draws the "onion" status icon as straight-alpha RGBA pixels
// (row-major, top-down, 4 bytes per pixel). It is platform independent so the
// same artwork can be used for the executable icon (see tools/genicon).
func RenderIcon(size int, s Status) []byte {
	light, dark := palette(s)
	px := make([]byte, size*size*4)
	fs := float64(size)
	c := fs / 2
	R := fs * 0.47
	stroke := math.Max(1.2, fs*0.075)
	rings := []float64{R * 0.66, R * 0.36}
	dotR := R * 0.15

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-c, float64(y)+0.5-c
			d := math.Hypot(dx, dy)
			cov := clamp01(R - d + 0.5)
			if cov == 0 {
				continue
			}
			// Diagonal gradient: light at top-left, dark at bottom-right.
			t := clamp01((float64(x) + float64(y)) / (2 * fs))
			col := rgb{
				light.r + (dark.r-light.r)*t,
				light.g + (dark.g-light.g)*t,
				light.b + (dark.b-light.b)*t,
			}
			// White onion layers + centre dot.
			white := 0.0
			for _, r := range rings {
				white = math.Max(white, clamp01(stroke/2-math.Abs(d-r)+0.5)*0.92)
			}
			white = math.Max(white, clamp01(dotR-d+0.5))
			col.r += (255 - col.r) * white
			col.g += (255 - col.g) * white
			col.b += (255 - col.b) * white
			// Subtle darker rim for contrast on light taskbars.
			rim := clamp01(stroke*0.6-math.Abs(d-(R-stroke*0.3))+0.5) * 0.35
			col.r *= 1 - rim
			col.g *= 1 - rim
			col.b *= 1 - rim

			i := (y*size + x) * 4
			px[i+0] = byte(col.r + 0.5)
			px[i+1] = byte(col.g + 0.5)
			px[i+2] = byte(col.b + 0.5)
			px[i+3] = byte(cov*255 + 0.5)
		}
	}
	return px
}
