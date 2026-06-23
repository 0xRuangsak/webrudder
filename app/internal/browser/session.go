// Package browser wraps a headless Chromium (via rod/CDP) as a single stateful
// session: it tracks one live page and exposes the high-level actions the HTTP
// API needs — scan, read, click, fill, upload, download, etc.
//
// Element refs (e1, e2, …) are not held as Go pointers. scan() tags each DOM
// node with a `data-wr-ref` attribute and the ref is resolved back to the live
// element by that attribute. The DOM itself is the ref store, so refs survive
// until the next scan or a navigation rewrites the page.
//
// A background watcher turns two CDP signals into channels so a plain click can
// report what it caused: file-chooser interception (→ needs_file) and download
// progress (→ downloaded path).
//
// Robustness: every page operation is wrapped in a per-op timeout, so a slow or
// hostile page (heavy SPA, consent wall) can never block the session mutex
// forever. /status reads a lock-free cached snapshot, so it stays responsive
// even while a long navigation holds the page lock.
package browser

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Per-operation timeouts — the ceiling on how long any single action may hold
// the session lock.
const (
	navTimeout   = 30 * time.Second
	actTimeout   = 20 * time.Second
	infoTimeout  = 5 * time.Second
	snapTimeout  = 45 * time.Second
	downloadWait = 30 * time.Second
)

// Session is one browser + one page, guarded by a mutex so concurrent HTTP
// handlers serialize their access to the (non-thread-safe) page.
type Session struct {
	mu        sync.Mutex
	browser   *rod.Browser
	launcher  *launcher.Launcher
	page      *rod.Page
	downloads string

	// Lock-free cached page state for /status (updated after each op), so a
	// status check never waits on a long-running navigation.
	stMu      sync.RWMutex
	lastURL   string
	lastTitle string

	chooser chan struct{} // a click opened a file chooser
	dlBegin chan struct{} // a download started
	dlDone  chan string   // a download finished → saved path
	dlMu    sync.Mutex
	dlNames map[string]string // download GUID → suggested filename

	dlgMu      sync.Mutex
	dlgAccept  bool   // how to answer JS dialogs (default accept)
	dlgText    string // prompt text when accepting
	lastDialog string
}

// Element is one actionable node returned by scan.
type Element struct {
	Ref    string `json:"ref"`
	Role   string `json:"role"`
	Name   string `json:"name"`
	Type   string `json:"type,omitempty"`
	Href   string `json:"href,omitempty"`
	Kind   string `json:"kind,omitempty"`   // "upload" | "download" when statically known
	Accept string `json:"accept,omitempty"` // file input accept attribute
}

// ClickResult describes what a click caused.
type ClickResult struct {
	Navigated  bool
	URL        string
	Downloaded string // saved path if the click triggered a download
	NeedsFile  bool   // the click opened a file chooser (use Upload)
}

// Action is one step in a /batch request.
type Action struct {
	Do   string `json:"do"`
	Ref  string `json:"ref,omitempty"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
	File string `json:"file,omitempty"`
}

// New launches headless Chromium (rod auto-downloads the binary on first run),
// opens a page, starts the event watcher, and navigates to entryURL when given.
func New(entryURL, downloadsDir string) (*Session, error) {
	if downloadsDir == "" {
		downloadsDir = filepath.Join(os.TempDir(), "webrudder-downloads")
	}
	if err := os.MkdirAll(downloadsDir, 0o755); err != nil {
		return nil, fmt.Errorf("downloads dir: %w", err)
	}

	l := launcher.New().Headless(true)
	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch chromium: %w", err)
	}

	b := rod.New().ControlURL(controlURL)
	if err := b.Connect(); err != nil {
		l.Cleanup()
		return nil, fmt.Errorf("connect chromium: %w", err)
	}

	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		_ = b.Close()
		l.Cleanup()
		return nil, fmt.Errorf("open page: %w", err)
	}

	s := &Session{
		browser:   b,
		launcher:  l,
		page:      page,
		downloads: downloadsDir,
		chooser:   make(chan struct{}, 8),
		dlBegin:   make(chan struct{}, 8),
		dlDone:    make(chan string, 8),
		dlNames:   map[string]string{},
		dlgAccept: true,
	}
	s.armBrowser()
	s.armPage(s.page)

	if entryURL != "" {
		if err := s.Goto(entryURL); err != nil {
			s.Close()
			return nil, err
		}
	}
	s.cacheState()
	return s, nil
}

// armBrowser sets the download behavior and watches browser-level download
// events (saved as GUID, renamed to the suggested filename on completion).
func (s *Session) armBrowser() {
	_ = proto.BrowserSetDownloadBehavior{
		Behavior:         proto.BrowserSetDownloadBehaviorBehaviorAllowAndName,
		BrowserContextID: s.browser.BrowserContextID,
		DownloadPath:     s.downloads,
	}.Call(s.browser)

	go s.browser.EachEvent(
		func(e *proto.PageDownloadWillBegin) {
			s.dlMu.Lock()
			s.dlNames[e.GUID] = e.SuggestedFilename
			s.dlMu.Unlock()
			signal(s.dlBegin)
		},
		func(e *proto.PageDownloadProgress) {
			if e.State != proto.PageDownloadProgressStateCompleted {
				return
			}
			s.dlMu.Lock()
			name := s.dlNames[e.GUID]
			delete(s.dlNames, e.GUID)
			s.dlMu.Unlock()

			saved := filepath.Join(s.downloads, e.GUID)
			// filepath.Base strips any path components the page may have smuggled
			// into the suggested filename (e.g. "../../.bashrc").
			if name = filepath.Base(name); name != "" && name != "." && name != ".." {
				dst := filepath.Join(s.downloads, name)
				if os.Rename(saved, dst) == nil {
					saved = dst
				}
			}
			select {
			case s.dlDone <- saved:
			default:
			}
		},
	)()
}

// armPage wires file-chooser interception and JS-dialog auto-handling on a page,
// once per page. File choosers signal the chooser channel; JS dialogs are
// answered per the current policy (default accept) so a page never hangs.
func (s *Session) armPage(p *rod.Page) {
	_ = proto.PageSetInterceptFileChooserDialog{Enabled: true}.Call(p)
	go p.EachEvent(func(e *proto.PageFileChooserOpened) { signal(s.chooser) })()
	go p.EachEvent(func(e *proto.PageJavascriptDialogOpening) {
		s.dlgMu.Lock()
		accept, text := s.dlgAccept, s.dlgText
		s.lastDialog = e.Message
		s.dlgMu.Unlock()
		_ = proto.PageHandleJavaScriptDialog{Accept: accept, PromptText: text}.Call(p)
	})()
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func drainStr(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// Close tears down the browser and removes the launcher's temp data.
func (s *Session) Close() {
	if s.browser != nil {
		_ = s.browser.Close()
	}
	if s.launcher != nil {
		s.launcher.Cleanup()
	}
}

// settle waits (bounded) for the page to finish loading and go quiet after an
// action. Timeouts are swallowed on purpose — a slow/animated page should not
// hang the API.
func (s *Session) settle() {
	_ = rod.Try(func() { s.page.Timeout(5 * time.Second).MustWaitLoad() })
	_ = rod.Try(func() { s.page.Timeout(3 * time.Second).MustWaitStable() })
}

// info reads the live URL/title, bounded so it can never hang.
func (s *Session) info() (pageURL, title string) {
	got, err := s.page.Timeout(infoTimeout).Info()
	if err != nil {
		return "", ""
	}
	return got.URL, got.Title
}

// cacheState snapshots the live URL/title into the lock-free cache for /status.
func (s *Session) cacheState() {
	u, t := s.info()
	if u != "" {
		s.stMu.Lock()
		s.lastURL, s.lastTitle = u, t
		s.stMu.Unlock()
	}
}

// Status returns the cached URL and title without taking the page lock, so it
// stays responsive even while a long navigation is in flight.
func (s *Session) Status() (pageURL, title string) {
	s.stMu.RLock()
	defer s.stMu.RUnlock()
	return s.lastURL, s.lastTitle
}

// validNavURL allows only http/https (and about:blank). Other schemes — file,
// chrome, data, javascript — are rejected: webrudder is a web automation tool
// and allowing them invites local-file disclosure. Loopback/private hosts are
// intentionally NOT blocked (testing local apps is the primary use case).
func validNavURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return nil
	case "about":
		if raw == "about:blank" {
			return nil
		}
	}
	return fmt.Errorf("unsupported url scheme %q — only http and https are allowed", u.Scheme)
}

// Goto navigates to an absolute URL (explicit jump; normal navigation is by clicking).
func (s *Session) Goto(rawURL string) error {
	if err := validNavURL(rawURL); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.page.Timeout(navTimeout).Navigate(rawURL); err != nil {
		s.cacheState()
		return fmt.Errorf("navigate: %w", err)
	}
	s.settle()
	s.cacheState()
	return nil
}

// scanJS collects visible interactive elements, tags each with data-wr-ref, and
// returns their metadata as a JSON string.
const scanJS = `() => {
  const sel = 'a,button,input,select,textarea,[role=button],[onclick],[contenteditable=""],[contenteditable=true]';
  const seen = new Set();
  const nodes = Array.from(document.querySelectorAll(sel));
  document.querySelectorAll('input[type=file]').forEach(n => nodes.push(n));
  const out = [];
  let i = 0;
  for (const el of nodes) {
    if (seen.has(el)) continue;
    seen.add(el);
    const tag = el.tagName.toLowerCase();
    const type = (el.getAttribute && el.getAttribute('type')) || '';
    const isFile = tag === 'input' && type.toLowerCase() === 'file';
    const style = window.getComputedStyle(el);
    const rect = el.getBoundingClientRect();
    const visible = style.display !== 'none' && style.visibility !== 'hidden' &&
      style.opacity !== '0' && rect.width > 0 && rect.height > 0;
    if (!visible && !isFile) continue;
    if (el.disabled) continue;
    i++;
    const ref = 'e' + i;
    el.setAttribute('data-wr-ref', ref);
    let name = (el.getAttribute('aria-label') || el.innerText || el.value ||
      el.getAttribute('placeholder') || el.getAttribute('name') ||
      el.getAttribute('title') || '').toString().trim().replace(/\s+/g, ' ').slice(0, 120);
    const role = el.getAttribute('role') || tag;
    const o = { ref: ref, role: role, name: name };
    if (tag === 'input') o.type = type || 'text';
    const href = el.getAttribute('href');
    if (href) o.href = href;
    if (isFile) {
      o.kind = 'upload';
      const acc = el.getAttribute('accept');
      if (acc) o.accept = acc;
    } else if (href) {
      const dl = el.hasAttribute('download') ||
        /\.(pdf|zip|csv|xlsx?|docx?|pptx?|png|jpe?g|gif|mp4|mp3|tar|gz|dmg|exe)(\?|$)/i.test(href);
      if (dl) o.kind = 'download';
    }
    out.push(o);
  }
  return JSON.stringify(out);
}`

// Scan returns the page's actionable elements, assigning fresh refs.
func (s *Session) Scan() ([]Element, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, err := s.page.Timeout(actTimeout).Eval(scanJS)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	var els []Element
	if err := json.Unmarshal([]byte(obj.Value.Str()), &els); err != nil {
		return nil, fmt.Errorf("parse scan: %w", err)
	}
	return els, nil
}

// Read returns the page URL, title, and visible text.
func (s *Session) Read() (pageURL, title, text string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pageURL, title = s.info()
	obj, err := s.page.Timeout(actTimeout).Eval(`() => document.body ? document.body.innerText : ""`)
	if err != nil {
		return pageURL, title, "", fmt.Errorf("read: %w", err)
	}
	return pageURL, title, obj.Value.Str(), nil
}

// Snap returns a PNG screenshot. fullPage captures the entire scrollable page;
// otherwise just the current viewport.
func (s *Session) Snap(fullPage bool) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	png, err := s.page.Timeout(snapTimeout).Screenshot(fullPage, nil)
	if err != nil {
		return nil, fmt.Errorf("snap: %w", err)
	}
	return png, nil
}

// Snapshot returns the page's accessibility tree as an indented text outline
// (role + accessible name). Unlike scan (interactive elements only) this
// includes non-interactive nodes and their ARIA state — e.g. a Wordle tile
// reads `image "1st letter, C, absent"` — so state-bearing pages can be read as
// text instead of a screenshot.
func (s *Session) Snapshot() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := proto.AccessibilityGetFullAXTree{}.Call(s.page.Timeout(actTimeout))
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	byID := make(map[proto.AccessibilityAXNodeID]*proto.AccessibilityAXNode, len(res.Nodes))
	for _, n := range res.Nodes {
		byID[n.NodeID] = n
	}
	var b strings.Builder
	var walk func(id proto.AccessibilityAXNodeID, depth int)
	walk = func(id proto.AccessibilityAXNodeID, depth int) {
		n := byID[id]
		if n == nil {
			return
		}
		next := depth
		if line, ok := axLine(n); ok {
			b.WriteString(strings.Repeat("  ", depth))
			b.WriteString(line)
			b.WriteByte('\n')
			next = depth + 1
		}
		for _, c := range n.ChildIDs {
			walk(c, next)
		}
	}
	for _, n := range res.Nodes {
		if n.ParentID == "" || byID[n.ParentID] == nil {
			walk(n.NodeID, 0)
		}
	}
	if b.Len() == 0 {
		return "(empty)\n", nil
	}
	return b.String(), nil
}

// axLine formats one AX node and reports whether it's worth showing (drops
// ignored nodes and unnamed structural noise).
func axLine(n *proto.AccessibilityAXNode) (string, bool) {
	if n.Ignored {
		return "", false
	}
	role := axStr(n.Role)
	if role == "" {
		return "", false
	}
	name := axStr(n.Name)
	if name == "" {
		switch role {
		case "generic", "none", "InlineTextBox", "LineBreak", "paragraph", "list", "listitem", "group":
			return "", false
		}
		return role, true
	}
	return fmt.Sprintf("%s %q", role, name), true
}

func axStr(v *proto.AccessibilityAXValue) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(v.Value.Str())
}

// HTML returns the page's outer HTML, or a single element's outer HTML when ref
// is given. The escape hatch for state the accessibility tree doesn't expose
// (e.g. CSS-only styling, custom data-* attributes).
func (s *Session) HTML(ref string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref != "" {
		el, err := s.elem(ref)
		if err != nil {
			return "", err
		}
		h, err := el.Timeout(actTimeout).HTML()
		if err != nil {
			return "", fmt.Errorf("html: %w", err)
		}
		return h, nil
	}
	h, err := s.page.Timeout(actTimeout).HTML()
	if err != nil {
		return "", fmt.Errorf("html: %w", err)
	}
	return h, nil
}

// elem resolves a ref (e1, e2, …) to the live element via its data-wr-ref tag.
func (s *Session) elem(ref string) (*rod.Element, error) {
	el, err := s.page.Timeout(actTimeout).Element(fmt.Sprintf(`[data-wr-ref=%q]`, ref))
	if err != nil {
		return nil, fmt.Errorf("ref %q not found — re-scan the page", ref)
	}
	return el, nil
}

// Click clicks an element and reports what it caused: navigation, a download, or
// a file chooser opening (needs_file).
func (s *Session) Click(ref string) (ClickResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res ClickResult
	el, err := s.elem(ref)
	if err != nil {
		return res, err
	}

	drain(s.chooser)
	drain(s.dlBegin)
	drainStr(s.dlDone)

	before, _ := s.info()
	if err := el.Timeout(actTimeout).Click(proto.InputMouseButtonLeft, 1); err != nil {
		return res, fmt.Errorf("click: %w", err)
	}
	s.settle()
	after, _ := s.info()
	res.Navigated = after != before
	res.URL = after
	s.cacheState()

	select {
	case <-s.chooser:
		res.NeedsFile = true
	default:
	}

	// If a download started, wait (bounded) for it to finish; otherwise no penalty.
	select {
	case <-s.dlBegin:
		select {
		case p := <-s.dlDone:
			res.Downloaded = p
		case <-time.After(downloadWait):
		}
	default:
	}

	return res, nil
}

// Fill replaces the value of an input/textarea with text.
func (s *Session) Fill(ref, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, err := s.elem(ref)
	if err != nil {
		return err
	}
	_ = el.Timeout(actTimeout).SelectAllText() // best-effort clear before typing
	if err := el.Timeout(actTimeout).Input(text); err != nil {
		return fmt.Errorf("fill: %w", err)
	}
	return nil
}

// Upload attaches a local file to a file input. ref is the element you'd click
// to upload; if it isn't itself a file input, the first file input is used.
func (s *Session) Upload(ref, file string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	abs, err := filepath.Abs(file)
	if err != nil {
		return fmt.Errorf("file path: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	el, err := s.elem(ref)
	if err != nil {
		return err
	}
	if !isFileInput(el) {
		alt, e := s.page.Timeout(actTimeout).Element(`input[type=file]`)
		if e != nil {
			return fmt.Errorf("no file input found for ref %q", ref)
		}
		el = alt
	}
	if err := el.Timeout(actTimeout).SetFiles([]string{abs}); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

func isFileInput(el *rod.Element) bool {
	t, err := el.Timeout(infoTimeout).Attribute("type")
	return err == nil && t != nil && strings.EqualFold(*t, "file")
}

// Download clicks an element that triggers a download, waits for it to finish,
// and returns the saved path (moved into dir when dir differs from the default).
func (s *Session) Download(ref, dir string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir == "" {
		dir = s.downloads
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("download dir: %w", err)
	}
	el, err := s.elem(ref)
	if err != nil {
		return "", err
	}

	drainStr(s.dlDone)
	if err := el.Timeout(actTimeout).Click(proto.InputMouseButtonLeft, 1); err != nil {
		return "", fmt.Errorf("download click: %w", err)
	}

	defer s.cacheState()
	select {
	case saved := <-s.dlDone:
		if dir != s.downloads {
			dst := filepath.Join(dir, filepath.Base(saved))
			if os.Rename(saved, dst) == nil {
				saved = dst
			}
		}
		return saved, nil
	case <-time.After(downloadWait):
		return "", fmt.Errorf("download timed out — did the click trigger a download?")
	}
}

// Batch runs a sequence of actions, returning one result object per action.
// Each action locks the session independently, so steps are serialized.
func (s *Session) Batch(actions []Action) []map[string]any {
	results := make([]map[string]any, 0, len(actions))
	for _, a := range actions {
		r := map[string]any{"ok": true}
		var err error
		switch a.Do {
		case "click":
			var cr ClickResult
			if cr, err = s.Click(a.Ref); err == nil {
				r["navigated"] = cr.Navigated
				if cr.Navigated {
					r["url"] = cr.URL
				}
				if cr.Downloaded != "" {
					r["downloaded"] = cr.Downloaded
				}
				if cr.NeedsFile {
					r["ok"] = false
					r["needs_file"] = true
					r["error"] = "click opened a file chooser — use upload"
				}
			}
		case "fill":
			err = s.Fill(a.Ref, a.Text)
		case "goto":
			err = s.Goto(a.URL)
		case "upload":
			err = s.Upload(a.Ref, a.File)
		default:
			err = fmt.Errorf("unknown action %q", a.Do)
		}
		if err != nil {
			r["ok"] = false
			r["error"] = err.Error()
		}
		results = append(results, r)
	}
	return results
}

const waitTimeout = 15 * time.Second

var pressKeys = map[string]input.Key{
	"enter": input.Enter, "return": input.Enter,
	"tab": input.Tab, "escape": input.Escape, "esc": input.Escape,
	"backspace": input.Backspace, "delete": input.Delete, "del": input.Delete,
	"space": input.Space, "home": input.Home, "end": input.End,
	"pageup": input.PageUp, "pagedown": input.PageDown,
	"up": input.ArrowUp, "arrowup": input.ArrowUp,
	"down": input.ArrowDown, "arrowdown": input.ArrowDown,
	"left": input.ArrowLeft, "arrowleft": input.ArrowLeft,
	"right": input.ArrowRight, "arrowright": input.ArrowRight,
	"a": input.KeyA, "b": input.KeyB, "c": input.KeyC, "d": input.KeyD, "e": input.KeyE,
	"f": input.KeyF, "g": input.KeyG, "h": input.KeyH, "i": input.KeyI, "j": input.KeyJ,
	"k": input.KeyK, "l": input.KeyL, "m": input.KeyM, "n": input.KeyN, "o": input.KeyO,
	"p": input.KeyP, "q": input.KeyQ, "r": input.KeyR, "s": input.KeyS, "t": input.KeyT,
	"u": input.KeyU, "v": input.KeyV, "w": input.KeyW, "x": input.KeyX, "y": input.KeyY, "z": input.KeyZ,
	"0": input.Digit0, "1": input.Digit1, "2": input.Digit2, "3": input.Digit3, "4": input.Digit4,
	"5": input.Digit5, "6": input.Digit6, "7": input.Digit7, "8": input.Digit8, "9": input.Digit9,
}

var pressMods = map[string]input.Key{
	"control": input.ControlLeft, "ctrl": input.ControlLeft, "ctl": input.ControlLeft,
	"shift": input.ShiftLeft, "alt": input.AltLeft, "option": input.AltLeft,
	"meta": input.MetaLeft, "cmd": input.MetaLeft, "command": input.MetaLeft, "super": input.MetaLeft,
}

// Press presses a key or chord like "Enter", "Tab", "ArrowDown", "Control+a".
// Modifiers are held around the final key.
func (s *Session) Press(combo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var mods []input.Key
	var key input.Key
	var have bool
	for _, p := range strings.Split(combo, "+") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if m, ok := pressMods[p]; ok {
			mods = append(mods, m)
			continue
		}
		k, ok := pressKeys[p]
		if !ok {
			return fmt.Errorf("unknown key %q", p)
		}
		key, have = k, true
	}
	if !have {
		return fmt.Errorf("no key in %q", combo)
	}
	if err := s.page.Timeout(actTimeout).KeyActions().Press(mods...).Type(key).Release(mods...).Do(); err != nil {
		return fmt.Errorf("press: %w", err)
	}
	s.settle()
	s.cacheState()
	return nil
}

// TypeText inserts text into the currently focused element. For a specific field
// prefer Fill (by ref); this types into whatever has focus and does not emit
// per-key keydown events.
func (s *Session) TypeText(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.page.Timeout(actTimeout).InsertText(text); err != nil {
		return fmt.Errorf("type: %w", err)
	}
	return nil
}

// Back, Forward, Reload navigate browser history.
func (s *Session) Back() error    { return s.nav(func() error { return s.page.Timeout(navTimeout).NavigateBack() }) }
func (s *Session) Forward() error { return s.nav(func() error { return s.page.Timeout(navTimeout).NavigateForward() }) }
func (s *Session) Reload() error  { return s.nav(func() error { return s.page.Timeout(navTimeout).Reload() }) }

func (s *Session) nav(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	s.settle()
	s.cacheState()
	return nil
}

// Hover moves the pointer over an element.
func (s *Session) Hover(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, err := s.elem(ref)
	if err != nil {
		return err
	}
	if err := el.Timeout(actTimeout).Hover(); err != nil {
		return fmt.Errorf("hover: %w", err)
	}
	return nil
}

// Scroll scrolls an element into view (when ref is set) or the page by amount px
// in dir (up/down/left/right; default down 500).
func (s *Session) Scroll(ref, dir string, amount float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref != "" {
		el, err := s.elem(ref)
		if err != nil {
			return err
		}
		if err := el.Timeout(actTimeout).ScrollIntoView(); err != nil {
			return fmt.Errorf("scroll: %w", err)
		}
		return nil
	}
	if amount == 0 {
		amount = 500
	}
	var x, y float64
	switch strings.ToLower(dir) {
	case "down", "":
		y = amount
	case "up":
		y = -amount
	case "right":
		x = amount
	case "left":
		x = -amount
	default:
		return fmt.Errorf("unknown scroll dir %q", dir)
	}
	if err := s.page.Mouse.Scroll(x, y, 1); err != nil {
		return fmt.Errorf("scroll: %w", err)
	}
	return nil
}

// SelectOption selects dropdown options by visible text.
func (s *Session) SelectOption(ref string, values []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, err := s.elem(ref)
	if err != nil {
		return err
	}
	if err := el.Timeout(actTimeout).Select(values, true, rod.SelectorTypeText); err != nil {
		return fmt.Errorf("select: %w", err)
	}
	return nil
}

// SetChecked toggles a checkbox/radio to the desired state (clicks only if needed).
func (s *Session) SetChecked(ref string, want bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, err := s.elem(ref)
	if err != nil {
		return err
	}
	cur := false
	if p, e := el.Property("checked"); e == nil {
		cur = p.Bool()
	}
	if cur != want {
		if err := el.Timeout(actTimeout).Click(proto.InputMouseButtonLeft, 1); err != nil {
			return fmt.Errorf("check: %w", err)
		}
	}
	return nil
}

// Wait blocks until a selector/text appears (or a selector goes away when gone),
// or for ms milliseconds. Bounded so it can't hang indefinitely.
func (s *Session) Wait(selector, text string, ms int, gone bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms > 0 {
		d := time.Duration(ms) * time.Millisecond
		if d > 60*time.Second {
			d = 60 * time.Second
		}
		time.Sleep(d)
		return nil
	}
	if selector != "" {
		if gone {
			el, err := s.page.Timeout(infoTimeout).Element(selector)
			if err != nil {
				return nil // already absent
			}
			if err := el.Timeout(waitTimeout).WaitInvisible(); err != nil {
				return fmt.Errorf("wait gone: %w", err)
			}
			return nil
		}
		if err := rod.Try(func() { s.page.Timeout(waitTimeout).MustElement(selector) }); err != nil {
			return fmt.Errorf("wait selector: %w", err)
		}
		return nil
	}
	if text != "" {
		if err := rod.Try(func() { s.page.Timeout(waitTimeout).MustSearch(text) }); err != nil {
			return fmt.Errorf("wait text: %w", err)
		}
		return nil
	}
	return fmt.Errorf("wait needs ms, selector, or text")
}

// --- dialogs ---

// SetDialog sets how future JS dialogs (alert/confirm/prompt) are answered.
// Default is accept; dialogs are always auto-handled so the page never hangs.
func (s *Session) SetDialog(accept bool, text string) {
	s.dlgMu.Lock()
	s.dlgAccept, s.dlgText = accept, text
	s.dlgMu.Unlock()
}

// --- session state (cookies + storage) ---

// State is a portable session snapshot: cookies plus the current origin's
// localStorage and sessionStorage. The caller persists it — webrudder writes
// nothing to disk.
type State struct {
	Cookies []*proto.NetworkCookieParam `json:"cookies"`
	Local   map[string]string           `json:"local"`
	Session map[string]string           `json:"session"`
}

// GetState exports cookies + current-origin storage.
func (s *Session) GetState() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st State
	cookies, err := s.browser.GetCookies()
	if err != nil {
		return st, fmt.Errorf("cookies: %w", err)
	}
	for _, c := range cookies {
		st.Cookies = append(st.Cookies, &proto.NetworkCookieParam{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: c.SameSite, Expires: c.Expires,
		})
	}
	st.Local = s.storageMap("localStorage")
	st.Session = s.storageMap("sessionStorage")
	return st, nil
}

// SetState restores cookies + storage. localStorage/sessionStorage apply to the
// CURRENT origin — navigate to the target site before calling this.
func (s *Session) SetState(st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(st.Cookies) > 0 {
		if err := s.browser.SetCookies(st.Cookies); err != nil {
			return fmt.Errorf("set cookies: %w", err)
		}
	}
	s.writeStorage("localStorage", st.Local)
	s.writeStorage("sessionStorage", st.Session)
	return nil
}

func (s *Session) storageMap(which string) map[string]string {
	m := map[string]string{}
	obj, err := s.page.Timeout(actTimeout).Eval(
		fmt.Sprintf(`() => JSON.stringify(Object.fromEntries(Object.entries(%s)))`, which))
	if err == nil {
		_ = json.Unmarshal([]byte(obj.Value.Str()), &m)
	}
	return m
}

func (s *Session) writeStorage(which string, m map[string]string) {
	if len(m) == 0 {
		return
	}
	data, _ := json.Marshal(m)
	_, _ = s.page.Timeout(actTimeout).Eval(
		fmt.Sprintf(`(d) => { const m = JSON.parse(d); for (const k in m) %s.setItem(k, m[k]); }`, which),
		string(data))
}

// (multi-tab support intentionally omitted — single-page session by design)
