package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDownloadsRejectCrossOriginRedirect(t *testing.T) {
	var contacted atomic.Bool
	untrusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Store(true)
		_, _ = w.Write([]byte("unexpected"))
	}))
	defer untrusted.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, untrusted.URL+"/archive", http.StatusFound)
	}))
	defer origin.Close()
	client := &Client{HTTP: origin.Client(), APIBase: origin.URL, Repository: "test"}
	if _, _, err := client.bytes(context.Background(), origin.URL+"/archive", 100, false); err == nil {
		t.Fatal("accepted untrusted download redirect")
	}
	if contacted.Load() {
		t.Fatal("sent request to untrusted redirect target")
	}
}

func TestOversizedMetadataRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flush first so there is no Content-Length shortcut: exercise the body bound.
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(strings.Repeat("x", int(maxMetadataSize)+1)))
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), APIBase: server.URL, Repository: "test"}
	if _, _, err := client.bytes(context.Background(), server.URL, maxMetadataSize, false); err == nil {
		t.Fatal("accepted oversized streamed metadata")
	}
}
