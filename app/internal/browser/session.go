// Package browser wraps a headless Chromium (via rod/CDP) as a single stateful
// session: it tracks one live page and exposes the high-level actions the HTTP
// API needs — scan, read, click, fill, upload, download, etc.
//
// Element refs (e1, e2, …) are not held as Go pointers. scan() tags each DOM
// node with a `data-wr-ref` attribute and the ref is resolved back to the live
// element by that attribute. The DOM itself is the ref store, so refs survive
// until the next scan or a navigation rewrites the page.
package browser

import (
	"fmt"
	"encoding/json"
	"net/url"
	"os"
	"path"
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

// Action is one step in a /batch request.
type Action struct {
	Do   string `json:"do"`
	Ref  string `json:"ref,omitempty"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
	File string `json:"file,omitempty"`
}

// New launches headless Chromium (rod auto-downloads the binary on first run),
// opens a page, and navigates to entryURL when given.
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

	s := &Session{browser: b, launcher: l, page: page, downloads: downloadsDir}
	if entryURL != "" {
		if err := s.Goto(entryURL); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
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
// action, so the next read sees the new state. Timeouts are swallowed on
// purpose — a slow/animated page should not hang the API.
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

// Goto navigates to an absolute URL (explicit jump; normal navigation is by clicking).
func (s *Session) Goto(rawURL string) error {
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

// Snap returns a PNG screenshot of the current viewport.
func (s *Session) Snap() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	png, err := s.page.Screenshot(false, nil)
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

// Click clicks an element and reports whether the URL changed.
func (s *Session) Click(ref string) (navigated bool, newURL string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, err := s.elem(ref)
	if err != nil {
		return false, "", err
	}
	before, _ := s.info()
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return false, "", fmt.Errorf("click: %w", err)
	}
	s.settle()
	after, _ := s.info()
	return after != before, after, nil
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
// and writes the bytes into dir (or the session default), returning the path.
func (s *Session) Download(ref, dir string) (saved string, err error) {
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

	name := downloadName(el)
	wait := s.browser.MustWaitDownload()
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return "", fmt.Errorf("download click: %w", err)
	}

	done := make(chan []byte, 1)
	go func() {
		defer func() { _ = recover() }()
		done <- wait()
	}()
	select {
	case data := <-done:
		saved = filepath.Join(dir, name)
		if err := os.WriteFile(saved, data, 0o644); err != nil {
			return "", fmt.Errorf("save download: %w", err)
		}
		return saved, nil
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("download timed out — did the click trigger a download?")
	}
}

// downloadName derives a filename from the element's href, falling back to a default.
func downloadName(el *rod.Element) string {
	if h, err := el.Attribute("href"); err == nil && h != nil && *h != "" {
		if u, err := url.Parse(*h); err == nil {
			base := path.Base(u.Path)
			if base != "" && base != "/" && base != "." {
				return base
			}
		}
	}
	return "download.bin"
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
			var nav bool
			var u string
			if nav, u, err = s.Click(a.Ref); err == nil {
				r["navigated"] = nav
				if nav {
					r["url"] = u
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
