package main

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
)

// downloadFiles lists everything the Hub hands to devices: the connector for
// each platform and the tmux bundle used when a device has no tmux. Bundles
// are optional; a Hub image built without them simply returns 404.
var downloadFiles = map[string]bool{
	"sinthmux-connector-darwin-amd64":      true,
	"sinthmux-connector-darwin-arm64":      true,
	"sinthmux-connector-linux-amd64":       true,
	"sinthmux-connector-linux-arm64":       true,
	"sinthmux-connector-windows-amd64.exe": true,
	"sinthmux-connector-windows-arm64.exe": true,
	"tmux-darwin-universal":                true,
	"tmux-linux-amd64":                     true,
	"tmux-linux-arm64":                     true,
	"tmux-windows-amd64.exe":               true,
	"tmux-windows-arm64.exe":               true,
	"SHA256SUMS":                           true,
}

func downloadHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		file := chi.URLParam(r, "file")
		if dir == "" || !downloadFiles[file] {
			http.NotFound(w, r)
			return
		}
		path := filepath.Join(dir, file)
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		if file == "SHA256SUMS" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		http.ServeFile(w, r, path)
	}
}
