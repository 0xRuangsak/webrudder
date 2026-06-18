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
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Session is one browser + one page, guarded by a mutex so concurrent HTTP
// handlers serialize their access to the (non-thread-safe) page.
type Session struct {
	mu        sync.Mutex
	browser   *rod.Browser
	launcher  *launcher.Launcher
	page      *rod.Page
	downloads string

	chooser chan struct{} // a click opened a file chooser
	dlBegin chan struct{} // a download started
	dlDone  chan string   // a download finished → saved path
	dlMu    sync.Mutex
	dlNames map[string]string // download GUID → suggested filename
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
	}
	s.startWatchers()

	if entryURL != "" {
		if err := s.Goto(entryURL); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// startWatchers enables file-chooser interception and download events, then
// spawns goroutines that translate those CDP events into channel signals. The
// goroutines exit when the page/browser context is cancelled by Close.
func (s *Session) startWatchers() {
	_ = proto.PageSetInterceptFileChooserDialog{Enabled: true}.Call(s.page)
	_ = proto.BrowserSetDownloadBehavior{
		Behavior:         proto.BrowserSetDownloadBehaviorBehaviorAllowAndName,
		BrowserContextID: s.browser.BrowserContextID,
		DownloadPath:     s.downloads,
	}.Call(s.browser)

	// File chooser is a page-level event.
	go s.page.EachEvent(func(e *proto.PageFileChooserOpened) {
		signal(s.chooser)
	})()

	// Download lifecycle is a browser-level event (saved as GUID, renamed to the
	// suggested filename on completion).
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

// Status returns the current URL and title.
func (s *Session) Status() (pageURL, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info()
}

func (s *Session) info() (pageURL, title string) {
	info, err := s.page.Info()
	if err != nil {
		return "", ""
	}
	return info.URL, info.Title
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
	if err := s.page.Navigate(rawURL); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	s.settle()
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
	res, err := s.page.Eval(scanJS)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	var els []Element
	if err := json.Unmarshal([]byte(res.Value.Str()), &els); err != nil {
		return nil, fmt.Errorf("parse scan: %w", err)
	}
	return els, nil
}

// Read returns the page URL, title, and visible text.
func (s *Session) Read() (pageURL, title, text string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pageURL, title = s.info()
	res, err := s.page.Eval(`() => document.body ? document.body.innerText : ""`)
	if err != nil {
		return pageURL, title, "", fmt.Errorf("read: %w", err)
	}
	return pageURL, title, res.Value.Str(), nil
}

// Snap returns a PNG screenshot. fullPage captures the entire scrollable page;
// otherwise just the current viewport.
func (s *Session) Snap(fullPage bool) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	png, err := s.page.Screenshot(fullPage, nil)
	if err != nil {
		return nil, fmt.Errorf("snap: %w", err)
	}
	return png, nil
}

// elem resolves a ref (e1, e2, …) to the live element via its data-wr-ref tag.
func (s *Session) elem(ref string) (*rod.Element, error) {
	el, err := s.page.Element(fmt.Sprintf(`[data-wr-ref=%q]`, ref))
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
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return res, fmt.Errorf("click: %w", err)
	}
	s.settle()
	after, _ := s.info()
	res.Navigated = after != before
	res.URL = after

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
		case <-time.After(30 * time.Second):
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
	_ = el.SelectAllText() // best-effort clear before typing
	if err := el.Input(text); err != nil {
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
		alt, e := s.page.Element(`input[type=file]`)
		if e != nil {
			return fmt.Errorf("no file input found for ref %q", ref)
		}
		el = alt
	}
	if err := el.SetFiles([]string{abs}); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

func isFileInput(el *rod.Element) bool {
	t, err := el.Attribute("type")
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
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return "", fmt.Errorf("download click: %w", err)
	}

	select {
	case saved := <-s.dlDone:
		if dir != s.downloads {
			dst := filepath.Join(dir, filepath.Base(saved))
			if os.Rename(saved, dst) == nil {
				saved = dst
			}
		}
		return saved, nil
	case <-time.After(30 * time.Second):
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
