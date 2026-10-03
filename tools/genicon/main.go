// Command genicon renders the application icon (the same artwork used for the
// tray icon) into PNG files consumed by go-winres to embed the executable icon.
//
//	go run ./tools/genicon -out winres
package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	"torproxymanager/internal/tray"
)

func main() {
	out := flag.String("out", "winres", "output directory")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, size := range []int{256, 128, 64, 48, 32, 24, 16} {
		img := image.NewNRGBA(image.Rect(0, 0, size, size))
		copy(img.Pix, tray.RenderIcon(size, tray.StatusStarting)) // brand violet
		path := filepath.Join(*out, fmt.Sprintf("icon_%d.png", size))
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := png.Encode(f, img); err != nil {
			f.Close()
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		f.Close()
	}
	fmt.Println("icons written to", *out)
}
