package grants

import "strings"

// CanSee reports whether a listed entry is inside a live read scope or is an ancestor needed to navigate to one.
func (s *Store) CanSee(sub Subject, requested, entry, action string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()
	for _, g := range s.grants {
		if g.Kind != "file" || g.Subject != sub || g.RevokedAt != nil || !now.Before(g.ExpiresAt) || !contains(g.Actions, action) {
			continue
		}
		if g.Path == entry || strings.HasPrefix(entry, g.Path+"/") || strings.HasPrefix(g.Path, entry+"/") {
			if requested == "" || requested == g.Path || requested == entry || strings.HasPrefix(requested, g.Path+"/") || strings.HasPrefix(g.Path, requested+"/") {
				return true
			}
		}
	}
	return false
}
