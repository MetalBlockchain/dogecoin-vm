package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
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
	{Path: "/developers", File: "developers", Title: "DogecoinVM API reference",
		Description: "The DogecoinVM and bridge API: balances, transactions, deposits, withdrawals, proof of reserves and JSON-RPC, with examples, and how to build a wallet."},
	{Path: "/run-a-node", File: "run-a-node", Title: "Run a DogecoinVM node",
		Description: "Run a Metal node that tracks the DogecoinVM L1 and serves its JSON-RPC: build, configure, run with systemd, check sync."},
	{Path: "/changelog", File: "changelog", Title: "DogecoinVM changelog",
		Description: "What changed in DogecoinVM, the DOGE bridge and the Mac wallet, newest first."},
	{Path: "/terms", File: "terms", Title: "DogecoinVM alpha terms of use",
		Description: "The terms for using DogecoinVM and its DOGE bridge during the alpha: the risks, the limits, held deposits and refunds."},
}

// notFoundPage is served, with status 404, for any path that is neither a
// page nor a file. It has no path of its own, so it isn't in the sitemap.
var notFoundPage = sitePage{File: "notfound", Title: "Page not found · DogecoinVM",
	Description: "There's no page at this address on the DogecoinVM site."}

// siteOrigin is where the site is published, for links that must be
// absolute: link previews, the canonical link and the sitemap.
const siteOrigin = "https://metaldoge.com"

func renderPage(p sitePage) ([]byte, error) {
	t, err := template.ParseFS(pageFiles, "pages/layout.html", "pages/"+p.File+".html")
	if err != nil {
		return nil, fmt.Errorf("page %s: %w", p.File, err)
	}
	p.Nav = siteNav
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", p); err != nil {
		return nil, fmt.Errorf("page %s: %w", p.File, err)
	}
	return buf.Bytes(), nil
}

// renderPages builds every page, keyed by its path.
func renderPages() (map[string][]byte, error) {
	out := make(map[string][]byte, len(sitePages))
	for _, p := range sitePages {
		body, err := renderPage(p)
		if err != nil {
			return nil, err
		}
		out[p.Path] = body
	}
	return out, nil
}

// sitemap lists every page, for search engines.
func sitemap() []byte {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	buf.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, p := range sitePages {
		fmt.Fprintf(&buf, "  <url><loc>%s%s</loc></url>\n", siteOrigin, p.Path)
	}
	buf.WriteString("</urlset>\n")
	return buf.Bytes()
}

// staticFiles serves the files in static. Anything else, a directory
// included, gets the site's 404 page rather than Go's plain one or a
// listing. A wallet should always run its current code, so browsers check
// for a newer file each time rather than reusing a cached one.
func staticFiles(static fs.FS, notFound []byte) http.Handler {
	files := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		name := path.Clean("/" + r.URL.Path)[1:]
		if st, err := fs.Stat(static, name); name == "" || err != nil || st.IsDir() {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(notFound)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// handlePages serves the static files, the pages at their clean URLs, the
// sitemap and the 404 page, and sends the old file names to their pages.
func handlePages(mux *http.ServeMux, static fs.FS) error {
	pages, err := renderPages()
	if err != nil {
		return err
	}
	notFound, err := renderPage(notFoundPage)
	if err != nil {
		return err
	}
	mux.Handle("GET /", staticFiles(static, notFound))
	siteMap := sitemap()
	mux.HandleFunc("GET /sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(siteMap)
	})
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
