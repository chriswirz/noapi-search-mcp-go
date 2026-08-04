package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/playwright-community/playwright-go"
)

// Browser owns the one headless browser every scraper drives.
//
// The Python server this one is modelled on launches a browser per tool call
// and closes it afterwards. That is simple and it is the wrong trade twice
// over: a launch costs a second or more on every call, and a browser started
// from an empty profile is a first-time visitor to Google every single time,
// which is most of what walks a scraper into a CAPTCHA. Here one browser is
// started on the first call that needs it, kept against a persistent profile so
// the cookies accumulate, and closed again once the server has been idle long
// enough that keeping a Chrome resident is no longer paying for itself.
//
// Pages are per call. A tool takes one, works in it, and gives it back; the
// semaphore bounds how many exist at once, so a burst of concurrent calls
// queues instead of opening thirty tabs.
type Browser struct {
	cfg    BrowserConfig
	search SearchConfig
	logger *log.Logger

	mu  sync.Mutex
	pw  *playwright.Playwright
	ctx playwright.BrowserContext

	// browser is set only when attached over CDP: it is the connection to the
	// operator's Chrome, and closing it drops the connection without closing
	// their browser. attached says which mode is in force, because almost
	// every decision about cleanup and about patching the page depends on
	// whether this browser is ours to alter.
	browser  playwright.Browser
	attached bool

	// lastUse is when a page was last handed back, and idle is the timer that
	// closes the browser that long afterwards.
	lastUse time.Time
	idle    *time.Timer

	// slots bounds concurrent pages. It is created once, at construction, so
	// that acquiring one never has to wait on the browser starting.
	slots chan struct{}
}

// NewBrowser prepares the session without starting anything.
func NewBrowser(cfg BrowserConfig, search SearchConfig, logger *log.Logger) *Browser {
	return &Browser{
		cfg:    cfg,
		search: search,
		logger: logger,
		slots:  make(chan struct{}, cfg.MaxPages),
	}
}

// Running reports whether the browser has been started.
func (b *Browser) Running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctx != nil
}

// Page is one tab checked out for the length of a single tool call. Release
// closes it and returns the slot; every caller must defer it.
type Page struct {
	playwright.Page
	browser *Browser
	once    sync.Once
}

// Release closes the page and hands its slot back.
func (p *Page) Release() {
	p.once.Do(func() {
		if err := p.Page.Close(); err != nil {
			p.browser.logger.Printf("closing a page: %v", err)
		}
		p.browser.release()
	})
}

// Acquire opens a page, starting the browser if this is the first call. It
// blocks while max_pages calls are already in flight, which is the intended
// back pressure: thirty concurrent scrapes would be thirty tabs competing for
// the same rate limit and would get all of them blocked rather than any of
// them answered.
func (b *Browser) Acquire() (*Page, error) {
	b.slots <- struct{}{}
	ctx, err := b.context()
	if err != nil {
		b.release()
		return nil, err
	}
	page, err := ctx.NewPage()
	if err != nil {
		b.release()
		return nil, fmt.Errorf("opening a page: %w", err)
	}
	return &Page{Page: page, browser: b}, nil
}

// release returns a slot and restarts the idle countdown.
func (b *Browser) release() {
	b.mu.Lock()
	b.lastUse = time.Now()
	b.armIdleLocked()
	b.mu.Unlock()
	<-b.slots
}

// context returns the running browser context, starting it if needed.
func (b *Browser) context() (playwright.BrowserContext, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx != nil {
		return b.ctx, nil
	}
	if err := b.startLocked(); err != nil {
		return nil, err
	}
	return b.ctx, nil
}

// Start brings the browser up if it is not already, which is what --warm does.
func (b *Browser) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx != nil {
		return nil
	}
	return b.startLocked()
}

func (b *Browser) startLocked() error {
	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf("starting playwright (run %q once, "+
			"or set browser.channel to a Chrome you already have): %w",
			"go run github.com/playwright-community/playwright-go/cmd/playwright@latest install --with-deps chromium", err)
	}
	b.pw = pw

	if b.cfg.CDPURL != "" {
		if err := b.attachLocked(); err != nil {
			pw.Stop() //nolint:errcheck
			b.pw = nil
			return err
		}
		return nil
	}
	if err := os.MkdirAll(b.cfg.ProfileDir, 0o755); err != nil {
		pw.Stop() //nolint:errcheck
		b.pw = nil
		return fmt.Errorf("creating the profile directory: %w", err)
	}

	opts := playwright.BrowserTypeLaunchPersistentContextOptions{
		Headless: playwright.Bool(b.cfg.IsHeadless()),
		Args:     b.launchArgs(),
		Locale:   playwright.String(b.cfg.Locale),
		Viewport: &playwright.Size{Width: b.cfg.ViewportWidth, Height: b.cfg.ViewportHeight},
	}
	switch {
	case b.cfg.Channel != "" && b.cfg.Channel != "chromium":
		opts.Channel = playwright.String(b.cfg.Channel)
	case b.cfg.IsHeadless():
		// The "chromium" channel is asked for explicitly, rather than left to
		// the default, because that is what selects Chromium's new headless
		// mode. The default headless build is `headless_shell`, a separate
		// binary that is missing enough of a real browser - no extension
		// support, a different user agent, a distinguishable feature set -
		// that Brave and Startpage refuse it outright and Google challenges it
		// far sooner. The new mode is the same binary as a headed Chromium
		// with the window not drawn, and it gets served real results.
		opts.Channel = playwright.String("chromium")
	}
	if b.cfg.ExecutablePath != "" {
		opts.ExecutablePath = playwright.String(b.cfg.ExecutablePath)
		opts.Channel = nil
	}
	if b.cfg.UserAgent != "" {
		opts.UserAgent = playwright.String(b.cfg.UserAgent)
	}
	if b.cfg.TimezoneID != "" {
		opts.TimezoneId = playwright.String(b.cfg.TimezoneID)
	}

	ctx, err := b.pw.Chromium.LaunchPersistentContext(b.cfg.ProfileDir, opts)
	if err != nil && opts.Channel != nil {
		// The named browser may not be installed. Falling back to the bundled
		// Chromium keeps the server usable; say so, because the fingerprint
		// changes with it and so may what the pages return.
		b.logger.Printf("channel %q could not be launched (%v); falling back to the bundled Chromium", b.cfg.Channel, err)
		opts.Channel = nil
		ctx, err = b.pw.Chromium.LaunchPersistentContext(b.cfg.ProfileDir, opts)
	}
	if err != nil {
		b.pw.Stop() //nolint:errcheck
		b.pw = nil
		return fmt.Errorf("launching the browser: %w", err)
	}
	b.ctx = ctx
	ctx.SetDefaultTimeout(float64(b.cfg.DefaultTimeoutMs))
	ctx.SetDefaultNavigationTimeout(float64(b.cfg.NavigationTimeoutMs))

	if b.cfg.StealthEnabled() {
		if err := ctx.AddInitScript(playwright.Script{Content: playwright.String(stealthJS)}); err != nil {
			// Not fatal: the browser works, it just looks more like a robot.
			b.logger.Printf("the stealth patches could not be installed: %v", err)
		}
		b.fixUserAgentLocked()
	}
	b.loadCookiesLocked()
	b.lastUse = time.Now()
	b.armIdleLocked()
	return nil
}

// fixUserAgentLocked removes "Headless" from the browser's user agent.
//
// This is the single loudest automation signal there is, and it is announced
// on every request in a header anyone can read: a headless Chromium introduces
// itself as "HeadlessChrome/148.0.0.0". Brave and Startpage refuse it outright
// and Google challenges it almost immediately.
//
// The fix has to be derived rather than hardcoded. The Python server this one
// is modelled on states a user agent by hand, and it drifts: its string claimed
// a Linux x86_64 Chrome 131 whatever binary was actually running, and a user
// agent that disagrees with the browser's real feature set is a worse signal
// than the honest one it replaced. So the real string is read out of the
// running browser and only the one misleading word is removed - everything
// else, the platform and the version, stays true.
//
// Both surfaces are patched, because they are checked independently: the
// request header, and navigator.userAgent in the page.
//
// A failure here is logged and no more. It costs stealth, not function, and
// refusing to serve because the user agent could not be tidied would be a
// wildly disproportionate response.
func (b *Browser) fixUserAgentLocked() {
	if b.cfg.UserAgent != "" {
		return // the operator named one; overriding it would be rude and surprising
	}
	page, err := b.ctx.NewPage()
	if err != nil {
		b.logger.Printf("could not check the user agent: %v", err)
		return
	}
	defer page.Close() //nolint:errcheck

	value, err := page.Evaluate("() => navigator.userAgent")
	if err != nil {
		b.logger.Printf("could not read the user agent: %v", err)
		return
	}
	agent, _ := value.(string)
	if !strings.Contains(agent, "Headless") {
		return
	}
	fixed := strings.ReplaceAll(agent, "HeadlessChrome", "Chrome")
	fixed = strings.ReplaceAll(fixed, "Headless", "")

	if err := b.ctx.SetExtraHTTPHeaders(map[string]string{"User-Agent": fixed}); err != nil {
		b.logger.Printf("could not set the user agent header: %v", err)
	}
	// The header alone is not enough: a page that reads navigator.userAgent
	// would see the two disagree, which is a sharper signal than either.
	script := fmt.Sprintf(`
Object.defineProperty(navigator, 'userAgent', { get: () => %q });
Object.defineProperty(navigator, 'appVersion', { get: () => %q });`,
		fixed, strings.TrimPrefix(fixed, "Mozilla/"))
	if err := b.ctx.AddInitScript(playwright.Script{Content: playwright.String(script)}); err != nil {
		b.logger.Printf("could not patch navigator.userAgent: %v", err)
	}
}

// attachLocked connects to a Chrome already running with a debugging port.
//
// Almost nothing else in this file applies in this mode, and that is the
// point: the browser is the operator's, started with their flags, holding
// their sessions. It is not ours to reconfigure.
//
// In particular the stealth patches and the user-agent correction are skipped.
// They exist to make a launched headless Chromium look like an ordinary
// browser; this *is* an ordinary browser, and injecting a script into a
// context the operator is also browsing in - one that redefines
// navigator.userAgent for every page they open - would be both unnecessary and
// rude. The cookie jar is left alone for the same reason: it is theirs, and
// this server has no business writing it to a file or seeding it from one.
func (b *Browser) attachLocked() error {
	// Chrome's DevTools endpoint binds to 127.0.0.1 only, and "localhost" can
	// resolve to ::1 first and be refused. Normalising avoids a connection
	// error that looks like the port being closed.
	endpoint := strings.Replace(b.cfg.CDPURL, "localhost", "127.0.0.1", 1)

	browser, err := b.pw.Chromium.ConnectOverCDP(endpoint)
	if err != nil {
		return fmt.Errorf("connecting to Chrome at %s: %w\n\n"+
			"Start Chrome with a debugging port and its own profile directory, for example:\n"+
			"  chrome.exe --remote-debugging-port=9222 --user-data-dir=%q\n"+
			"The --user-data-dir is not optional: without it Chrome hands the command line to an "+
			"already-running instance and never opens the port",
			endpoint, err, filepath.Join(os.TempDir(), "chrome-debug"))
	}
	b.browser = browser

	contexts := browser.Contexts()
	if len(contexts) == 0 {
		// A browser with no context is unusual but possible. Opening one is
		// safe; it is ours, and closing it later closes nothing of theirs.
		ctx, err := browser.NewContext()
		if err != nil {
			browser.Close() //nolint:errcheck
			b.browser = nil
			return fmt.Errorf("opening a context on the attached browser: %w", err)
		}
		b.ctx = ctx
	} else {
		b.ctx = contexts[0]
	}
	b.attached = true
	b.ctx.SetDefaultTimeout(float64(b.cfg.DefaultTimeoutMs))
	b.ctx.SetDefaultNavigationTimeout(float64(b.cfg.NavigationTimeoutMs))

	b.lastUse = time.Now()
	b.armIdleLocked()
	b.logger.Printf("attached to Chrome at %s; its tabs are yours, and this server opens only its own", endpoint)
	return nil
}

// launchArgs is the command line: the automation flags dropped, plus whatever
// the operator added.
func (b *Browser) launchArgs() []string {
	args := []string{
		"--disable-blink-features=AutomationControlled",
		"--disable-dev-shm-usage",
		"--disable-infobars",
		fmt.Sprintf("--window-size=%d,%d", b.cfg.ViewportWidth, b.cfg.ViewportHeight),
	}
	return append(args, b.cfg.Args...)
}

// armIdleLocked restarts the countdown that closes an unused browser.
func (b *Browser) armIdleLocked() {
	if b.cfg.IdleTimeoutSeconds <= 0 || b.ctx == nil {
		return
	}
	d := time.Duration(b.cfg.IdleTimeoutSeconds) * time.Second
	if b.idle == nil {
		b.idle = time.AfterFunc(d, b.closeIfIdle)
		return
	}
	b.idle.Reset(d)
}

// closeIfIdle shuts the browser down if nothing has used it since the timer was
// armed. The check is repeated here because a call may have started in the
// window between the timer firing and this function taking the lock.
func (b *Browser) closeIfIdle() {
	b.mu.Lock()
	if b.ctx == nil || time.Since(b.lastUse) < time.Duration(b.cfg.IdleTimeoutSeconds)*time.Second {
		b.armIdleLocked()
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	b.logger.Printf("closing the idle browser; the next call starts a new one")
	b.Close()
}

// Close shuts the browser down. Cookies are saved first: they are what makes
// the next run look like a returning visitor rather than a new one.
func (b *Browser) Close() {
	// The state is taken and cleared under the lock and the closing happens
	// outside it, because closing a context fires page-close handlers that
	// would want the same lock.
	b.mu.Lock()
	if b.idle != nil {
		b.idle.Stop()
		b.idle = nil
	}
	ctx, browser, pw, attached := b.ctx, b.browser, b.pw, b.attached
	b.ctx, b.browser, b.pw, b.attached = nil, nil, nil, false
	// The cookie jar belongs to whoever owns the profile. In attach mode that
	// is the operator, and writing their cookies out to this server's file
	// would be taking a copy of something it was only lent.
	if ctx != nil && !attached {
		b.saveCookies(ctx)
	}
	b.mu.Unlock()

	// In attach mode only the connection is dropped. Closing the context would
	// close the operator's window out from under them, which is the worst
	// possible way for this server to exit - and it would happen on every idle
	// timeout, not just at shutdown.
	if ctx != nil && !attached {
		if err := ctx.Close(); err != nil {
			b.logger.Printf("closing the browser: %v", err)
		}
	}
	if browser != nil {
		if err := browser.Close(); err != nil {
			b.logger.Printf("disconnecting from the browser: %v", err)
		}
	}
	if pw != nil {
		if err := pw.Stop(); err != nil {
			b.logger.Printf("stopping playwright: %v", err)
		}
	}
}

// saveCookies writes the context's cookies out. Every failure here is logged
// and swallowed: losing the cookie jar costs a fresh-looking profile next run,
// which is not worth failing a tool call over.
func (b *Browser) saveCookies(ctx playwright.BrowserContext) {
	if b.search.CookieFile == "" {
		return
	}
	cookies, err := ctx.Cookies()
	if err != nil {
		b.logger.Printf("reading cookies: %v", err)
		return
	}
	data, err := json.Marshal(cookies)
	if err != nil {
		b.logger.Printf("encoding cookies: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(b.search.CookieFile), 0o755); err != nil {
		b.logger.Printf("creating the cookie directory: %v", err)
		return
	}
	// 0600: a Google cookie jar is a credential.
	if err := os.WriteFile(b.search.CookieFile, data, 0o600); err != nil {
		b.logger.Printf("writing %s: %v", b.search.CookieFile, err)
	}
}

// loadCookiesLocked seeds the context from a previous run.
func (b *Browser) loadCookiesLocked() {
	if b.search.CookieFile == "" {
		return
	}
	data, err := os.ReadFile(b.search.CookieFile)
	if err != nil {
		if !os.IsNotExist(err) {
			b.logger.Printf("reading %s: %v", b.search.CookieFile, err)
		}
		return
	}
	var cookies []playwright.Cookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		b.logger.Printf("%s is not a cookie file: %v", b.search.CookieFile, err)
		return
	}
	optional := make([]playwright.OptionalCookie, 0, len(cookies))
	for _, c := range cookies {
		// An expired cookie is not merely useless: handing one back is a
		// signal in itself, so drop them rather than replaying the jar whole.
		if c.Expires > 0 && float64(time.Now().Unix()) > c.Expires {
			continue
		}
		optional = append(optional, playwright.OptionalCookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   playwright.String(c.Domain),
			Path:     playwright.String(c.Path),
			Expires:  playwright.Float(c.Expires),
			HttpOnly: playwright.Bool(c.HttpOnly),
			Secure:   playwright.Bool(c.Secure),
		})
	}
	if len(optional) == 0 {
		return
	}
	if err := b.ctx.AddCookies(optional); err != nil {
		b.logger.Printf("restoring cookies: %v", err)
	}
}

// Open navigates a page and settles it: the consent banner dismissed, a human
// pause taken, and a block detected. Every scraper starts here, so that
// "Google served a CAPTCHA" is one error shape rather than fourteen different
// timeouts.
func (p *Page) Open(url string) error {
	if _, err := p.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	}); err != nil {
		return fmt.Errorf("loading %s: %w", url, err)
	}
	p.DismissConsent()
	if p.Blocked() {
		return ErrBlocked
	}
	return nil
}

// ErrBlocked is Google deciding this is a robot. It is reported as itself
// rather than as a scraping failure because the two want different responses:
// a selector that stopped matching is a bug in this server, and a block is a
// reason to wait, switch engine, or both.
var ErrBlocked = fmt.Errorf("the search engine served a bot check instead of results: " +
	"this address may be rate-limited. Wait a few minutes, or pass a different engine " +
	"(duckduckgo needs no browser at all and is rarely blocked)")

// Blocked reports whether the page is a CAPTCHA or rate-limit interstitial.
func (p *Page) Blocked() bool {
	if strings.Contains(p.URL(), "/sorry/") {
		return true
	}
	// Scholar is the reason for the gs_captcha entries: it serves its check
	// inline on the results URL rather than redirecting to /sorry/, so
	// without these a rate-limited Scholar looks like an empty page and gets
	// reported as broken extraction instead of as a block.
	count, err := p.Locator("iframe[src*='recaptcha'], #captcha-form, form[action*='sorry'], " +
		"div.g-recaptcha, #gs_captcha_ccl, #gs_captcha_f, form#gs_captcha_f").Count()
	if err == nil && count > 0 {
		return true
	}
	// Datacenter addresses are often refused with a text-only page that has no
	// captcha widget at all ("unusual traffic", "can't process your request").
	count, err = p.Locator("body:has-text('unusual traffic from your computer network'), " +
		"body:has-text(\"can't process your request\"), " +
		"body:has-text('automated queries')").Count()
	return err == nil && count > 0
}

// consentButtons is the accept/reject control on Google's consent interstitial,
// in the languages a redirected client is most likely to be shown. Rejecting is
// listed alongside accepting because either one dismisses the banner and gets
// to the results, which is all this needs.
const consentButtons = "button:has-text('Accept all'), button:has-text('I agree'), " +
	"button:has-text('Reject all'), button:has-text('Alle akzeptieren'), " +
	"button:has-text('Alle ablehnen'), button:has-text('Tout accepter'), " +
	"button:has-text('Tout refuser'), button:has-text('Aceptar todo'), " +
	"button:has-text('Rechazar todo'), button:has-text('Accetta tutto'), " +
	"button:has-text('Rifiuta tutto'), button:has-text('Alles accepteren'), " +
	"button[aria-label='Accept all'], button[aria-label='Reject all']"

// DismissConsent clicks past the cookie banner if one is showing, then pauses.
// A failure is ignored: the banner not being there is the common case, and a
// click that misses is not a reason to abandon a page that may have results on
// it anyway.
func (p *Page) DismissConsent() {
	button := p.Locator(consentButtons)
	if count, err := button.Count(); err == nil && count > 0 {
		if err := button.First().Click(playwright.LocatorClickOptions{
			Timeout: playwright.Float(5000),
		}); err == nil {
			_ = p.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
				State:   playwright.LoadStateDomcontentloaded,
				Timeout: playwright.Float(5000),
			})
		}
	}
	p.HumanDelay()
}

// HumanDelay pauses for a random moment. Machine-perfect timing between a page
// load and the first interaction is itself a signal, and this is cheap.
func (p *Page) HumanDelay() {
	p.WaitForTimeout(float64(500 + rand.Intn(1000))) //nolint:gosec // timing jitter, not a secret
}

// Settle waits a fixed time for the parts of a page that arrive after
// DOMContentLoaded - the results panel, the answer card, the map tiles. There
// is no event for "Google has finished rendering", so this is a wait, named
// honestly rather than dressed up as a condition.
func (p *Page) Settle(ms int) {
	p.WaitForTimeout(float64(ms))
}

// stealthJS is injected before every page load. None of it is exotic: it
// patches the handful of properties that differ between a headless Chromium and
// a browser a person is using, which is what the automation detectors read
// first. It does not make the browser undetectable and is not meant to - it
// makes it unremarkable.
const stealthJS = `
// navigator.webdriver is the single most-read automation signal.
Object.defineProperty(navigator, 'webdriver', { get: () => false });

// A headless browser reports no plugins at all, which no real one does.
Object.defineProperty(navigator, 'plugins', {
  get: () => [
    { name: 'PDF Viewer', filename: 'internal-pdf-viewer', description: 'Portable Document Format', length: 1 },
    { name: 'Chrome PDF Viewer', filename: 'mhjfbmdgcfjbbpaeojofohoefgiehjai', description: '', length: 1 },
    { name: 'Chromium PDF Viewer', filename: 'internal-pdf-viewer', description: '', length: 1 },
  ],
});
Object.defineProperty(navigator, 'languages', { get: () => ['en-US', 'en'] });

// window.chrome exists in every real Chrome and in no headless one.
if (!window.chrome) { window.chrome = {}; }
if (!window.chrome.runtime) {
  window.chrome.runtime = { connect() {}, sendMessage() {}, onMessage: { addListener() {} } };
}

// Playwright's own footprints.
delete window.__playwright;
delete window.__pw_manual;
delete window.__PW_inspect;

// An automated browser answers "denied" for notifications while reporting
// Notification.permission as "default", which is a contradiction no real
// browser produces.
if (navigator.permissions && navigator.permissions.query) {
  const original = navigator.permissions.query.bind(navigator.permissions);
  navigator.permissions.query = (params) =>
    params && params.name === 'notifications'
      ? Promise.resolve({ state: Notification.permission, onchange: null })
      : original(params);
}
`
