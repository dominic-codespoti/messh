package grants

import (
	"path/filepath"
	"testing"
	"time"
)

func TestScopedExpiryRevocationSurvivesReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "grants.json")
	s, e := New(p)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	g, e := s.Create(Grant{Subject: Subject{"device-A", "agent"}, Kind: "file", Path: "ws/project", Actions: []string{"read"}, ExpiresAt: now.Add(time.Hour)})
	if e != nil {
		t.Fatal(e)
	}
	sub := Subject{"device-A", "agent"}
	if d := s.Decide(sub, Request{Kind: "file", Path: "ws/project/file", Action: "read"}); !d.Allowed {
		t.Fatalf("expected allowed, got %+v", d)
	}
	if d := s.Decide(sub, Request{Kind: "file", Path: "ws/project2/file", Action: "read"}); d.Allowed {
		t.Fatal("segment boundary escaped")
	}
	if d := s.Decide(Subject{"device-B", "agent"}, Request{Kind: "file", Path: "ws/project/file", Action: "read"}); d.Allowed {
		t.Fatal("wrong subject allowed")
	}
	now = now.Add(2 * time.Hour)
	if d := s.Decide(sub, Request{Kind: "file", Path: "ws/project/file", Action: "read"}); d.Code != "grant_expired" {
		t.Fatalf("got %+v", d)
	}
	if _, e = s.Revoke(g.ID); e != nil {
		t.Fatal(e)
	}
	s2, e := New(p)
	if e != nil {
		t.Fatal(e)
	}
	s2.now = func() time.Time { return now.Add(-3 * time.Hour) }
	if d := s2.Decide(sub, Request{Kind: "file", Path: "ws/project/file", Action: "read"}); d.Code != "grant_revoked" {
		t.Fatalf("revoke not durable: %+v", d)
	}
}
