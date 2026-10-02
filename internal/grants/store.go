package grants

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"messh/internal/files"
	"messh/internal/state"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid capability grant")
	ErrNotFound = errors.New("capability grant not found")
)

type Subject struct {
	DeviceID string `json:"device_id"`
	Agent    string `json:"agent"`
}
type Grant struct {
	ID        string     `json:"id"`
	Subject   Subject    `json:"subject"`
	Kind      string     `json:"kind"`
	Tool      string     `json:"tool,omitempty"`
	ArgsHash  string     `json:"args_hash,omitempty"`
	Path      string     `json:"path,omitempty"`
	Actions   []string   `json:"actions"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}
type Request struct {
	Kind     string `json:"kind"`
	Tool     string `json:"tool,omitempty"`
	ArgsHash string `json:"args_hash,omitempty"`
	Path     string `json:"path,omitempty"`
	Action   string `json:"action,omitempty"`
}
type Decision struct {
	Allowed   bool      `json:"allowed"`
	Code      string    `json:"code"`
	GrantID   string    `json:"grant_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}
type document struct {
	Version int     `json:"version"`
	Grants  []Grant `json:"grants"`
}
type Store struct {
	mu     sync.RWMutex
	file   string
	grants []Grant
	now    func() time.Time
}

func New(file string) (*Store, error) {
	s := &Store{file: file, now: time.Now}
	b, e := os.ReadFile(file)
	if errors.Is(e, os.ErrNotExist) {
		return s, nil
	}
	if e != nil {
		return nil, e
	}
	var d document
	if e = json.Unmarshal(b, &d); e != nil {
		return nil, fmt.Errorf("decode grants: %w", e)
	}
	if d.Version != 1 {
		return nil, fmt.Errorf("unsupported grants version %d", d.Version)
	}
	for _, g := range d.Grants {
		if validate(g) != nil {
			return nil, fmt.Errorf("invalid persisted capability grant %q", g.ID)
		}
	}
	s.grants = d.Grants
	return s, nil
}
func validate(g Grant) error {
	if g.ID == "" || g.Subject.DeviceID == "" || g.Subject.Agent == "" || g.CreatedAt.IsZero() || g.ExpiresAt.IsZero() || !g.ExpiresAt.After(g.CreatedAt) {
		return ErrInvalid
	}
	if g.Kind == "tool" {
		if g.Tool == "" || g.ArgsHash == "" || g.Path != "" || len(g.Actions) != 1 || g.Actions[0] != "invoke" {
			return ErrInvalid
		}
	} else if g.Kind == "file" {
		if g.Tool != "" || g.ArgsHash != "" || g.Path == "" {
			return ErrInvalid
		}
		r, e := files.ParseRef(g.Path)
		if e != nil || string(r) != g.Path {
			return ErrInvalid
		}
		for _, a := range g.Actions {
			if a != "read" && a != "write" {
				return ErrInvalid
			}
		}
		if len(g.Actions) == 0 {
			return ErrInvalid
		}
		if strings.HasPrefix(g.Path, "artifacts/") {
			for _, a := range g.Actions {
				if a == "write" {
					return ErrInvalid
				}
			}
		}
	} else {
		return ErrInvalid
	}
	return nil
}
func (s *Store) Create(g Grant) (Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g.CreatedAt.IsZero() {
		g.CreatedAt = s.now().UTC()
	}
	if g.ID == "" {
		var b [16]byte
		if _, e := rand.Read(b[:]); e != nil {
			return Grant{}, e
		}
		g.ID = hex.EncodeToString(b[:])
	}
	if e := validate(g); e != nil {
		return Grant{}, e
	}
	for _, x := range s.grants {
		if x.ID == g.ID {
			return Grant{}, fmt.Errorf("duplicate grant id")
		}
	}
	g.Actions = append([]string(nil), g.Actions...)
	s.grants = append(s.grants, g)
	if e := s.save(); e != nil {
		s.grants = s.grants[:len(s.grants)-1]
		return Grant{}, e
	}
	return g, nil
}
func (s *Store) List() []Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Grant, len(s.grants))
	copy(out, s.grants)
	for i := range out {
		out[i].Actions = append([]string(nil), out[i].Actions...)
	}
	return out
}
func (s *Store) Revoke(id string) (Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.grants {
		if s.grants[i].ID == id {
			if s.grants[i].RevokedAt == nil {
				t := s.now().UTC()
				s.grants[i].RevokedAt = &t
				if e := s.save(); e != nil {
					s.grants[i].RevokedAt = nil
					return Grant{}, e
				}
			}
			return s.grants[i], nil
		}
	}
	return Grant{}, ErrNotFound
}
func (s *Store) Decide(sub Subject, r Request) Decision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()
	var expired, revoked bool
	for i := range s.grants {
		g := &s.grants[i]
		if g.Subject != sub || !matches(*g, r) {
			continue
		}
		if g.RevokedAt != nil {
			revoked = true
			continue
		}
		if !now.Before(g.ExpiresAt) {
			expired = true
			continue
		}
		return Decision{Allowed: true, Code: "allowed", GrantID: g.ID, ExpiresAt: g.ExpiresAt}
	}
	if revoked {
		return Decision{Code: "grant_revoked"}
	}
	if expired {
		return Decision{Code: "grant_expired"}
	}
	return Decision{Code: "no_matching_grant"}
}
func matches(g Grant, r Request) bool {
	if g.Kind != r.Kind {
		return false
	}
	if r.Kind == "tool" {
		return g.Tool == r.Tool && g.ArgsHash == r.ArgsHash && contains(g.Actions, "invoke")
	}
	return (g.Path == r.Path || strings.HasPrefix(r.Path, g.Path+"/")) && contains(g.Actions, r.Action)
}
func contains(a []string, v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}
func (s *Store) save() error {
	b, e := json.MarshalIndent(document{Version: 1, Grants: s.grants}, "", "  ")
	if e != nil {
		return e
	}
	return state.WriteFileAtomic(s.file, append(b, '\n'), 0o600)
}
