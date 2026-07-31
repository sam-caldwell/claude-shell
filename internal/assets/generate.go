//go:build ignore

// generate.go compresses build assets into the data/ directory for embedding.
// Run via: go generate ./internal/assets/
package main

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	// Map source files (relative to repo root) to output names in data/
	assets := map[string]string{
		"Dockerfile":     "data/Dockerfile.gz",
		"entrypoint.sh":  "data/entrypoint.sh.gz",
		"skel/CLAUDE.md": "data/CLAUDE.md.gz",
	}

	// Resolve repo root: this script runs from internal/assets/
	repoRoot := filepath.Join("..", "..")

	for src, dst := range assets {
		srcPath := filepath.Join(repoRoot, src)
		data, err := os.ReadFile(srcPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", srcPath, err)
			os.Exit(1)
		}

		f, err := os.Create(dst)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", dst, err)
			os.Exit(1)
		}

		w, err := gzip.NewWriterLevel(f, gzip.BestCompression)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gzip writer: %v\n", err)
			os.Exit(1)
		}

		if _, err := w.Write(data); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", dst, err)
			os.Exit(1)
		}
		if err := w.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close gzip %s: %v\n", dst, err)
			os.Exit(1)
		}
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close %s: %v\n", dst, err)
			os.Exit(1)
		}

		fmt.Printf("compressed %s -> %s (%d bytes)\n", src, dst, len(data))
	}
}
