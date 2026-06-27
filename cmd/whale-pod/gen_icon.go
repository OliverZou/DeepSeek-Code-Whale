//go:build ignore

package main

import (
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

func main() {
	// Read whale.png from the same directory as this script
	srcPath := filepath.Join(".", "whale.png")
	src, err := os.Open(srcPath)
	if err != nil {
		log.Fatalf("failed to open whale.png: %v", err)
	}
	defer src.Close()

	img, _, err := image.Decode(src)
	if err != nil {
		log.Fatalf("failed to decode whale.png: %v", err)
	}

	// Ensure build directory exists
	if err := os.MkdirAll("build", 0o755); err != nil {
		log.Fatalf("failed to create build dir: %v", err)
	}

	// Write appicon.png — Wails reads this to generate icon.ico internally
	outPath := filepath.Join("build", "appicon.png")
	dst, err := os.Create(outPath)
	if err != nil {
		log.Fatalf("failed to create %s: %v", outPath, err)
	}
	defer dst.Close()

	if err := png.Encode(dst, img); err != nil {
		log.Fatalf("failed to encode appicon.png: %v", err)
	}

	// Delete stale icon.ico so Wails regenerates it from appicon.png
	// using its own winicon.GenerateIcon (CatmullRom high-quality scaling).
	icoPath := filepath.Join("build", "windows", "icon.ico")
	os.Remove(icoPath) // best-effort, ignore error if not present

	fmt.Printf("Icon ready: %s (%dx%d)\n", outPath, img.Bounds().Dx(), img.Bounds().Dy())
}
