// Package server exposes a browser.Session over a small JSON/HTTP API on a
// local port, plus an embedded Swagger UI at /. One webrudder process = one
// browser = one port.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	httpSwagger "github.com/swaggo/http-swagger/v2"

	"github.com/0xRuangsak/webrudder/internal/browser"

	_ "github.com/0xRuangsak/webrudder/docs" // generated OpenAPI spec (swag init)
)

type Server struct {
	sess     *browser.Session
	port     int
	quit     chan struct{}
	quitOnce sync.Once
}

// Run binds a port (auto-incrementing if busy), serves the API, and shuts down
// cleanly on SIGINT/SIGTERM or a /shutdown request.
func Run(sess *browser.Session, port int) error {
	ln, chosen, err := listen(port)
	if err != nil {
		return err
	}
	s := &Server{sess: sess, port: chosen, quit: make(chan struct{})}

	mux := http.NewServeMux()
	s.routes(mux)
	httpSrv := &http.Server{Handler: mux}

	fmt.Printf("webrudder · http://localhost:%d · ctrl-c to quit\n", chosen)

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sig:
	case <-s.quit:
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	return nil
}

// listen returns a TCP listener on 127.0.0.1. port 0 lets the OS choose; a
// fixed port auto-increments through the next 50 if busy.
func listen(port int) (net.Listener, int, error) {
	if port == 0 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, 0, err
		}
		return ln, ln.Addr().(*net.TCPAddr).Port, nil
	}
	for p := port; p < port+50; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return ln, p, nil
		}
	}
	return nil, 0, fmt.Errorf("no free port in %d..%d", port, port+49)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/status", s.route(http.MethodGet, s.handleStatus))
	mux.HandleFunc("/scan", s.route(http.MethodGet, s.handleScan))
	mux.HandleFunc("/read", s.route(http.MethodGet, s.handleRead))
	mux.HandleFunc("/snap", s.route(http.MethodGet, s.handleSnap))
	mux.HandleFunc("/goto", s.route(http.MethodPost, s.handleGoto))
	mux.HandleFunc("/click", s.route(http.MethodPost, s.handleClick))
	mux.HandleFunc("/fill", s.route(http.MethodPost, s.handleFill))
	mux.HandleFunc("/upload", s.route(http.MethodPost, s.handleUpload))
	mux.HandleFunc("/download", s.route(http.MethodPost, s.handleDownload))
	mux.HandleFunc("/batch", s.route(http.MethodPost, s.handleBatch))
	mux.HandleFunc("/shutdown", s.route(http.MethodPost, s.handleShutdown))
	mux.Handle("/swagger/", s.secure(httpSwagger.Handler(httpSwagger.URL("/swagger/doc.json"))))
	mux.HandleFunc("/", s.secure(s.handleRoot))
}

// secure rejects requests that aren't local (DNS-rebinding guard) or are
// cross-site browser requests (CSRF guard). Non-browser clients (curl, agents,
// the Swagger UI itself) send no cross-site Sec-Fetch-Site header and pass
// through, so the JSON API UX is unchanged.
func (s *Server) secure(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !localHost(r.Host) {
			http.Error(w, "forbidden: non-local host", http.StatusForbidden)
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
			h(w, r)
		default:
			http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
		}
	}
}

// route wraps secure and pins the HTTP method.
func (s *Server) route(method string, h http.HandlerFunc) http.HandlerFunc {
	return s.secure(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	})
}

// localHost reports whether the Host header points at the loopback interface.
func localHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// --- handlers ---

// handleStatus reports the current page.
// @Summary  Current URL, title, and port
// @Tags     read
// @Produce  json
// @Success  200 {object} StatusResp
// @Router   /status [get]
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	u, t := s.sess.Status()
	writeJSON(w, http.StatusOK, StatusResp{URL: u, Title: t, Port: s.port})
}

// handleScan lists actionable elements.
// @Summary  List actionable elements (assigns refs e1, e2, …)
// @Tags     read
// @Produce  json
// @Success  200 {object} ScanResp
// @Failure  500 {object} ErrResp
// @Router   /scan [get]
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	els, err := s.sess.Scan()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ScanResp{Elements: els})
}

// handleRead returns the page text.
// @Summary  Extract page URL, title, and visible text
// @Tags     read
// @Produce  json
// @Success  200 {object} ReadResp
// @Failure  500 {object} ErrResp
// @Router   /read [get]
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	u, t, text, err := s.sess.Read()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ReadResp{URL: u, Title: t, Text: text})
}

// handleSnap returns a screenshot.
// @Summary  Screenshot the page (PNG, full page by default)
// @Tags     read
// @Produce  png
// @Param    full query boolean false "full scrollable page (default true); false = viewport only"
// @Success  200 {file} binary
// @Failure  500 {object} ErrResp
// @Router   /snap [get]
func (s *Server) handleSnap(w http.ResponseWriter, r *http.Request) {
	full := true
	if v := r.URL.Query().Get("full"); v == "false" || v == "0" {
		full = false
	}
	png, err := s.sess.Snap(full)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

// handleGoto navigates to a URL.
// @Summary  Navigate to an absolute URL (explicit jump)
// @Tags     interact
// @Accept   json
// @Produce  json
// @Param    body body GotoReq true "target url"
// @Success  200 {object} GotoResp
// @Failure  400 {object} ErrResp
// @Router   /goto [post]
func (s *Server) handleGoto(w http.ResponseWriter, r *http.Request) {
	var b GotoReq
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sess.Goto(b.URL); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	u, _ := s.sess.Status()
	writeJSON(w, http.StatusOK, GotoResp{OK: true, URL: u})
}

// handleClick clicks an element.
// @Summary  Click an element by ref
// @Tags     interact
// @Accept   json
// @Produce  json
// @Param    body body ClickReq true "element ref"
// @Success  200 {object} ClickResp
// @Failure  400 {object} ErrResp
// @Router   /click [post]
func (s *Server) handleClick(w http.ResponseWriter, r *http.Request) {
	var b ClickReq
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	cr, err := s.sess.Click(b.Ref)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if cr.NeedsFile {
		writeJSON(w, http.StatusOK, ClickResp{NeedsFile: true})
		return
	}
	res := ClickResp{OK: true, Navigated: cr.Navigated}
	if cr.Navigated {
		res.URL = cr.URL
	}
	if cr.Downloaded != "" {
		res.Downloaded = cr.Downloaded
	}
	writeJSON(w, http.StatusOK, res)
}

// handleFill types into an input.
// @Summary  Type text into an input/textarea by ref
// @Tags     interact
// @Accept   json
// @Produce  json
// @Param    body body FillReq true "ref and text"
// @Success  200 {object} OKResp
// @Failure  400 {object} ErrResp
// @Router   /fill [post]
func (s *Server) handleFill(w http.ResponseWriter, r *http.Request) {
	var b FillReq
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sess.Fill(b.Ref, b.Text); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, OKResp{OK: true})
}

// handleUpload attaches a file to a file input.
// @Summary  Attach a local file to a file input by ref
// @Tags     interact
// @Accept   json
// @Produce  json
// @Param    body body UploadReq true "ref and local file path"
// @Success  200 {object} OKResp
// @Failure  400 {object} ErrResp
// @Router   /upload [post]
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	var b UploadReq
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sess.Upload(b.Ref, b.File); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, OKResp{OK: true})
}

// handleDownload clicks and captures a download.
// @Summary  Click an element, wait for the download, return the saved path
// @Tags     interact
// @Accept   json
// @Produce  json
// @Param    body body DownloadReq true "ref and optional dir"
// @Success  200 {object} DownloadResp
// @Failure  400 {object} ErrResp
// @Router   /download [post]
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	var b DownloadReq
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	saved, err := s.sess.Download(b.Ref, b.Dir)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, DownloadResp{OK: true, Saved: saved})
}

// handleBatch runs several actions in one request.
// @Summary  Run multiple actions in one request
// @Tags     interact
// @Accept   json
// @Produce  json
// @Param    body body BatchReq true "ordered actions"
// @Success  200 {object} BatchResp
// @Failure  400 {object} ErrResp
// @Router   /batch [post]
func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var b BatchReq
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	results := s.sess.Batch(b.Actions)
	writeJSON(w, http.StatusOK, BatchResp{OK: true, Results: results})
}

// handleShutdown stops the daemon.
// @Summary  Stop the daemon and browser
// @Tags     control
// @Produce  json
// @Success  200 {object} OKResp
// @Router   /shutdown [post]
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, OKResp{OK: true})
	s.quitOnce.Do(func() { close(s.quit) })
}

// handleRoot redirects to the embedded Swagger UI.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/swagger/index.html", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}
