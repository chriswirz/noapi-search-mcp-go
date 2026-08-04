package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/playwright-community/playwright-go"
)

func init() {
	registerOperation(&Operation{
		Name:     "google_lens",
		Title:    "Reverse image search",
		Path:     "/lens",
		ReadOnly: true,
		Summary:  "Identify an image, or find where it appears.",
		Description: `Reverse image search with Google Lens: identify what is in a picture, find
the product, or find where else the image appears.

This is what gives a text-only model something like sight. Point it at a public
image URL or a file on this machine, and it comes back with Google's own
description of the image and the pages that match it.

Sample prompts:
  - "What is this? https://example.com/photo.jpg"
  - "Identify the building in /home/me/pictures/trip.jpg"
  - "What product is this a photo of?"

A local file is uploaded to Google. That is the only way Lens works, and it is
worth being deliberate about: do not point this at anything private.`,
		Schema: objectSchema([]string{"image"}, map[string]any{
			"image": stringProp("A public image URL, or the path to an image file on the machine " +
				"running this server."),
		}),
		Run: func(_ context.Context, s *Server, raw json.RawMessage) (any, error) {
			image, err := oneString(raw, "image")
			if err != nil {
				return nil, err
			}
			return s.scraper.lens(image)
		},
	})

	registerOperation(&Operation{
		Name:     "list_images",
		Title:    "List local images",
		Path:     "/images/local",
		ReadOnly: true,
		Summary:  "List image files in a directory, for use with google_lens.",
		Description: `List the image files in a directory on the machine running this server.

It exists to make google_lens usable when nobody can remember the filename:
list the directory, then hand one of the paths back to google_lens.

Sample prompts:
  - "What images are in my Downloads folder?"
  - "List the pictures in /home/me/screenshots"`,
		Schema: objectSchema(nil, map[string]any{
			"directory": stringProp("The directory to list. Defaults to the working directory. " +
				"A leading ~ is expanded."),
		}),
		Run: func(_ context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args struct {
				Directory string `json:"directory"`
			}
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			return listImages(args.Directory)
		},
	})
}

// imageExtensions is what list_images counts as an image.
var imageExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".webp": true, ".bmp": true, ".tiff": true, ".tif": true, ".avif": true,
}

// ImageListing is the contents of one directory.
type ImageListing struct {
	Directory string      `json:"directory"`
	Count     int         `json:"count"`
	Images    []ImageFile `json:"images"`
	Notes     []string    `json:"notes,omitempty"`
}

// ImageFile is one image on disk.
type ImageFile struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

func (l *ImageListing) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d images in %s\n", l.Count, l.Directory)
	for _, note := range l.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	for _, image := range l.Images {
		fmt.Fprintf(&b, "  %-40s %8d bytes  %s\n", image.Name, image.Bytes, image.Path)
	}
	return strings.TrimRight(b.String(), "\n")
}

// listImages reads a directory. It does not recurse: a tool that walks a home
// directory looking for pictures is a tool that hangs, and the caller can name
// the subdirectory.
func listImages(dir string) (*ImageListing, error) {
	expanded, err := expandPath(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(expanded)
	if err != nil {
		return nil, badInput("could not read %s: %v", expanded, err)
	}

	listing := &ImageListing{Directory: expanded}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !imageExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		listing.Images = append(listing.Images, ImageFile{
			Name:  entry.Name(),
			Path:  filepath.Join(expanded, entry.Name()),
			Bytes: info.Size(),
		})
	}
	sort.Slice(listing.Images, func(i, j int) bool { return listing.Images[i].Name < listing.Images[j].Name })
	listing.Count = len(listing.Images)
	if listing.Count == 0 {
		listing.Notes = append(listing.Notes, "no image files here; this does not look in subdirectories")
	}
	return listing, nil
}

// expandPath resolves ~ and makes a path absolute.
func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return os.Getwd()
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", badInput("could not resolve ~: %v", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path[1:], string(filepath.Separator)))
	}
	return filepath.Abs(path)
}

// ---------------------------------------------------------------------------
// google_lens
// ---------------------------------------------------------------------------

// LensResult is what Lens said about an image.
type LensResult struct {
	Image string `json:"image"`

	// Description is Google's own account of what the image shows, where it
	// offered one. It is the field that answers "what is this?".
	Description string `json:"description,omitempty"`

	Matches []LensMatch `json:"matches,omitempty"`
	Notes   []string    `json:"notes,omitempty"`
}

// LensMatch is one page Lens matched the image to.
type LensMatch struct {
	Title  string `json:"title"`
	URL    string `json:"url,omitempty"`
	Source string `json:"source,omitempty"`
	Price  string `json:"price,omitempty"`
}

func (r *LensResult) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Lens: %s\n", r.Image)
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	if r.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Description)
	}
	if len(r.Matches) > 0 {
		b.WriteString("\nMatches:\n")
		for i, match := range r.Matches {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, match.Title)
			if meta := joinNonEmpty(" - ", match.Source, match.Price); meta != "" {
				fmt.Fprintf(&b, "     %s\n", meta)
			}
			if match.URL != "" {
				fmt.Fprintf(&b, "     %s\n", match.URL)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// lens runs a reverse image search.
func (s *Scraper) lens(image string) (*LensResult, error) {
	local := !strings.HasPrefix(image, "http://") && !strings.HasPrefix(image, "https://")

	var file string
	if local {
		expanded, err := expandPath(image)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(expanded)
		if err != nil {
			return nil, badInput("no such file: %s. Give a path on the machine running this "+
				"server, or a public image URL", image)
		}
		if info.IsDir() {
			return nil, badInput("%s is a directory; name an image file (list_images will show them)", expanded)
		}
		file = expanded
	}

	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	if local {
		if err := page.uploadToLens(file); err != nil {
			return nil, err
		}
	} else {
		// The uploadbyurl endpoint is the whole flow for a URL: Lens fetches
		// the image itself and lands directly on the results.
		if err := page.Open("https://lens.google.com/uploadbyurl?hl=en&url=" + url.QueryEscape(image)); err != nil {
			return nil, err
		}
	}
	// Lens processes the image after the page arrives, and there is no
	// element that means "done" - the results fill in progressively.
	page.Settle(4000)

	if text, err := page.Locator("body").First().InnerText(); err == nil {
		if strings.Contains(text, "No image at the URL") || strings.Contains(text, "Something went wrong") {
			if local {
				return nil, fmt.Errorf("Lens could not read %s; it may be corrupt or in a format Lens does not accept", file)
			}
			return nil, fmt.Errorf("Lens could not fetch %s. The URL has to be publicly reachable "+
				"and point straight at the image, not at a page containing it", image)
		}
	}

	result := &LensResult{Image: image}
	if err := page.EvaluateInto(result, lensJS); err != nil {
		return nil, fmt.Errorf("reading the Lens results: %w", err)
	}
	result.Image = image
	if result.Description == "" && len(result.Matches) == 0 {
		result.Notes = append(result.Notes, "Lens returned nothing it could identify. "+
			"A clearer picture, or one with the subject filling more of the frame, usually helps.")
	}
	return result, nil
}

// uploadToLens opens Google Images and puts a local file through its search-by-
// image control.
//
// The file input is set directly rather than by clicking the camera button and
// catching the file chooser. Setting the input works whether or not the control
// is visible, and the alternative depends on the button's label, which is
// localised and moves.
func (p *Page) uploadToLens(file string) error {
	if err := p.Open("https://images.google.com/?hl=en"); err != nil {
		return err
	}
	// The input exists only once the search-by-image panel has been opened, so
	// the camera button is clicked first - and its absence is not fatal, since
	// some layouts render the input up front.
	button := p.Locator("[aria-label='Search by image'], .nDcEnd, .Gdd5U")
	if count, err := button.Count(); err == nil && count > 0 {
		if err := button.First().Click(playwright.LocatorClickOptions{
			Timeout: playwright.Float(5000),
		}); err == nil {
			p.Settle(1200)
		}
	}

	input := p.Locator("input[type='file']")
	count, err := input.Count()
	if err != nil || count == 0 {
		return fmt.Errorf("could not find the image upload control on Google Images; " +
			"a public image URL works instead and does not depend on that page's layout")
	}
	if err := input.First().SetInputFiles([]string{file}); err != nil {
		return fmt.Errorf("uploading %s: %w", file, err)
	}
	if err := p.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
		State:   playwright.LoadStateDomcontentloaded,
		Timeout: playwright.Float(30000),
	}); err != nil {
		return fmt.Errorf("waiting for the Lens results after upload: %w", err)
	}
	p.DismissConsent()
	return nil
}

// lensJS reads the Lens results page: the description Google writes at the top,
// then the match cards.
const lensJS = `() => {
  const out = { description: '', matches: [] };

  // The description sits under an "AI Overview"-style heading with no stable
  // container, so it is read out of the page text between that heading and
  // whichever section header comes next.
  const text = document.body.innerText || '';
  const start = text.search(/\b(AI Overview|About this image)\b/);
  if (start !== -1) {
    const after = text.slice(start).replace(/^(AI Overview|About this image)\s*/, '');
    const stops = ['Visual matches', 'Exact matches', 'Products', 'Related links', 'Search results'];
    let end = after.length;
    for (const stop of stops) {
      const at = after.indexOf(stop);
      if (at > 0 && at < end) end = at;
    }
    out.description = after.slice(0, Math.min(end, 1200)).trim();
    const dive = out.description.indexOf('Dive deeper');
    if (dive > 0) out.description = out.description.slice(0, dive).trim();
  }

  const seen = new Set();
  const skip = new Set(['Search Results', 'Filters and topics', 'Customised date range']);
  for (const heading of document.querySelectorAll('div[role="heading"], h3')) {
    if (out.matches.length >= 12) break;
    const title = (heading.innerText || '').trim();
    if (!title || title.length < 3 || skip.has(title) || seen.has(title)) continue;

    const link = heading.closest('a[href]');
    const href = link ? link.href : '';
    if (!href || href.includes('google.com/search')) continue;
    seen.add(title);

    // The source is the line above the title inside the same link, and a
    // price if there is one is in the surrounding block.
    let source = '';
    if (link) {
      const lines = (link.innerText || '').trim().split('\n').map(s => s.trim()).filter(Boolean);
      if (lines.length > 1 && lines[0] !== title) source = lines[0];
    }
    let price = '';
    const box = heading.closest('div');
    if (box) {
      const m = (box.innerText || '').match(/(?:US?\$|€|£|CHF|¥)\s?[\d,.]+/);
      if (m) price = m[0];
    }
    out.matches.push({ title: title, url: href, source: source, price: price });
  }
  return out;
}`
