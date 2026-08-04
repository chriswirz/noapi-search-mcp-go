// noapi-search-mcp is a Model Context Protocol server that gives a model real
// search results and real web pages, with no API keys: it drives a headless
// browser and reads the search engines' own HTML endpoints.
//
// The same capabilities are served twice - as MCP tools, and as an OpenAPI
// described HTTP API with Swagger UI compiled into the binary - from one set of
// operation definitions, so the two cannot describe different behaviour.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	appName    = "noapi-search-mcp"
	defaultURL = "http://127.0.0.1:8780/mcp"
)

const usage = `noapi-search-mcp - real search results and web pages for a model, with no API keys

It drives a headless browser and reads search engines' own HTML endpoints, so
there is no key to obtain, no quota and no per-call cost. Google, DuckDuckGo,
Bing, Brave, Startpage and Mojeek for general search; Google's verticals for
news, scholar, books, shopping and images; geocoding, places and directions;
the weather, finance, translation and travel cards; and a page reader that
renders in a real browser before extracting the text.

Usage:
  noapi-search-mcp [flags]

Two surfaces, one definition
  Over MCP, as tools, which is what --transport stdio serves and what an editor
  or a desktop client will use.

  Over HTTP, as an OpenAPI 3.1 API with Swagger UI at ` + "`--docs-path`" + `, for
  everything that does not speak MCP. Both are generated from the same
  operations, so they cannot drift.

It speaks MCP revision ` + ProtocolVersion + `, which is stateless: there is no
initialize handshake and no session id. Older clients are served too - one that
opens with an initialize handshake gets the revision it negotiates. Pass
--no-legacy to serve only the current revision.

Flags:
  -c, --config <path>      configuration file to read (default: config.json in
                           the working directory; a missing default file is not
                           an error - the server runs on its defaults)
      --engine <name>      default search engine for web_search: google
                           (default), duckduckgo, bing, brave, startpage or
                           mojeek. duckduckgo, bing and mojeek need no browser
      --cdp <url>          attach to a Chrome you started yourself with
                           --remote-debugging-port, e.g. http://127.0.0.1:9222,
                           instead of launching one. This is the best answer to
                           being blocked: a browser you have actually been using
                           is not what the bot detection is looking for, and the
                           engines that refuse a headless one generally serve it.
                           It drives a browser holding your real sessions, in
                           tabs beside your own - this server opens and closes
                           only its own tabs, but every page it visits is
                           visited as you
      --headed             show the browser window. It is headless by default;
                           this is for watching a scraper meet a consent banner
                           or a CAPTCHA rather than reading about it
      --channel <name>     browser to launch: chromium (default, Playwright's
                           bundled build), chrome, chrome-beta or msedge
      --profile <dir>      persistent user-data directory (default: ./profile).
                           Keeping it matters: a browser with accumulated
                           cookies is a returning visitor rather than a fresh
                           one, which is most of what avoids a CAPTCHA
      --max-pages <n>      how many scrapes may run at once (default: 4)
      --idle-timeout <s>   close the browser after this many idle seconds
                           (default: 300; 0 takes the default)
      --max-page-chars <n> cap on what visit_page returns (default: 8000, or
                           the MAX_PAGE_CHARS environment variable)
      --no-stealth         do not patch the automation signals before each page
                           load. Expect more CAPTCHAs
      --warm               start the browser at boot rather than on the first
                           call that needs it
      --transport <name>   "stdio" (default) to speak JSON-RPC on stdin/stdout
                           for a client that launches this server, or "http"
  -u, --url <url>          full URL clients connect to over http, and what the
                           server binds to (default: ` + defaultURL + `)
      --token <token>      require this bearer token on the MCP endpoint and
                           every REST operation. The docs and the spec stay
                           open - they describe the API but cannot use it, and
                           a browser cannot put a header on a pasted URL. Enter
                           the token through Authorize in Swagger UI
      --token-env <name>   read that token from this environment variable
                           instead, so it stays out of the config file
      --no-api             do not serve the HTTP API, the spec or Swagger UI;
                           serve the MCP endpoint alone
      --api-path <path>    subtree the REST resources live under
                           (default: /api/v1)
      --docs-path <path>   where Swagger UI is served (default: /docs)
      --spec-path <path>   where the OpenAPI document is served
                           (default: /openapi.json)
      --public-url <url>   the origin written into the spec, for when the API
                           is reached through the tunnel or a reverse proxy
      --tunnel <url>       expose this server through an https-tunnel server at
                           this URL, e.g. https://tunnel.example.com. The
                           handler is served in process by the tunnel client,
                           so this works without opening a port. Set a --token
                           as well: the tunnel URL is public
      --tunnel-key <key>   API key for that tunnel server (default: the
                           TUNNEL_API_KEY environment variable)
      --tunnel-subdomain <label>
                           subdomain to ask for. It is granted when free and a
                           random one is issued otherwise, so read the URL the
                           server prints when the tunnel comes up
      --tunnel-session-file <path>
                           keep the session id in this file instead of in
                           config.json, so a restart reclaims the same URL
      --tunnel-only        serve the tunnel alone, binding no local port
      --allow-origin <o>   comma-separated origins a browser may call the MCP
                           endpoint from. "*" allows any
      --tls-cert <path>    TLS certificate to serve with, and
      --tls-key <path>     its private key
      --tls-self-signed    generate a self-signed certificate on startup
      --no-legacy          serve only protocol version ` + ProtocolVersion + `
      --check              load the config, report what would be served, and
                           exit without listening or starting a browser
      --selftest [names]   run the compatibility checks against the live
                           services and exit. Every tool here depends on
                           somebody else's page not changing, and when one does
                           the symptom is an empty result rather than an error -
                           this is what tells the two apart. It reports each
                           service as ok, blocked (rate-limited, not a fault) or
                           BROKEN (the extraction needs fixing), and exits
                           non-zero only for the last. Takes an optional
                           comma-separated filter matched against the check
                           names, e.g. --selftest search or --selftest google
      --list-checks        print the names --selftest accepts and exit
  -v, --version            print the version and exit
  -h, --help               show this help and exit

Examples:
  noapi-search-mcp
  noapi-search-mcp --engine duckduckgo
  noapi-search-mcp --cdp http://127.0.0.1:9222
  noapi-search-mcp --transport http --url http://127.0.0.1:8780/mcp
  noapi-search-mcp --transport http --token "$MCP_TOKEN"     # then open /docs
  noapi-search-mcp --tunnel https://tunnel.example.com --token "$MCP_TOKEN"
  noapi-search-mcp --headed --engine google --check

First run:
  The browser engines need Playwright's Chromium, once:
    go run github.com/playwright-community/playwright-go/cmd/playwright@latest install --with-deps chromium
  Or set --channel chrome to drive a Chrome you already have. The duckduckgo,
  bing and mojeek engines need neither and work immediately.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", appName, err)
		os.Exit(1)
	}
}

type options struct {
	configPath    string
	engine        string
	cdp           string
	headed        bool
	channel       string
	profile       string
	maxPages      int
	idleTimeout   int
	maxPageChars  int
	noStealth     bool
	warm          bool
	transport     string
	url           string
	token         string
	tokenEnv      string
	noAPI         bool
	apiPath       string
	docsPath      string
	specPath      string
	publicURL     string
	tunnel        string
	tunnelKey     string
	tunnelSub     string
	tunnelSession string
	tunnelOnly    bool
	allowOrigin   string
	tlsCert       string
	tlsKey        string
	tlsSelfSigned bool
	noLegacy      bool
	check         bool
	selftest      string
	runSelftest   bool
	listChecks    bool
	showVersion   bool
	showHelp      bool
}

func parseFlags(args []string) (*options, error) {
	var opts options
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	str := func(target *string, names ...string) {
		for _, name := range names {
			fs.StringVar(target, name, "", "")
		}
	}
	boolean := func(target *bool, names ...string) {
		for _, name := range names {
			fs.BoolVar(target, name, false, "")
		}
	}
	integer := func(target *int, names ...string) {
		for _, name := range names {
			fs.IntVar(target, name, 0, "")
		}
	}

	str(&opts.configPath, "config", "c")
	str(&opts.engine, "engine")
	str(&opts.cdp, "cdp")
	boolean(&opts.headed, "headed")
	str(&opts.channel, "channel")
	str(&opts.profile, "profile")
	integer(&opts.maxPages, "max-pages")
	integer(&opts.idleTimeout, "idle-timeout")
	integer(&opts.maxPageChars, "max-page-chars")
	boolean(&opts.noStealth, "no-stealth")
	boolean(&opts.warm, "warm")
	str(&opts.transport, "transport")
	str(&opts.url, "url", "u")
	str(&opts.token, "token")
	str(&opts.tokenEnv, "token-env")
	boolean(&opts.noAPI, "no-api")
	str(&opts.apiPath, "api-path")
	str(&opts.docsPath, "docs-path")
	str(&opts.specPath, "spec-path")
	str(&opts.publicURL, "public-url")
	str(&opts.tunnel, "tunnel")
	str(&opts.tunnelKey, "tunnel-key")
	str(&opts.tunnelSub, "tunnel-subdomain")
	str(&opts.tunnelSession, "tunnel-session-file")
	boolean(&opts.tunnelOnly, "tunnel-only")
	str(&opts.allowOrigin, "allow-origin")
	str(&opts.tlsCert, "tls-cert")
	str(&opts.tlsKey, "tls-key")
	boolean(&opts.tlsSelfSigned, "tls-self-signed")
	boolean(&opts.noLegacy, "no-legacy")
	boolean(&opts.check, "check")
	// --selftest takes an optional filter, so it is a string flag that also
	// works bare. Go's flag package cannot express that, so the bare form is
	// picked out of the arguments first and removed before parsing.
	str(&opts.selftest, "selftest")
	boolean(&opts.listChecks, "list-checks")
	boolean(&opts.showVersion, "version", "v")
	boolean(&opts.showHelp, "help", "h")

	args, bareSelftest := extractBareSelftest(args)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			opts.showHelp = true
			return &opts, nil
		}
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q; %s takes flags only", fs.Arg(0), appName)
	}
	opts.runSelftest = bareSelftest || opts.selftest != ""
	return &opts, nil
}

// extractBareSelftest handles `--selftest` with no value.
//
// The flag is useful both ways - bare to run everything, and with a filter to
// run one service - and Go's flag package supports only one of those for a
// given flag. Rather than make people write `--selftest all`, the bare form is
// recognised here and taken out of the arguments before parsing; a `--selftest
// search` is left alone for the parser to pick up as a value.
func extractBareSelftest(args []string) ([]string, bool) {
	for i, arg := range args {
		if arg != "--selftest" && arg != "-selftest" {
			continue
		}
		// A following argument that looks like another flag, or nothing at
		// all, means this was the bare form.
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			return args, false
		}
		return append(append([]string{}, args[:i]...), args[i+1:]...), true
	}
	return args, false
}

func run(args []string) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}
	switch {
	case opts.showHelp:
		fmt.Print(usage)
		return nil
	case opts.showVersion:
		fmt.Printf("%s %s (MCP %s)\n", appName, version, ProtocolVersion)
		return nil
	case opts.listChecks:
		fmt.Printf("Compatibility checks --selftest can run:\n\n")
		for _, name := range probeNames() {
			fmt.Printf("  %s\n", name)
		}
		fmt.Printf("\nA filter is matched as a substring, so --selftest search runs every engine\n" +
			"and --selftest google runs the Google ones.\n")
		return nil
	}

	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("could not determine the working directory: %w", err)
	}
	configPath := opts.configPath
	explicit := configPath != ""
	if !explicit {
		configPath = joinPath(dir, "config.json")
	}
	cfg, err := LoadConfig(configPath, explicit)
	if err != nil {
		return err
	}
	applyOptions(&cfg, opts)
	if err := cfg.Normalize(dir); err != nil {
		return err
	}

	// On stdio, stdout carries the protocol: everything human-readable has to
	// go to stderr or it corrupts the stream.
	banner := os.Stdout
	if cfg.Server.Transport == "stdio" {
		banner = os.Stderr
	}
	logger := log.New(os.Stderr, appName+": ", log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := NewServer(cfg.Server.Name, cfg.Server.Version, cfg.Server.Instructions, cfg.Server.LegacyCompatibility)
	srv.cfg = cfg
	srv.browser = NewBrowser(cfg.Browser, cfg.Search, logger)
	srv.scraper = NewScraper(srv.browser, cfg.Search)
	srv.downloads = NewDownloadStore(cfg.Downloads)
	defer srv.browser.Close()
	srv.registerOperations()

	// Both of these are built before the banner, so that --check reports a bad
	// certificate or a spec that will not render, rather than a server that
	// only fails once somebody connects to it.
	var tlsConfig *tls.Config
	if cfg.Server.Transport == "http" {
		if tlsConfig, err = cfg.TLSConfig(); err != nil {
			return err
		}
	}
	var api *RESTAPI
	if cfg.APIEnabled() {
		if api, err = NewRESTAPI(srv, &srv.cfg, logger); err != nil {
			return err
		}
	}

	printBanner(banner, &cfg, srv)
	if opts.check {
		fmt.Fprintln(banner, "\nConfiguration is valid. Exiting because --check was given.")
		return nil
	}
	if opts.runSelftest {
		return runSelfTestCommand(ctx, srv, opts.selftest)
	}

	// A browser that will not start is reported and then left alone: the
	// browser-free engines still work, the browser retries on the next call
	// that needs it, and refusing to serve would take the MCP client down too.
	if opts.warm {
		if err := srv.browser.Start(); err != nil {
			logger.Printf("the browser did not start: %v", err)
			logger.Printf("serving anyway; the browser-free engines (%s) work regardless",
				strings.Join(browserFreeEngines(), ", "))
		}
	}

	if cfg.Server.Transport == "stdio" {
		return NewStdioTransport(srv, os.Stdin, os.Stdout, cfg.Server.LegacyCompatibility, logger).Serve(ctx)
	}
	listeners, err := cfg.Listeners()
	if err != nil {
		return err
	}
	transport := NewHTTPTransport(srv, cfg.Server, listeners[0].Paths[0], logger)
	transport.rest = api
	if !cfg.Tunnel.Enabled {
		return transport.ServeAll(ctx, listeners, tlsConfig)
	}

	// The tunnel serves the same handler in process, on every path the local
	// listeners answer on, so a client reaches the same endpoints either way.
	tunnel, err := NewTunnel(&cfg, transport.HandlerFor(endpointPaths(listeners)...), logger)
	if err != nil {
		return err
	}
	if cfg.Tunnel.Only {
		return tunnel.Run(ctx)
	}

	// Either half failing takes the other down: a server that is half up is
	// worse than one that refuses to start, because a client connected to the
	// surviving half has no way to know.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- transport.ServeAll(ctx, listeners, tlsConfig) }()
	go func() { errs <- tunnel.Run(ctx) }()
	var first error
	for range 2 {
		if err := <-errs; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

// runSelfTestCommand runs the compatibility checks and reports them, exiting
// non-zero only when something is genuinely broken.
//
// The exit code is the whole contract of this mode, because that is what a
// monitor or a CI job reads. Being rate-limited is not a failure: it says
// nothing about whether this server's code is correct, and a check that goes
// red for it would be silenced within a week and then be worth nothing when a
// selector actually breaks. Only "the service answered and we could not read
// it" is a failure.
func runSelfTestCommand(ctx context.Context, srv *Server, filter string) error {
	fmt.Fprintf(os.Stderr, "\nRunning the compatibility checks. These make real requests to real "+
		"services and take a minute or two.\n\n")

	report := RunSelfTest(ctx, srv.scraper, splitList(filter))
	fmt.Println(report.Render())

	if len(report.Results) == 0 {
		return fmt.Errorf("no check matched %q; --list-checks shows the names", filter)
	}
	if !report.Healthy {
		return fmt.Errorf("%d compatibility check(s) failed: a service answered normally and this "+
			"server could not read the result, which means the extraction needs updating", report.Broken)
	}
	return nil
}

// browserFreeEngines is the engines that work with no browser installed.
func browserFreeEngines() []string {
	var out []string
	for _, e := range AllEngines() {
		if !e.NeedsBrowser() {
			out = append(out, e.Name())
		}
	}
	return out
}

// endpointPaths is every distinct path the configured listeners answer on.
func endpointPaths(listeners []Listener) []string {
	var paths []string
	seen := map[string]bool{}
	for _, l := range listeners {
		for _, path := range l.Paths {
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// applyOptions lets the command line override the configuration file.
func applyOptions(cfg *Config, opts *options) {
	if opts.engine != "" {
		cfg.Search.Engine = opts.engine
	}
	if opts.cdp != "" {
		cfg.Browser.CDPURL = opts.cdp
	}
	if opts.headed {
		cfg.Browser.Headless = boolPtr(false)
	}
	if opts.channel != "" {
		cfg.Browser.Channel = opts.channel
	}
	if opts.profile != "" {
		cfg.Browser.ProfileDir = opts.profile
	}
	if opts.maxPages > 0 {
		cfg.Browser.MaxPages = opts.maxPages
	}
	if opts.idleTimeout > 0 {
		cfg.Browser.IdleTimeoutSeconds = opts.idleTimeout
	}
	if opts.maxPageChars > 0 {
		cfg.Search.MaxPageChars = opts.maxPageChars
	}
	if opts.noStealth {
		cfg.Browser.Stealth = boolPtr(false)
	}
	if opts.transport != "" {
		cfg.Server.Transport = opts.transport
	}
	if opts.tunnel != "" {
		cfg.Tunnel.Enabled = true
		cfg.Tunnel.ServerURL = opts.tunnel
	}
	if opts.tunnelKey != "" {
		cfg.Tunnel.APIKey = opts.tunnelKey
	}
	if opts.tunnelSub != "" {
		cfg.Tunnel.Subdomain = opts.tunnelSub
	}
	if opts.tunnelSession != "" {
		cfg.Tunnel.SessionFile = opts.tunnelSession
	}
	if opts.tunnelOnly {
		cfg.Tunnel.Enabled = true
		cfg.Tunnel.Only = true
	}
	// The tunnel serves the HTTP handler, so asking for one is asking for the
	// HTTP transport. Making the operator say both would only ever produce a
	// confusing validation error.
	if cfg.Tunnel.Enabled && opts.transport == "" && cfg.Server.Transport == "stdio" {
		cfg.Server.Transport = "http"
	}
	if opts.url != "" {
		cfg.Server.URL = opts.url
		cfg.Server.AdditionalURLs = nil
		// Naming a URL is how the HTTP transport is asked for; requiring
		// --transport http as well would be a papercut and nothing more.
		if opts.transport == "" {
			cfg.Server.Transport = "http"
		}
	}
	if opts.token != "" {
		cfg.Server.AuthToken = opts.token
	}
	if opts.tokenEnv != "" {
		cfg.Server.AuthTokenEnv = opts.tokenEnv
	}
	if opts.noAPI {
		cfg.API.Enabled = boolPtr(false)
	}
	if opts.apiPath != "" {
		cfg.API.BasePath = opts.apiPath
	}
	if opts.docsPath != "" {
		cfg.API.DocsPath = opts.docsPath
	}
	if opts.specPath != "" {
		cfg.API.SpecPath = opts.specPath
	}
	if opts.publicURL != "" {
		cfg.API.PublicURL = opts.publicURL
	}
	if opts.allowOrigin != "" {
		cfg.Server.AllowedOrigins = splitList(opts.allowOrigin)
	}
	if opts.tlsCert != "" {
		cfg.Server.TLSCertFile = opts.tlsCert
	}
	if opts.tlsKey != "" {
		cfg.Server.TLSKeyFile = opts.tlsKey
	}
	if opts.tlsSelfSigned {
		cfg.Server.TLSSelfSigned = true
	}
	// Asking for TLS without saying so in the URL is a contradiction the
	// operator almost certainly did not mean; upgrade the scheme instead of
	// silently serving plaintext.
	if (cfg.Server.TLSSelfSigned || cfg.Server.TLSCertFile != "") && !cfg.Server.IsTLS() {
		cfg.Server.URL = strings.Replace(cfg.Server.URL, "http://", "https://", 1)
	}
	if opts.noLegacy {
		cfg.Server.LegacyCompatibility = false
	}
}

// printBanner is the startup summary: what is served, where, and - the thing an
// operator actually needs - the URLs to open.
func printBanner(w *os.File, cfg *Config, srv *Server) {
	fmt.Fprintf(w, "%s %s  (MCP protocol %s)\n", appName, version, ProtocolVersion)
	if cfg.Server.LegacyCompatibility {
		fmt.Fprintf(w, "  versions   %s\n", strings.Join(SupportedVersions, ", "))
	} else {
		fmt.Fprintf(w, "  versions   %s only (--no-legacy)\n", ProtocolVersion)
	}

	b := cfg.Browser
	if b.CDPURL != "" {
		fmt.Fprintf(w, "  browser    attaching to Chrome at %s\n", b.CDPURL)
		fmt.Fprintf(w, "             it must be started with --remote-debugging-port and its own\n")
		fmt.Fprintf(w, "             --user-data-dir, or the connection is refused\n")
		fmt.Fprintf(w, "             that browser is yours: this server opens and closes only its own\n")
		fmt.Fprintf(w, "             tabs, patches no fingerprints, and never writes your cookie jar\n")
		fmt.Fprintf(w, "             up to %d scrapes at once\n", b.MaxPages)
	} else {
		mode := "headless"
		if !b.IsHeadless() {
			mode = "headed"
		}
		channel := b.Channel
		if channel == "" {
			channel = "chromium (bundled)"
		}
		fmt.Fprintf(w, "  browser    %s, %s; up to %d at once, closing after %ds idle\n",
			channel, mode, b.MaxPages, b.IdleTimeoutSeconds)
		fmt.Fprintf(w, "  profile    %s\n", b.ProfileDir)
		if !b.StealthEnabled() {
			fmt.Fprintf(w, "  stealth    disabled; expect more CAPTCHAs\n")
		}
	}
	fmt.Fprintf(w, "  engine     %s by default (%s need no browser)\n",
		cfg.Search.Engine, strings.Join(browserFreeEngines(), ", "))
	fmt.Fprintf(w, "  page text  %d characters maximum\n", cfg.Search.MaxPageChars)

	fmt.Fprintf(w, "  transport  %s\n", cfg.Server.Transport)
	if cfg.Server.Transport == "stdio" {
		fmt.Fprintf(w, "  endpoint   stdin/stdout (JSON-RPC, newline delimited)\n")
	} else if endpoints, err := cfg.Endpoints(); err == nil {
		var addrs []string
		for _, e := range endpoints {
			addrs = append(addrs, e.Addr)
		}
		fmt.Fprintf(w, "  listening  %s\n", strings.Join(addrs, ", "))
		fmt.Fprintf(w, "\n  MCP endpoint:  %s\n", endpoints[0].URL)
		base := cfg.BaseURL()
		if cfg.APIEnabled() {
			fmt.Fprintf(w, "  REST API:      %s%s\n", base, cfg.API.BasePath)
			fmt.Fprintf(w, "  OpenAPI:       %s%s\n", base, cfg.API.SpecPath)
			fmt.Fprintf(w, "  Swagger UI:    %s%s\n", base, cfg.API.DocsPath)
		}
		if cfg.DownloadsServed() {
			access := "token required"
			if cfg.Downloads.Public || cfg.Server.AuthTokenValue() == "" {
				access = "open, but the paths are unguessable"
			}
			fmt.Fprintf(w, "  Downloads:     %s%s/<id>  (%s)\n", base, cfg.API.DownloadsPath, access)
		}
		fmt.Fprintln(w)
		if cfg.Server.AuthTokenValue() != "" {
			fmt.Fprintf(w, "  auth       required: Authorization: Bearer <token>\n")
		} else {
			fmt.Fprintf(w, "  auth       none; set --token before exposing this beyond this machine\n")
		}
		origins := strings.Join(cfg.Server.AllowedOrigins, ", ")
		if origins == "" {
			origins = "none (browser clients will be refused on the MCP endpoint)"
		}
		fmt.Fprintf(w, "  origins    %s\n", origins)
	}
	if cfg.Tunnel.Enabled {
		fmt.Fprintf(w, "  tunnel     through %s", cfg.Tunnel.ServerURL)
		if cfg.Tunnel.Subdomain != "" {
			fmt.Fprintf(w, ", asking for %q", cfg.Tunnel.Subdomain)
		}
		fmt.Fprintln(w)
		if cfg.Server.AuthTokenValue() == "" {
			fmt.Fprintf(w, "             the tunnel URL is public and no token is set - anyone who "+
				"finds it can drive this browser\n")
		}
	}

	names := srv.ToolNames()
	sort.Strings(names)
	fmt.Fprintf(w, "  tools (%d) %s\n", len(names), wrapList(names, 58, "             "))
	fmt.Fprintf(w, "\n  The browser starts on the first call that needs it, not now.\n")
}

// wrapList renders a long list of names over several indented lines.
func wrapList(items []string, width int, indent string) string {
	var lines []string
	var current string
	for _, item := range items {
		switch {
		case current == "":
			current = item
		case len(current)+len(item)+2 <= width:
			current += ", " + item
		default:
			lines = append(lines, current)
			current = item
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return strings.Join(lines, "\n"+indent)
}

// splitList parses a comma-separated flag value, dropping empty entries.
func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func joinPath(dir, name string) string {
	if strings.HasSuffix(dir, string(os.PathSeparator)) {
		return dir + name
	}
	return dir + string(os.PathSeparator) + name
}

// sortedKeys returns a map's keys in order, so rendered output is stable.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
