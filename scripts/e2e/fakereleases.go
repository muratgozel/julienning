//go:build ignore

// fakereleases stands in for https://github.com/<repo>/releases in
// scripts/e2e.sh, serving exactly what scripts/install.sh and
// internal/selfupdate ask for:
//
//	GET|HEAD /releases/latest                 302 to /releases/tag/<tag>, where
//	                                          <tag> is the content of <root>/LATEST,
//	                                          re-read per request so the harness
//	                                          "publishes" by rewriting that file
//	GET|HEAD /releases/download/<tag>/<asset> the file <root>/<tag>/<asset>
//
// Build and run: go build -o fakereleases scripts/e2e/fakereleases.go &&
// ./fakereleases -root DIR [-port N]. It prints one line,
// "listening on http://127.0.0.1:<port>", once it accepts connections.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func main() {
	root := flag.String("root", "", "directory holding LATEST and one <tag>/ dir per release")
	port := flag.Int("port", 0, "port to listen on (0 picks a free one)")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "fakereleases: -root is required")
		os.Exit(2)
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakereleases:", err)
		os.Exit(2)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakereleases:", err)
		os.Exit(1)
	}
	base := "http://" + ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		raw, err := os.ReadFile(filepath.Join(abs, "LATEST"))
		tag := strings.TrimSpace(string(raw))
		if err != nil || !nameRe.MatchString(tag) {
			// GitHub sends /releases/latest to /releases when nothing is published.
			http.Redirect(w, r, base+"/releases", http.StatusFound)
			return
		}
		http.Redirect(w, r, base+"/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/releases/download/")
		tag, asset, ok := strings.Cut(rest, "/")
		if !ok || !nameRe.MatchString(tag) || !nameRe.MatchString(asset) {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(abs, tag, asset))
	})

	fmt.Printf("listening on %s\n", base)
	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintln(os.Stderr, "fakereleases:", err)
		os.Exit(1)
	}
}
