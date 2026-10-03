package tray

import "testing"

func TestRenderIcon(t *testing.T) {
	for _, size := range []int{16, 24, 32, 256} {
		for s := StatusStopped; s <= StatusError; s++ {
			px := RenderIcon(size, s)
			if len(px) != size*size*4 {
				t.Fatalf("size %d: got %d bytes", size, len(px))
			}
			// Corners are transparent, centre is opaque.
			if px[3] != 0 {
				t.Errorf("size %d status %d: corner not transparent", size, s)
			}
			c := (size/2*size + size/2) * 4
			if px[c+3] != 255 {
				t.Errorf("size %d status %d: centre alpha %d", size, s, px[c+3])
			}
		}
	}
	// Different statuses must produce visibly different tints.
	a, b := RenderIcon(32, StatusRunning), RenderIcon(32, StatusError)
	i := (8*32 + 16) * 4
	if a[i] == b[i] && a[i+1] == b[i+1] && a[i+2] == b[i+2] {
		t.Errorf("running and error icons are identical")
	}
}
