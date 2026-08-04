package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func init() {
	registerOperation(&Operation{
		Name:     "download_asset",
		Title:    "Download a file",
		Path:     "/downloads",
		ReadOnly: false,
		Summary:  "Fetch a file and host it from this server.",
		Description: `Download a file - a PDF, an image, a document - and keep it, served back
from this server under a stable URL.

The searches hand back links, and a link is not always enough. The thing that
wants the file may have no network of its own, or the URL may be signed and
about to expire, or it may simply be easier to pass one address around than to
make everyone fetch the same thing again. This fetches it once and gives you a
URL on this server that keeps working.

It pairs with the tools that produce links: the PDF on a google_patents result,
an image from google_images, anything a search turned up.

Sample prompts:
  - "Download the PDF for that patent"
  - "Save that image and give me a link to it"
  - "Grab that CSV so I can look at it"

The stored URL is only reachable while this server is running with the HTTP
transport, and the file stays until delete_download removes it. Nothing is
deleted automatically.`,
		Schema: objectSchema([]string{"url"}, map[string]any{
			"url": stringProp("The file to download."),
			"filename": stringProp("What to call it. Only a label - it is a suggestion for " +
				"whoever saves the file, and does not decide where it is stored. Taken from the " +
				"source when left out."),
		}),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args struct {
				URL      string `json:"url"`
				Filename string `json:"filename"`
			}
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.URL) == "" {
				return nil, badInput("url is required")
			}
			download, err := s.downloads.Fetch(ctx, args.URL, args.Filename)
			if err != nil {
				return nil, err
			}
			return &DownloadResult{Download: *s.withDownloadURL(download)}, nil
		},
	})

	registerOperation(&Operation{
		Name:     "list_downloads",
		Title:    "List downloads",
		Path:     "/downloads/list",
		ReadOnly: true,
		Summary:  "List the files this server is holding.",
		Description: `List the files download_asset has stored, newest first, with the URL each
is served from and where it came from.

Sample prompts:
  - "What have you downloaded?"
  - "List the files you are holding and how big they are"`,
		Schema: objectSchema(nil, map[string]any{}),
		Run: func(_ context.Context, s *Server, _ json.RawMessage) (any, error) {
			downloads, err := s.downloads.List()
			if err != nil {
				return nil, err
			}
			listing := &DownloadListing{}
			for _, d := range downloads {
				listing.Downloads = append(listing.Downloads, *s.withDownloadURL(d))
				listing.Bytes += d.Bytes
			}
			listing.Count = len(listing.Downloads)
			if listing.Count == 0 {
				listing.Notes = append(listing.Notes, "nothing has been downloaded yet")
			}
			if !s.cfg.DownloadsServed() {
				listing.Notes = append(listing.Notes, "these files are on disk but are not being "+
					"served: that needs the HTTP transport, and this server is on stdio")
			}
			return listing, nil
		},
	})

	registerOperation(&Operation{
		Name:     "delete_download",
		Title:    "Delete a download",
		Path:     "/downloads/delete",
		ReadOnly: false,
		Summary:  "Remove a stored file.",
		Description: `Delete a file download_asset stored, and stop serving it.

Nothing here is deleted on its own - not on a timer, and not to make room for a
new download - so this is how the store is kept from growing without end.

Sample prompts:
  - "Delete that download"
  - "Remove the file you saved earlier"`,
		Schema: objectSchema([]string{"id"}, map[string]any{
			"id": stringProp("The download's id, as returned by download_asset or list_downloads."),
		}),
		Run: func(_ context.Context, s *Server, raw json.RawMessage) (any, error) {
			id, err := oneString(raw, "id")
			if err != nil {
				return nil, err
			}
			download, err := s.downloads.Get(id)
			if err != nil {
				return nil, err
			}
			if err := s.downloads.Delete(id); err != nil {
				return nil, err
			}
			return &DownloadDeleted{ID: id, Filename: download.Filename, Bytes: download.Bytes}, nil
		},
	})
}

// withDownloadURL fills in where a stored file is served from. It is a method
// on the server because only the server knows the address it is reachable at,
// which the store - which only knows about disk - does not.
func (s *Server) withDownloadURL(d *Download) *Download {
	if !s.cfg.DownloadsServed() {
		return d
	}
	d.URL = strings.TrimSuffix(s.cfg.BaseURL(), "/") + s.cfg.API.DownloadsPath + "/" + d.ID
	return d
}

// DownloadResult is one stored file.
type DownloadResult struct {
	Download
}

func (r *DownloadResult) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Downloaded %s (%s, %s)\n", r.Filename, r.ContentType, humanBytes(r.Bytes))
	fmt.Fprintf(&b, "  id:     %s\n", r.ID)
	fmt.Fprintf(&b, "  from:   %s\n", r.SourceURL)
	if r.URL != "" {
		fmt.Fprintf(&b, "  served: %s\n", r.URL)
	} else {
		fmt.Fprintf(&b, "  stored: %s\n", r.Path)
		b.WriteString("  (not served: that needs the HTTP transport, and this server is on stdio)\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// DownloadListing is everything the store is holding.
type DownloadListing struct {
	Count     int        `json:"count"`
	Bytes     int64      `json:"bytes"`
	Downloads []Download `json:"downloads"`
	Notes     []string   `json:"notes,omitempty"`
}

func (l *DownloadListing) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d downloads, %s\n", l.Count, humanBytes(l.Bytes))
	for _, note := range l.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	for _, d := range l.Downloads {
		fmt.Fprintf(&b, "\n%s  %s (%s)\n", d.ID, d.Filename, humanBytes(d.Bytes))
		fmt.Fprintf(&b, "  from:   %s\n", d.SourceURL)
		if d.URL != "" {
			fmt.Fprintf(&b, "  served: %s\n", d.URL)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// DownloadDeleted reports what was removed.
type DownloadDeleted struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Bytes    int64  `json:"bytes"`
	Deleted  bool   `json:"deleted"`
}

func (d *DownloadDeleted) Render() string {
	return fmt.Sprintf("Deleted %s (%s, %s)", d.Filename, d.ID, humanBytes(d.Bytes))
}

// ---------------------------------------------------------------------------
// Serving the stored files
// ---------------------------------------------------------------------------

// handleDownload serves one stored file.
func (a *RESTAPI) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeAPIError(w, http.StatusMethodNotAllowed, "", "stored files are read-only")
		return
	}

	id := strings.TrimPrefix(r.URL.Path, a.cfg.API.DownloadsPath+"/")
	id = strings.Trim(id, "/")
	if id == "" {
		writeAPIError(w, http.StatusNotFound, "",
			"name a download; list_downloads, or POST "+a.cfg.API.BasePath+"/downloads/list, shows them")
		return
	}

	download, err := a.server.downloads.Get(id)
	if err != nil {
		// The same answer for a malformed id and an unknown one. Distinguishing
		// them would say whether a guess had the right shape, which is the only
		// thing an unguessable id is protecting.
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(download.Path)
	if err != nil {
		a.logger.Printf("serving download %s: %v", id, err)
		writeAPIError(w, http.StatusNotFound, "", "that download's file is no longer on disk")
		return
	}
	defer file.Close() //nolint:errcheck

	info, err := file.Stat()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "", "could not read that download")
		return
	}

	h := w.Header()
	h.Set("Content-Type", download.ContentType)
	// Never sniffed. The type here came from whatever server the file was
	// fetched from, and a browser second-guessing it is how a file stored as
	// text becomes script.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", contentDisposition(download))
	// These files came from elsewhere and are served from this origin. A
	// restrictive policy means that even if something slips past the
	// disposition rules, it has nothing to reach.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
	h.Set("Cache-Control", "private, max-age=300")

	// ServeContent rather than io.Copy: it handles range requests, conditional
	// requests and HEAD, which is what makes a stored video seekable and a
	// large PDF resumable.
	http.ServeContent(w, r, download.Filename, info.ModTime(), file)
}

// inlineTypes are the types safe to render in place. Everything else is sent
// as an attachment.
//
// The rule is about what can execute, not about what is convenient. A stored
// image or PDF shown inline is a picture; a stored HTML file shown inline is
// script running on this server's origin, which on a tunnelled deployment is
// an origin someone may have granted a token to. SVG is xml with script in it
// and is not on this list for that reason, however much it is an image.
var inlineTypes = map[string]bool{
	"image/jpeg":      true,
	"image/png":       true,
	"image/gif":       true,
	"image/webp":      true,
	"image/avif":      true,
	"image/bmp":       true,
	"application/pdf": true,
	"text/plain":      true,
}

// contentDisposition decides whether a stored file is shown or saved.
func contentDisposition(d *Download) string {
	kind := "attachment"
	if inlineTypes[d.ContentType] {
		kind = "inline"
	}
	// The filename is quoted and also given in the RFC 5987 form, so a name
	// with a space or a non-ASCII character survives.
	return fmt.Sprintf("%s; filename=%q; filename*=UTF-8''%s",
		kind, sanitiseFilename(d.Filename), url.PathEscape(sanitiseFilename(d.Filename)))
}
