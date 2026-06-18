package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:10000": true,
		"localhost:10000":  true,
		"[::1]:10000":      true,
		"127.0.0.1":        true,
		"localhost":        true,
		"evil.com:10000":   false,
		"example.com":      false,
		"10.0.0.5:10000":   false,
	}
	for host, want := range cases {
		if got := localHost(host); got != want {
			t.Errorf("localHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestListenAutoIncrement(t *testing.T) {
	// Occupy a port, then ask listen() for it — it should pick the next free one.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	ln, chosen, err := listen(port)
	if err != nil {
		t.Fatalf("listen(%d): %v", port, err)
	}
	defer ln.Close()
	if chosen == port {
		t.Errorf("listen(%d) returned the busy port", port)
	}
	if chosen < port {
		t.Errorf("listen(%d) = %d, want >= %d", port, chosen, port)
	}
}

func TestListenOSAssigned(t *testing.T) {
	ln, chosen, err := listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if chosen <= 0 {
		t.Errorf("listen(0) chose port %d", chosen)
	}
}

func TestSecureAndMethod(t *testing.T) {
	s := &Server{}
	h := s.route(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	call := func(method, host, secFetch string) int {
		req := httptest.NewRequest(method, "http://"+host+"/click", nil)
		req.Host = host
		if secFetch != "" {
			req.Header.Set("Sec-Fetch-Site", secFetch)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	if code := call(http.MethodPost, "localhost:10000", ""); code != http.StatusOK {
		t.Errorf("local POST (curl-like) = %d, want 200", code)
	}
	if code := call(http.MethodPost, "localhost:10000", "same-origin"); code != http.StatusOK {
		t.Errorf("same-origin POST = %d, want 200", code)
	}
	if code := call(http.MethodPost, "evil.com:10000", ""); code != http.StatusForbidden {
		t.Errorf("non-local host = %d, want 403 (DNS rebinding guard)", code)
	}
	if code := call(http.MethodPost, "localhost:10000", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403 (CSRF guard)", code)
	}
	if code := call(http.MethodGet, "localhost:10000", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method = %d, want 405", code)
	}
}
