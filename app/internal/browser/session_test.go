package browser

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

func hostOf(rawURL string) string {
	u, _ := url.Parse(rawURL)
	return u.Hostname()
}

func TestValidNavURL(t *testing.T) {
	ok := []string{"http://example.com", "https://x.io/y", "about:blank", "http://localhost:3000"}
	for _, u := range ok {
		if err := validNavURL(u); err != nil {
			t.Errorf("validNavURL(%q) unexpected error: %v", u, err)
		}
	}
	bad := []string{"file:///etc/passwd", "chrome://settings", "javascript:alert(1)", "data:text/html,x", "ftp://x"}
	for _, u := range bad {
		if err := validNavURL(u); err == nil {
			t.Errorf("validNavURL(%q) = nil, want error", u)
		}
	}
}

const testIndexHTML = `<!doctype html><html><body>
<h1>Test Home</h1>
<p>marker-text-123</p>
<a href="/next">Go Next</a>
<input id="name" type="text" placeholder="yourname">
<input id="f" type="file" accept="image/*">
<button id="up" onclick="document.getElementById('f').click()">Choose File</button>
<a id="dl" href="/file.txt" download>Download File</a>
<input id="cb" type="checkbox" name="agree">
<select id="sel" name="choice"><option>one</option><option>two</option></select>
</body></html>`

func testServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(testIndexHTML))
	})
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><html><body><h1>Next Page</h1></body></html>"))
	})
	mux.HandleFunc("/file.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="file.txt"`)
		_, _ = w.Write([]byte("hello-download"))
	})
	return httptest.NewServer(mux)
}

func refByName(els []Element, substr string) string {
	for _, e := range els {
		if strings.Contains(e.Name, substr) {
			return e.Ref
		}
	}
	return ""
}

func refByKind(els []Element, kind string) string {
	for _, e := range els {
		if e.Kind == kind {
			return e.Ref
		}
	}
	return ""
}

// TestSessionFunctional drives a real headless browser against a local test
// server, exercising scan/read/fill/upload/download/click and the click-time
// download + file-chooser detection. Needs Chromium; skipped under -short.
func TestSessionFunctional(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a browser; skipped in -short")
	}
	ts := testServer()
	defer ts.Close()
	tmp := t.TempDir()

	s, err := New(ts.URL, tmp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	els, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(els) == 0 {
		t.Fatal("Scan returned no elements")
	}

	// read
	if _, _, text, err := s.Read(); err != nil {
		t.Fatalf("Read: %v", err)
	} else if !strings.Contains(text, "marker-text-123") {
		t.Errorf("Read missing marker, got: %q", text)
	}

	// scan kind hints
	upRef := refByKind(els, "upload")
	dlRef := refByKind(els, "download")
	if upRef == "" {
		t.Error("scan did not tag the file input kind=upload")
	}
	if dlRef == "" {
		t.Error("scan did not tag the download link kind=download")
	}

	// a11y snapshot surfaces non-interactive names as text (what scan misses)
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !strings.Contains(snap, "Go Next") {
		t.Errorf("snapshot missing link name; got:\n%s", snap)
	}

	// raw HTML escape hatch
	html, err := s.HTML("")
	if err != nil {
		t.Fatalf("HTML: %v", err)
	}
	if !strings.Contains(html, `type="file"`) {
		t.Error("HTML missing the file input markup")
	}

	// check (toggle checkbox to checked)
	var cbRef string
	for _, e := range els {
		if e.Type == "checkbox" {
			cbRef = e.Ref
			break
		}
	}
	if cbRef == "" {
		t.Fatal("checkbox not found in scan")
	}
	if err := s.SetChecked(cbRef, true); err != nil {
		t.Fatalf("SetChecked: %v", err)
	}
	if !s.page.MustEval(`() => document.getElementById('cb').checked`).Bool() {
		t.Error("checkbox not checked after SetChecked(true)")
	}

	// select (pick option by visible text)
	var selRef string
	for _, e := range els {
		if e.Role == "select" {
			selRef = e.Ref
			break
		}
	}
	if selRef == "" {
		t.Fatal("select not found in scan")
	}
	if err := s.SelectOption(selRef, []string{"two"}); err != nil {
		t.Fatalf("SelectOption: %v", err)
	}
	if v := s.page.MustEval(`() => document.getElementById('sel').value`).Str(); v != "two" {
		t.Errorf("select value = %q, want two", v)
	}

	// hover / scroll / wait / press / type — smoke (no error)
	if err := s.Hover(cbRef); err != nil {
		t.Errorf("Hover: %v", err)
	}
	if err := s.Scroll("", "down", 100); err != nil {
		t.Errorf("Scroll: %v", err)
	}
	if err := s.Wait("h1", "", 0, false); err != nil {
		t.Errorf("Wait selector: %v", err)
	}
	if err := s.Wait("", "", 30, false); err != nil {
		t.Errorf("Wait ms: %v", err)
	}
	if err := s.Press("Tab"); err != nil {
		t.Errorf("Press: %v", err)
	}
	if err := s.TypeText("x"); err != nil {
		t.Errorf("TypeText: %v", err)
	}

	// fill
	nameRef := refByName(els, "yourname")
	if nameRef == "" {
		t.Fatal("name input not found in scan")
	}
	if err := s.Fill(nameRef, "alice"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if v := s.page.MustEval(`() => document.getElementById('name').value`).Str(); v != "alice" {
		t.Errorf("fill value = %q, want alice", v)
	}

	// upload
	srcFile := filepath.Join(tmp, "up.txt")
	if err := os.WriteFile(srcFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Upload(upRef, srcFile); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if n := s.page.MustEval(`() => document.getElementById('f').files.length`).Int(); n != 1 {
		t.Errorf("file input files = %d, want 1", n)
	}

	// download
	saved, err := s.Download(dlRef, "")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if data, err := os.ReadFile(saved); err != nil {
		t.Fatalf("read saved download: %v", err)
	} else if string(data) != "hello-download" {
		t.Errorf("download content = %q, want hello-download", string(data))
	}

	// needs_file: clicking the "Choose File" button opens a chooser
	chooseRef := refByName(els, "Choose File")
	if chooseRef == "" {
		t.Fatal("choose-file button not found")
	}
	if cr, err := s.Click(chooseRef); err != nil {
		t.Fatalf("Click(choose): %v", err)
	} else if !cr.NeedsFile {
		t.Error("click on file-chooser button did not report needs_file")
	}

	// navigation (last — it leaves the page)
	nextRef := refByName(els, "Go Next")
	if nextRef == "" {
		t.Fatal("link not found")
	}
	if cr, err := s.Click(nextRef); err != nil {
		t.Fatalf("Click(link): %v", err)
	} else if !cr.Navigated || !strings.Contains(cr.URL, "/next") {
		t.Errorf("click link: navigated=%v url=%q", cr.Navigated, cr.URL)
	}

	// back / forward / reload
	if err := s.Back(); err != nil {
		t.Fatalf("Back: %v", err)
	}
	if u, _ := s.Status(); strings.Contains(u, "/next") {
		t.Errorf("Back did not leave /next: %s", u)
	}
	if err := s.Forward(); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if u, _ := s.Status(); !strings.Contains(u, "/next") {
		t.Errorf("Forward did not return to /next: %s", u)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

// TestSessionTier2 covers state save/load, dialog auto-handling, and tabs.
func TestSessionTier2(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a browser; skipped in -short")
	}
	ts := testServer()
	defer ts.Close()

	s, err := New(ts.URL, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// state: restore a cookie, then export it back
	if err := s.SetState(State{Cookies: []*proto.NetworkCookieParam{
		{Name: "wr", Value: "42", Domain: hostOf(ts.URL), Path: "/"},
	}}); err != nil {
		t.Fatalf("SetState cookie: %v", err)
	}
	st, err := s.GetState()
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	found := false
	for _, c := range st.Cookies {
		if c.Name == "wr" && c.Value == "42" {
			found = true
		}
	}
	if !found {
		t.Error("cookie not exported after SetState")
	}

	// localStorage round-trip (current origin)
	if err := s.SetState(State{Local: map[string]string{"k": "v"}}); err != nil {
		t.Fatalf("SetState local: %v", err)
	}
	if st2, _ := s.GetState(); st2.Local["k"] != "v" {
		t.Errorf("localStorage not restored: %v", st2.Local)
	}

	// dialogs: auto-accept must not hang on alert()
	s.SetDialog(true, "")
	done := make(chan error, 1)
	go func() {
		_, e := s.page.Timeout(5 * time.Second).Eval(`() => { alert('hi'); return 1 }`)
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Errorf("alert handling failed: %v", e)
		}
	case <-time.After(8 * time.Second):
		t.Error("alert hung — dialog not auto-handled")
	}
}
