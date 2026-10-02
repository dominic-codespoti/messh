package files

import (
	"encoding/json"
	"net/http"
	"strings"
)

type Authorize func(*http.Request, string, string) (bool, string)
type CanSee func(*http.Request, string, string, string) bool

func NewAuthorizedHandler(s *Store, authorize Authorize, canSee CanSee) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref := ""
		action := "read"
		list := r.URL.Path == Path
		if list {
			ref = r.URL.Query().Get("ref")
			if r.URL.Query().Get("stat") == "1" {
				list = false
			}
		} else {
			ref = strings.TrimPrefix(r.URL.Path, Path+"/")
			if r.Method == http.MethodPut || r.Method == http.MethodPost {
				action = "write"
			}
		}
		allowed, code := false, "no_matching_grant"
		if authorize != nil {
			allowed, code = authorize(r, ref, action)
		}
		if list && !allowed && canSee != nil {
			var l Listing
			var e error
			if r.URL.Query().Get("recursive") == "1" {
				l, e = s.Walk(ref)
			} else {
				l, e = s.List(ref)
			}
			if e != nil {
				writeFail(w, r, e)
				return
			}
			visible := make([]Info, 0, len(l.Entries))
			for _, entry := range l.Entries {
				if canSee(r, ref, entry.Ref, "read") {
					visible = append(visible, entry)
				}
			}
			l.Entries = visible
			l.Skipped = 0
			l.Truncated = false
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(l)
			return
		}
		if !allowed {
			writeErr(w, r, http.StatusForbidden, code, &CapabilityDeniedError{Code: code})
			return
		}
		NewHandler(s).ServeHTTP(w, r)
	})
}
