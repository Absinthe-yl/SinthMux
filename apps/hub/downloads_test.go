package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestDownloadHandlerServesOnlyListedFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"tmux-linux-amd64": "tmux", "sinthmux-connector-windows-amd64.exe": "exe", "secret.txt": "no"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	router := chi.NewRouter()
	router.Get("/downloads/{file}", downloadHandler(dir))
	for path, want := range map[string]int{
		"/downloads/tmux-linux-amd64":                     http.StatusOK,
		"/downloads/sinthmux-connector-windows-amd64.exe": http.StatusOK,
		"/downloads/tmux-darwin-universal":                http.StatusNotFound, // listed but not built
		"/downloads/secret.txt":                           http.StatusNotFound,
		"/downloads/..%2Fsecret.txt":                      http.StatusNotFound,
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != want {
			t.Errorf("%s: status %d, want %d", path, recorder.Code, want)
		}
	}
	recorder := httptest.NewRecorder()
	chiRouter := chi.NewRouter()
	chiRouter.Get("/downloads/{file}", downloadHandler(""))
	chiRouter.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/downloads/tmux-linux-amd64", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("empty download dir served a file: %d", recorder.Code)
	}
}
