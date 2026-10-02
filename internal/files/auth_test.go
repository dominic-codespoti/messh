package files

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthorizedHandlerDeniesDirectAccessAndFiltersListing(t *testing.T) {
	s, _ := newTestStore(t)
	put(t, s, "ws/project/secret.txt", []byte("secret"), false)
	put(t, s, "ws/other/public.txt", []byte("public"), false)
	allowed := func(_ *http.Request, ref, action string) (bool, string) {
		return action == "read" && (ref == "ws/project" || strings.HasPrefix(ref, "ws/project/")), "no_matching_grant"
	}
	visible := func(_ *http.Request, requested, entry, action string) bool {
		return action == "read" && (entry == "ws/project" || strings.HasPrefix(entry, "ws/project/"))
	}
	h := NewAuthorizedHandler(s, allowed, visible)
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, Path+"/ws/other/public.txt", nil))
	if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), `"code":"no_matching_grant"`) {
		t.Fatalf("ungranted direct read = %d %s", denied.Code, denied.Body.String())
	}
	listed := httptest.NewRecorder()
	h.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, Path+"?ref=ws&recursive=1", nil))
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "ws/project/secret.txt") || strings.Contains(listed.Body.String(), "ws/other/public.txt") {
		t.Fatalf("filtered listing = %d %s", listed.Code, listed.Body.String())
	}
	granted := httptest.NewRecorder()
	h.ServeHTTP(granted, httptest.NewRequest(http.MethodGet, Path+"/ws/project/secret.txt", nil))
	if granted.Code != http.StatusOK || granted.Body.String() != "secret" {
		t.Fatalf("granted direct read = %d %q", granted.Code, granted.Body.String())
	}
}
