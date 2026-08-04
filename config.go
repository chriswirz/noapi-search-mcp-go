package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Config is the whole of config.json.
type Config struct {
	Server  ServerConfig  `json:"server"`
	Tunnel  TunnelConfig  `json:"tunnel"`
	Browser BrowserConfig `json:"browser"`
	Search  SearchConfig  `json:"search"`
	API     APIConfig     `json:"api"`

	Downloads DownloadConfig `json:"downloads"`

	// path is the file this was loaded from, so a session id the tunnel
	// server issues can be written back to it. It is not part of the file.
	path string
}

// Path returns the file the configuration was loaded from.
func (c *Config) Path() string { return c.path }

// ErrNoConfigFile reports that there is nowhere to persist a session id: the
// server is running on its defaults and flags, with no config.json to write
// back to. It is not a failure of the tunnel, only of remembering the URL.
var ErrNoConfigFile = errors.New("no config file to write to")

// configWriteMu serialises the read-modify-write of the config file. It is a
// package variable rather than a field so that Config stays copyable - the
// server keeps its own copy, and a mutex inside would make that a vet error.
var configWriteMu sync.Mutex

// SaveSessionID writes a session id the tunnel server issued back into the
// tunnel section of config.json, so restarting the server reclaims the same
// public URL instead of being handed a new one.
//
// Every other key is left exactly as it was found: the file is decoded into
// raw messages, one field is replaced, and the result is written through a
// temporary file so an interrupted write cannot truncate the configuration.
func (c *Config) SaveSessionID(id string) error {
	if id == "" || id == c.Tunnel.SessionID {
		return nil
	}
	c.Tunnel.SessionID = id
	if c.path == "" {
		return ErrNoConfigFile
	}
	if _, err := os.Stat(c.path); err != nil {
		if os.IsNotExist(err) {
			return ErrNoConfigFile
		}
		return err
	}
	return c.updateSection("tunnel", "session_id", func(section map[string]json.RawMessage) error {
		encoded, err := json.Marshal(id)
		if err != nil {
			return err
		}
		section["session_id"] = encoded
		return nil
	})
}

// updateSection rewrites one top-level section of the config file, leaving
// every other key as it was found.
func (c *Config) updateSection(name, field string, mutate func(map[string]json.RawMessage) error) error {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	raw, err := os.ReadFile(c.path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}), &doc); err != nil {
		return fmt.Errorf("%s: %w", c.path, err)
	}
	section := map[string]json.RawMessage{}
	if b, ok := doc[name]; ok {
		if err := json.Unmarshal(b, &section); err != nil {
			return fmt.Errorf("%s: %s: %w", c.path, name, err)
		}
	}
	if err := mutate(section); err != nil {
		return fmt.Errorf("updating %s.%s: %w", name, field, err)
	}
	if doc[name], err = json.Marshal(section); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// Written to a temporary file and renamed, so a crash mid-write leaves the
	// old configuration intact rather than half a file. 0600 because the
	// config can carry an auth token and a tunnel key.
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// TunnelConfig exposes this server on a public HTTPS URL through an
// https-tunnel server, without opening a port on this machine. The handler is
// served in process by the tunnel client, so it works even where binding a
// local port is awkward, and it is what makes these tools reachable from a
// hosted client that cannot see this network.
//
// Think about what that means before enabling it. The tunnel is public, and
// these tools drive a real browser against a real profile: set an auth token,
// which is what server.auth_token is for.
type TunnelConfig struct {
	Enabled bool `json:"enabled"`

	// ServerURL is the tunnel control plane, e.g. https://tunnel.example.com.
	ServerURL string `json:"server_url"`

	// APIKey authenticates against that server. Leave it empty to read
	// APIKeyEnv instead, which keeps the key out of the config file.
	APIKey    string `json:"api_key"`
	APIKeyEnv string `json:"api_key_env"`

	// Subdomain asks for a particular label. The server grants it when free
	// and issues a random one otherwise, so read the URL the client reports
	// rather than assuming this one.
	Subdomain string `json:"subdomain"`

	// SessionID resumes a previous session, keeping its public URL. It is
	// normally left empty and managed through SessionFile.
	SessionID string `json:"session_id"`

	// SessionFile persists the session id the server issues, so a restart
	// reclaims the same URL. Relative paths resolve against the working
	// directory; empty means do not persist.
	SessionFile string `json:"session_file"`

	// Only serves the tunnel alone, with no local listener at all.
	Only bool `json:"only"`

	// AllowAnonymous serves the tunnel with no bearer token. It exists so that
	// running an open endpoint is a decision someone made rather than one they
	// drifted into: without it, a tunnel and an empty auth_token is refused at
	// startup. Setting it means accepting that anyone with the URL can drive
	// this browser.
	AllowAnonymous bool `json:"allow_anonymous"`

	ClientInfo string `json:"client_info"`
}

// APIKeyValue is the key to authenticate with, from the config or the
// environment variable it names.
func (t TunnelConfig) APIKeyValue() string {
	if t.APIKey != "" {
		return t.APIKey
	}
	if name := t.APIKeyEnvName(); name != "" {
		return os.Getenv(name)
	}
	return ""
}

// APIKeyEnvName is the environment variable the key is read from.
func (t TunnelConfig) APIKeyEnvName() string {
	if t.APIKeyEnv != "" {
		return t.APIKeyEnv
	}
	return "TUNNEL_API_KEY"
}

// ServerConfig covers identity and transport. It is the same shape code-mcp
// uses, so a client configured for one server needs nothing new for this one.
type ServerConfig struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Instructions string `json:"instructions"`
	Transport    string `json:"transport"` // "stdio" (default) or "http"
	URL          string `json:"url"`       // full MCP endpoint URL clients connect to

	// AdditionalURLs serves several endpoints at once, typically one http and
	// one https on different ports. When set it replaces URL entirely.
	AdditionalURLs []string `json:"urls,omitempty"`

	AllowedOrigins []string `json:"allowed_origins"`

	// AllowedHeaders is what a CORS preflight is told a browser may send.
	// ["*"] allows any header; empty echoes whatever the preflight asks for.
	AllowedHeaders []string `json:"allowed_headers"`

	// AllowPrivateNetwork answers Chrome's Private Network Access preflight,
	// which a page on a public address must pass before it may reach a server
	// on a private one such as 127.0.0.1. On by default.
	AllowPrivateNetwork bool `json:"allow_private_network"`

	// AuthToken, when set, is required as "Authorization: Bearer <token>" on
	// every HTTP request - the MCP endpoint, the REST resources and the spec
	// alike. Set it whenever the server is reachable by anything but this
	// machine, and always when the tunnel is on: the tunnel URL is public.
	AuthToken string `json:"auth_token"`

	// AuthTokenEnv names an environment variable to read the token from
	// instead, which keeps the credential out of a file that gets committed.
	AuthTokenEnv string `json:"auth_token_env"`

	TLSCertFile   string `json:"tls_cert_file"`
	TLSKeyFile    string `json:"tls_key_file"`
	TLSSelfSigned bool   `json:"tls_self_signed"`

	// LegacyCompatibility serves the initialize-based revisions 2025-03-26
	// through 2025-11-25 alongside the current one. On by default.
	LegacyCompatibility bool `json:"legacy_compatibility"`

	// SessionTimeoutSeconds is how long an idle legacy session is kept.
	SessionTimeoutSeconds int `json:"session_timeout_seconds"`

	// SessionRecovery adopts a session id this process does not know instead
	// of refusing it with 404.
	//
	// Off unless it is asked for, which is the letter of the specification:
	// the server answers 404 and the client is required to start a new session
	// with a fresh initialize. Turn it on for a client that does not do that
	// and leaves its user with a dead conversation instead - a legacy session
	// here holds an id, a version and two timestamps, so adopting the id
	// restores everything it held.
	SessionRecovery bool `json:"session_recovery,omitempty"`
}

// AuthTokenValue is the bearer token to require, from the config or the
// environment variable it names.
func (s ServerConfig) AuthTokenValue() string {
	if s.AuthToken != "" {
		return s.AuthToken
	}
	if s.AuthTokenEnv != "" {
		return os.Getenv(s.AuthTokenEnv)
	}
	return ""
}

// BrowserConfig decides how the headless browser the scrapers drive is
// launched. Everything here has a working default: the point of this server is
// that it needs no configuration and no API key.
type BrowserConfig struct {
	// CDPURL, when set (e.g. "http://127.0.0.1:9222"), attaches to a Chrome
	// you started yourself with --remote-debugging-port instead of launching
	// one. Most of the rest of this section is then ignored: the browser is
	// already running with whatever flags you gave it.
	//
	// This is the most effective answer to being blocked. A launched browser
	// is a machine with no history, however carefully its fingerprint is
	// patched; a Chrome you have been using for months - signed in, with years
	// of cookies - is not what the bot detection is looking for, and the
	// engines that refuse a headless browser generally serve this one.
	//
	// What it costs is isolation. The tools drive a browser holding your real
	// sessions, in tabs beside your own. This server opens and closes only its
	// own tabs and never touches yours, but the browser is shared, and every
	// page it visits is visited as you.
	CDPURL string `json:"cdp_url"`

	// Headless runs with no window, which is the default and almost always
	// right. Turning it off is a debugging aid: you get to watch a scraper
	// meet the consent banner or the CAPTCHA it is reporting. Ignored when
	// attaching, where the window is the one you already have open.
	Headless *bool `json:"headless"`

	// Channel picks a stable browser install ("chrome", "chrome-beta",
	// "msedge") instead of Playwright's bundled Chromium. Empty means the
	// bundled Chromium, which is the reproducible choice for scraping: its
	// version is known, so the user agent this server presents matches the
	// binary actually running rather than contradicting it.
	Channel string `json:"channel"`

	// ExecutablePath overrides Channel with a specific binary.
	ExecutablePath string `json:"executable_path"`

	// ProfileDir is the persistent user-data directory. It is what makes
	// Google see a returning browser rather than a fresh one on every call,
	// which is most of what keeps a scraper out of the CAPTCHA path.
	ProfileDir string `json:"profile_dir"`

	// UserAgent overrides the one derived from the launched binary. Leave it
	// empty: a user agent that disagrees with the browser actually running is
	// a fingerprint mismatch, and that is exactly what bot detection looks for.
	UserAgent string `json:"user_agent"`

	// Locale and TimezoneID shape what the pages return. The locale defaults
	// to US English, because every selector in this server was written against
	// the English layout and a German results page silently matches none.
	Locale     string `json:"locale"`
	TimezoneID string `json:"timezone_id"`

	ViewportWidth  int `json:"viewport_width"`
	ViewportHeight int `json:"viewport_height"`

	// NavigationTimeoutMs bounds a page load, DefaultTimeoutMs every other
	// wait. Both are longer than an app test would want: these pages are heavy
	// and a slow load is not a failure.
	DefaultTimeoutMs    int `json:"default_timeout_ms"`
	NavigationTimeoutMs int `json:"navigation_timeout_ms"`

	// MaxPages is how many pages may be open at once, which is the limit on
	// concurrent scrapes. Each is a tab in one shared browser; the browser is
	// started on the first call that needs it and then kept, because launching
	// one per call is both slow and a fresh unrecognised profile every time.
	MaxPages int `json:"max_pages"`

	// IdleTimeoutSeconds closes the browser after this long with no tool call.
	// A server left running all day in an editor's MCP config should not also
	// leave a Chrome resident all day. Zero means the default; a negative
	// value is refused rather than guessed at.
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`

	// Args are extra command-line flags.
	Args []string `json:"args"`

	// Stealth applies the automation-signal patches before every page load.
	// On by default: it is what the "no API key" premise rests on.
	Stealth *bool `json:"stealth"`
}

// IsHeadless reports whether the browser runs with no window.
func (b BrowserConfig) IsHeadless() bool { return b.Headless == nil || *b.Headless }

// StealthEnabled reports whether the automation-signal patches are applied.
func (b BrowserConfig) StealthEnabled() bool { return b.Stealth == nil || *b.Stealth }

// SearchConfig covers the search behaviour itself rather than the browser.
type SearchConfig struct {
	// Engine is what web_search uses when a call does not name one.
	Engine string `json:"engine"`

	// MaxResults caps num_results on every search tool. A model that asks for
	// a hundred results gets the cap, not a hundred: the results land in a
	// context window, and a search tool that can silently return a novel is a
	// way to lose a conversation to one call.
	MaxResults int `json:"max_results"`

	// MaxPageChars caps what visit_page returns, for the same reason. The
	// MAX_PAGE_CHARS environment variable overrides the default, which is how
	// the Python server this one is modelled on was configured.
	MaxPageChars int `json:"max_page_chars"`

	// ResolveRedirects follows the tracking redirect a search engine wraps
	// each result URL in, so callers get the real destination. Google's is
	// opaque and can only be resolved by following it; DuckDuckGo's and
	// Bing's are decoded locally and cost nothing either way. On by default:
	// a result URL that only works by round-tripping through the engine is a
	// URL a caller cannot store, compare or cite.
	ResolveRedirects *bool `json:"resolve_redirects"`

	// SafeSearch asks the engine to filter explicit results.
	SafeSearch bool `json:"safe_search"`

	// CookieFile persists cookies between runs, which is the other half of
	// looking like a returning browser rather than a fresh one.
	CookieFile string `json:"cookie_file"`
}

// ResolvesRedirects reports whether result URLs are unwrapped.
func (s SearchConfig) ResolvesRedirects() bool {
	return s.ResolveRedirects == nil || *s.ResolveRedirects
}

// DownloadConfig covers the files download_asset fetches and this server then
// hosts. Holding somebody else's files and serving them back is the part of
// this server that most resembles a file host, so the limits here are the ones
// that keep it from becoming one.
type DownloadConfig struct {
	// Dir is where the files live. Relative paths resolve against the working
	// directory.
	Dir string `json:"dir"`

	// MaxFileBytes caps one download, MaxTotalBytes the store. Reaching the
	// total refuses new downloads rather than deleting old ones: these are
	// files somebody asked for by name, and a server that quietly discards
	// them to serve the next request is worse than one that says it is full.
	MaxFileBytes  int64 `json:"max_file_bytes"`
	MaxTotalBytes int64 `json:"max_total_bytes"`

	// AllowPrivateHosts permits downloading from loopback, link-local and
	// private addresses. Off by default, and worth understanding before it is
	// turned on: this server fetches a URL a caller chose, from wherever this
	// server sits, and then serves the result back. On a machine with a
	// private network around it that is a way to read things the caller could
	// not otherwise reach - another service on localhost, or a cloud metadata
	// endpoint handing out credentials. Turn it on to fetch from your own
	// intranet, knowing that is what it means.
	AllowPrivateHosts bool `json:"allow_private_hosts"`

	// Public serves the stored files with no bearer token. The paths are
	// unguessable, so a link can be handed to something that cannot set a
	// header - which is often the point of storing a file at all. It is still
	// a decision: anyone with the URL has the file.
	Public bool `json:"public"`
}

// APIConfig covers the OpenAPI REST facade served alongside the MCP endpoint:
// the same tools reachable by ordinary HTTP, described by a spec, with Swagger
// UI to read and exercise it. It needs the HTTP transport, since there is
// nothing to serve it over on stdio.
type APIConfig struct {
	// Enabled serves the facade. On by default when the transport is http.
	Enabled *bool `json:"enabled"`

	// BasePath is the subtree the resources live under.
	BasePath string `json:"base_path"`

	// DocsPath serves Swagger UI, which is embedded in this binary: no CDN,
	// no network, and it works on a machine that has neither.
	DocsPath string `json:"docs_path"`

	// SpecPath serves the OpenAPI document itself.
	SpecPath string `json:"spec_path"`

	// ProtectDocs requires the bearer token for the documentation page and the
	// spec as well as for the operations. Off by default: neither carries data
	// nor does anything, and protecting them mostly defeats them, since a
	// browser cannot send an Authorization header to a pasted URL. Turn it on
	// when the surface itself is worth not publishing - behind the tunnel, say
	// - and reach the docs with a client that can set headers.
	ProtectDocs *bool `json:"protect_docs"`

	// DownloadsPath is where stored files are served. It defaults to
	// "downloads" under the MCP endpoint's own path, so it rides along with
	// the address a client is already configured for - which is what makes it
	// reachable through a tunnel with nothing else to arrange.
	DownloadsPath string `json:"downloads_path"`

	// PublicURL is the server URL written into the spec. Set it when the
	// facade is reached through the tunnel or a reverse proxy, so the "Try it
	// out" button in Swagger UI aims at the address the browser can reach
	// rather than the one this process happened to bind.
	PublicURL string `json:"public_url"`
}

// DownloadsServed reports whether stored files are reachable over HTTP. On
// stdio they are on disk and nothing more, since there is no server to fetch
// them from.
func (c *Config) DownloadsServed() bool {
	return c.Server.Transport == "http" && c.API.DownloadsPath != ""
}

// DocsProtected reports whether the documentation needs the bearer token too.
func (a APIConfig) DocsProtected() bool { return a.ProtectDocs != nil && *a.ProtectDocs }

// APIEnabled reports whether the REST facade is served.
func (c *Config) APIEnabled() bool {
	if c.Server.Transport != "http" {
		return false
	}
	return c.API.Enabled == nil || *c.API.Enabled
}

// DefaultConfig is the configuration before any file is read.
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Name:                appName,
			Transport:           "stdio",
			URL:                 defaultURL,
			AllowedOrigins:      []string{"http://127.0.0.1", "http://localhost"},
			AllowPrivateNetwork: true,
			LegacyCompatibility: true,
		},
	}
}

// LoadConfig reads config.json. A missing file is not an error unless the path
// was given explicitly: the server is fully usable on its defaults.
func LoadConfig(path string, explicit bool) (Config, error) {
	cfg := DefaultConfig()
	cfg.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return cfg, nil
		}
		return cfg, err
	}
	// PowerShell's redirection writes a UTF-8 BOM, and copying the example
	// config is the documented way to get started, so tolerate one.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	data, err = stripCommentKeys(data)
	if err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	// Unknown keys are refused rather than ignored, so a misspelled setting is
	// reported at startup instead of silently doing nothing - which is the
	// worst way to discover that the option you set was never read.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// stripCommentKeys removes every object key beginning with an underscore,
// recursively.
//
// JSON has no comments, and a configuration file that cannot be annotated is
// one people copy without understanding. Refusing unknown keys and allowing
// underscore-prefixed ones together give both: a note next to a setting is
// kept, and a typo in the setting itself is still caught. The convention costs
// one reserved prefix, and no real setting here starts with an underscore.
func stripCommentKeys(data []byte) ([]byte, error) {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	stripped := stripComments(doc)
	out, err := json.Marshal(stripped)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func stripComments(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key := range typed {
			if strings.HasPrefix(key, "_") {
				delete(typed, key)
				continue
			}
			typed[key] = stripComments(typed[key])
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = stripComments(typed[i])
		}
		return typed
	}
	return value
}

// Normalize fills in derived values and validates the result. dir is the
// directory the server was started in, which relative paths resolve against.
func (c *Config) Normalize(dir string) error {
	if c.Server.Name == "" {
		c.Server.Name = appName
	}
	if c.Server.Version == "" {
		c.Server.Version = version
	}
	if c.Server.Instructions == "" {
		c.Server.Instructions = Instructions
	}
	switch c.Server.Transport {
	case "":
		c.Server.Transport = "stdio"
	case "http", "stdio":
	default:
		return fmt.Errorf("server.transport: want %q or %q, got %q", "stdio", "http", c.Server.Transport)
	}
	if c.Server.URL == "" {
		c.Server.URL = defaultURL
	}
	if c.Server.SessionTimeoutSeconds <= 0 {
		c.Server.SessionTimeoutSeconds = 7200
	}
	if c.Server.Transport == "http" {
		if _, _, err := c.Endpoint(); err != nil {
			return err
		}
	}

	b := &c.Browser
	if b.CDPURL != "" {
		u, err := url.Parse(b.CDPURL)
		if err != nil {
			return fmt.Errorf("browser.cdp_url %q: %w", b.CDPURL, err)
		}
		switch u.Scheme {
		case "http", "https", "ws", "wss":
		default:
			return fmt.Errorf("browser.cdp_url: want an http(s) or ws(s) URL, got %q", b.CDPURL)
		}
		if u.Host == "" {
			return fmt.Errorf("browser.cdp_url %q has no host; it should look like %q",
				b.CDPURL, "http://127.0.0.1:9222")
		}
	}
	if b.ProfileDir == "" {
		b.ProfileDir = "profile"
	}
	b.ProfileDir = resolveDir(dir, b.ProfileDir)
	if b.Locale == "" {
		b.Locale = "en-US"
	}
	if b.ViewportWidth <= 0 {
		b.ViewportWidth = 1280
	}
	if b.ViewportHeight <= 0 {
		b.ViewportHeight = 800
	}
	if b.DefaultTimeoutMs <= 0 {
		b.DefaultTimeoutMs = 20000
	}
	if b.NavigationTimeoutMs <= 0 {
		b.NavigationTimeoutMs = 45000
	}
	if b.MaxPages <= 0 {
		b.MaxPages = 4
	}
	if b.IdleTimeoutSeconds < 0 {
		return fmt.Errorf("browser.idle_timeout_seconds: must not be negative; 0 takes the default")
	}
	if b.IdleTimeoutSeconds == 0 {
		b.IdleTimeoutSeconds = 300
	}

	s := &c.Search
	if s.Engine == "" {
		s.Engine = EngineGoogle
	}
	if !IsEngine(s.Engine) {
		return fmt.Errorf("search.engine %q: want one of %s", s.Engine, strings.Join(EngineNames(), ", "))
	}
	if s.MaxResults <= 0 {
		s.MaxResults = 20
	}
	if s.MaxPageChars <= 0 {
		s.MaxPageChars = envPageChars(8000)
	}
	if s.MaxPageChars > maxPageCharsCeiling {
		s.MaxPageChars = maxPageCharsCeiling
	}
	if s.CookieFile == "" {
		s.CookieFile = filepath.Join(b.ProfileDir, "cookies.json")
	}
	s.CookieFile = resolveDir(dir, s.CookieFile)

	d := &c.Downloads
	if d.Dir == "" {
		d.Dir = "downloads"
	}
	d.Dir = resolveDir(dir, d.Dir)
	if d.MaxFileBytes <= 0 {
		d.MaxFileBytes = 25 << 20 // 25 MB
	}
	if d.MaxTotalBytes <= 0 {
		d.MaxTotalBytes = 500 << 20 // 500 MB
	}
	if d.MaxFileBytes > d.MaxTotalBytes {
		return fmt.Errorf("downloads.max_file_bytes (%d) is larger than downloads.max_total_bytes (%d), "+
			"so no download could ever be stored", d.MaxFileBytes, d.MaxTotalBytes)
	}

	a := &c.API
	if a.BasePath == "" {
		a.BasePath = "/api/v1"
	}
	if a.DocsPath == "" {
		a.DocsPath = "/docs"
	}
	if a.SpecPath == "" {
		a.SpecPath = "/openapi.json"
	}
	for _, p := range []struct {
		name  string
		value *string
	}{{"base_path", &a.BasePath}, {"docs_path", &a.DocsPath}, {"spec_path", &a.SpecPath}} {
		v := *p.value
		if !strings.HasPrefix(v, "/") {
			v = "/" + v
		}
		v = strings.TrimSuffix(v, "/")
		if v == "" {
			return fmt.Errorf("api.%s: %q would take over the whole server", p.name, "/")
		}
		*p.value = v
	}
	// Under the MCP endpoint's own path by default, so it is reachable
	// wherever that is - including through the tunnel, with nothing extra to
	// configure or allowlist.
	if a.DownloadsPath == "" && c.Server.Transport == "http" {
		a.DownloadsPath = strings.TrimSuffix(firstPath(c), "/") + "/downloads"
	}
	if a.DownloadsPath != "" {
		if !strings.HasPrefix(a.DownloadsPath, "/") {
			a.DownloadsPath = "/" + a.DownloadsPath
		}
		a.DownloadsPath = strings.TrimSuffix(a.DownloadsPath, "/")
		if a.DownloadsPath == "" {
			return fmt.Errorf("api.downloads_path: %q would take over the whole server", "/")
		}
	}

	if c.API.Enabled != nil && *c.API.Enabled && c.Server.Transport != "http" {
		return fmt.Errorf("api.enabled: the REST facade is served over HTTP, so it needs server.transport %q, not %q",
			"http", c.Server.Transport)
	}

	if c.Tunnel.Enabled {
		if c.Server.Transport != "http" {
			return fmt.Errorf("tunnel.enabled: the tunnel serves the HTTP handler, so it needs server.transport %q, not %q",
				"http", c.Server.Transport)
		}
		if c.Tunnel.ServerURL == "" {
			return fmt.Errorf("tunnel.server_url: required when the tunnel is enabled, e.g. https://tunnel.example.com")
		}
		u, err := url.Parse(c.Tunnel.ServerURL)
		if err != nil {
			return fmt.Errorf("tunnel.server_url %q: %w", c.Tunnel.ServerURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("tunnel.server_url: want an http(s) URL, got %q", c.Tunnel.ServerURL)
		}
		if c.Tunnel.APIKeyValue() == "" {
			return fmt.Errorf("tunnel.api_key: required when the tunnel is enabled, or set %s in the environment",
				c.Tunnel.APIKeyEnvName())
		}
		// A tunnel with no token is a public URL that drives a browser. Anyone
		// who finds it can search as you, read pages as you, and - in attach
		// mode - do both inside the Chrome holding your signed-in sessions.
		//
		// This is refused rather than warned about. A warning printed at
		// startup is not read: this server is started by an editor, or in the
		// background with its output going to a log nobody opens, and the
		// first sign of trouble would be someone else's traffic. Anyone who
		// genuinely wants an open endpoint can say so explicitly, and then
		// they have said it on purpose.
		if c.Server.AuthTokenValue() == "" && !c.Tunnel.AllowAnonymous {
			mode := "a browser holding whatever sessions its profile has"
			if c.Browser.CDPURL != "" {
				mode = "the Chrome you attached to, with your own signed-in sessions"
			}
			return fmt.Errorf("tunnel.enabled with no server.auth_token: the tunnel URL is public, "+
				"and these tools drive %s. Set server.auth_token (or server.auth_token_env, or "+
				"--token / --token-env), or set tunnel.allow_anonymous true if you really mean to "+
				"serve it open", mode)
		}
		c.Tunnel.SessionFile = resolveDir(dir, c.Tunnel.SessionFile)
	}
	return nil
}

// maxPageCharsCeiling bounds visit_page however it is configured. The ceiling
// is not timidity about big pages - it is that this text lands in a model's
// context window, and a tool that can silently return a megabyte turns one
// careless visit into a blown context. A larger value is clamped rather than
// refused.
const maxPageCharsCeiling = 200000

// envPageChars reads the visit_page character cap from the environment, which
// is how the Python server this one is modelled on was configured and so what
// an existing MCP client config already sets. Both spellings it accepted are
// accepted here. A value that is not a positive number falls back to the
// default: a typo in a config file should not stop the server from starting.
func envPageChars(fallback int) int {
	for _, name := range []string{"MAX_PAGE_CHARS", "max_characters"} {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			continue
		}
		return min(value, maxPageCharsCeiling)
	}
	return fallback
}

func boolPtr(v bool) *bool { return &v }

// resolveDir makes a path absolute against base.
func resolveDir(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

// Endpoint splits the configured URL into a listen address and a path.
func (c *Config) Endpoint() (addr, path string, err error) {
	endpoints, err := c.Endpoints()
	if err != nil {
		return "", "", err
	}
	return endpoints[0].Addr, endpoints[0].Path, nil
}

// Endpoint is one URL this server answers on.
type Endpoint struct {
	URL  string
	Addr string
	Path string
	TLS  bool
}

// URLs is every URL the server is configured to serve, in order.
func (s ServerConfig) URLs() []string {
	if len(s.AdditionalURLs) > 0 {
		return s.AdditionalURLs
	}
	if s.URL == "" {
		return nil
	}
	return []string{s.URL}
}

// Endpoints parses every configured URL.
func (c *Config) Endpoints() ([]Endpoint, error) {
	urls := c.Server.URLs()
	if len(urls) == 0 {
		return nil, fmt.Errorf("server.url: no endpoint URL configured")
	}
	var endpoints []Endpoint
	seen := map[string]string{}

	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("server.url %q: %w", raw, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("server.url: want an http(s) URL, got %q", raw)
		}
		host := u.Hostname()
		if host == "" {
			host = "127.0.0.1"
		}
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		if _, convErr := strconv.Atoi(port); convErr != nil {
			return nil, fmt.Errorf("server.url %q: bad port %q", raw, port)
		}
		path := u.Path
		if path == "" {
			path = "/"
		}
		addr := net.JoinHostPort(host, port)

		// One port carries one protocol. Two URLs sharing an address must
		// agree on the scheme, or neither would work.
		if previous, ok := seen[addr]; ok && previous != u.Scheme {
			return nil, fmt.Errorf("server.urls: %s is configured for both %s and %s; "+
				"give the two schemes different ports", addr, previous, u.Scheme)
		}
		seen[addr] = u.Scheme

		for _, existing := range endpoints {
			if existing.Addr == addr && existing.Path == path {
				return nil, fmt.Errorf("server.urls: %q is listed twice", raw)
			}
		}
		endpoints = append(endpoints, Endpoint{URL: raw, Addr: addr, Path: path, TLS: u.Scheme == "https"})
	}
	return endpoints, nil
}

// Listeners groups the endpoints by listen address, since one address is one
// socket however many paths it serves.
func (c *Config) Listeners() ([]Listener, error) {
	endpoints, err := c.Endpoints()
	if err != nil {
		return nil, err
	}
	var listeners []Listener
	index := map[string]int{}
	for _, e := range endpoints {
		if at, ok := index[e.Addr]; ok {
			listeners[at].Paths = append(listeners[at].Paths, e.Path)
			listeners[at].URLs = append(listeners[at].URLs, e.URL)
			continue
		}
		index[e.Addr] = len(listeners)
		listeners = append(listeners, Listener{
			Addr:  e.Addr,
			Paths: []string{e.Path},
			URLs:  []string{e.URL},
			TLS:   e.TLS,
		})
	}
	return listeners, nil
}

// Listener is one socket and everything it serves.
type Listener struct {
	Addr  string
	Paths []string
	URLs  []string
	TLS   bool
}

// BaseURL is the origin the REST facade advertises in its spec: the configured
// public URL when there is one, and otherwise the first endpoint with its path
// stripped. It is what the "Try it out" button in Swagger UI aims at.
func (c *Config) BaseURL() string {
	if c.API.PublicURL != "" {
		return strings.TrimSuffix(c.API.PublicURL, "/")
	}
	urls := c.Server.URLs()
	if len(urls) == 0 {
		return ""
	}
	u, err := url.Parse(urls[0])
	if err != nil {
		return ""
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return strings.TrimSuffix(u.String(), "/")
}

// urlHost returns the hostname of a URL, or "" if it cannot be parsed. It is
// what the self-signed certificate is issued for, so that a browser opening
// Swagger UI over https recognises the name it was told to connect to.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
