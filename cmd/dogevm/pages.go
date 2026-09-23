package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
)

// The site's pages are assembled from pages/layout.html (head, menu and
// footer) and each page's own file, once at startup.
//
//go:embed pages
var pageFiles embed.FS

type navLink struct {
	Path, Label string
	CTA         bool
}

var siteNav = []navLink{
	{Path: "/", Label: "Bridge"},
	{Path: "/explorer", Label: "Explorer"},
	{Path: "/roadmap", Label: "Roadmap"},
	{Path: "/docs", Label: "Docs"},
	{Path: "/download", Label: "Download", CTA: true},
}

type sitePage struct {
	Path, File, Title, Description string
	Nav                            []navLink
}

var sitePages = []sitePage{
	{Path: "/", File: "bridge", Title: "DogecoinVM bridge",
		Description: "Move DOGE between Dogecoin and DogecoinVM on Metal Blockchain, one for one."},
	{Path: "/explorer", File: "explorer", Title: "DogecoinVM explorer",
		Description: "Bridge activity, proof of reserves, and DogecoinVM blocks, transactions and addresses."},
	{Path: "/roadmap", File: "roadmap", Title: "DogecoinVM roadmap",
		Description: "What's running on DogecoinVM, how the DOGE bridge works, its known limits, and the plan to close them."},
	{Path: "/docs", File: "docs", Title: "DogecoinVM docs",
		Description: "How DogecoinVM and its DOGE bridge work: deposits, withdrawals, fees and timings, trust and limits, wallets and the API."},
	{Path: "/download", File: "download", Title: "DogecoinVM Wallet for Mac",
		Description: "A native Mac wallet for Dogecoin and DogecoinVM: review before you sign, Touch ID for every payment, signed and notarized by Apple."},
}

// renderPages builds every page, keyed by its path.
func renderPages() (map[string][]byte, error) {
	out := make(map[string][]byte, len(sitePages))
	for _, p := range sitePages {
		t, err := template.ParseFS(pageFiles, "pages/layout.html", "pages/"+p.File+".html")
		if err != nil {
			return nil, fmt.Errorf("page %s: %w", p.File, err)
		}
		p.Nav = siteNav
		var buf bytes.Buffer
		if err := t.ExecuteTemplate(&buf, "layout.html", p); err != nil {
			return nil, fmt.Errorf("page %s: %w", p.File, err)
		}
		out[p.Path] = buf.Bytes()
	}
	return out, nil
}

// handlePages serves the pages at their clean URLs, and sends the old
// file names there.
func handlePages(mux *http.ServeMux) error {
	pages, err := renderPages()
	if err != nil {
		return err
	}
	for path, body := range pages {
		pattern := "GET " + path
		if path == "/" {
			pattern = "GET /{$}"
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(body)
		})
	}
	for old, path := range map[string]string{
		"/index.html": "/", "/roadmap.html": "/roadmap", "/docs.html": "/docs", "/download.html": "/download",
	} {
		mux.Handle("GET "+old, http.RedirectHandler(path, http.StatusMovedPermanently))
	}
	return nil
}
