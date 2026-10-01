// Package roster stores the devices this node has paired with.
package roster

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/state"
	"messh/internal/wol"
)

const maxAddrs = 4

// Peer is a paired device.
type Peer struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Addrs        []string    `json:"addrs"` // host:port, most recent first
	PairedAt     time.Time   `json:"paired_at"`
	LastSeen     time.Time   `json:"last_seen,omitzero"`
	Tools        []*mcp.Tool `json:"tools,omitempty"` // last tool list fetched from the peer
	ToolsFetched time.Time   `json:"tools_fetched,omitzero"`

	// Wake lists the peer's wakeable NICs, learned over the authenticated
	// mesh (never from discovery packets); WakeFetched is when.
	Wake        []wol.Target `json:"wake,omitempty"`
	WakeFetched time.Time    `json:"wake_fetched,omitzero"`

	// Asleep is when the peer last said it was going to sleep. It holds
	// until the peer is heard from again; zero means no such notice.
	Asleep time.Time `json:"asleep,omitzero"`

	// AutoWake wakes the peer before a tool call when it is asleep or offline.
	AutoWake bool `json:"auto_wake,omitempty"`
}

type file struct {
	Peers []*Peer `json:"peers"`
}

// Roster is the persisted, concurrency-safe set of paired peers.
type Roster struct {
	path string

	mu        sync.Mutex
	peers     map[string]*Peer
	listeners []func()
}

// Load reads the roster at path; a missing file yields an empty roster.
func Load(path string) (*Roster, error) {
	r := &Roster{path: path, peers: map[string]*Peer{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	for _, p := range f.Peers {
		r.peers[p.ID] = p
	}
	return r, nil
}

// OnChange registers fn to run (without the lock held) after membership,
// names, or tool lists change.
func (r *Roster) OnChange(fn func()) {
	r.mu.Lock()
	r.listeners = append(r.listeners, fn)
	r.mu.Unlock()
}

// Get returns a copy of the peer with the given ID.
func (r *Roster) Get(id string) (Peer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if !ok {
		return Peer{}, false
	}
	return clone(p), true
}

// List returns copies of all peers sorted by name, then ID.
func (r *Roster) List() []Peer {
	r.mu.Lock()
	out := make([]Peer, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, clone(p))
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b Peer) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Pair adds or refreshes a peer after a successful pairing.
func (r *Roster) Pair(id, name, addr string, now time.Time) error {
	r.mu.Lock()
	p, ok := r.peers[id]
	if !ok {
		p = &Peer{ID: id}
		r.peers[id] = p
	}
	p.Name = name
	p.PairedAt = now
	p.LastSeen = now
	p.Addrs = pushAddr(p.Addrs, addr)
	return r.saveAndNotify()
}

// Remove unpairs a peer. It reports whether the peer existed.
func (r *Roster) Remove(id string) (bool, error) {
	r.mu.Lock()
	if _, ok := r.peers[id]; !ok {
		r.mu.Unlock()
		return false, nil
	}
	delete(r.peers, id)
	return true, r.saveAndNotify()
}

// Seen records contact with a peer at addr. It persists only when the address
// list changes, so frequent sightings do not rewrite the file.
func (r *Roster) Seen(id, addr string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if !ok {
		return nil
	}
	if at.After(p.LastSeen) {
		p.LastSeen = at
	}
	if addr == "" || (len(p.Addrs) > 0 && p.Addrs[0] == addr) {
		return nil
	}
	p.Addrs = pushAddr(p.Addrs, addr)
	return r.save()
}

// SetTools stores the tool list most recently fetched from a peer.
func (r *Roster) SetTools(id string, tools []*mcp.Tool, at time.Time) error {
	r.mu.Lock()
	p, ok := r.peers[id]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	p.Tools = tools
	p.ToolsFetched = at
	return r.saveAndNotify()
}

// SetWake stores the wakeable interfaces most recently fetched from a peer.
// Gateway listeners are not notified: nothing agents see depends on it.
func (r *Roster) SetWake(id string, targets []wol.Target, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if !ok {
		return nil
	}
	p.Wake = targets
	p.WakeFetched = at
	return r.save()
}

// SetAsleep records that a peer announced it is going to sleep at at; a zero
// time clears the notice. It writes the file only on change.
func (r *Roster) SetAsleep(id string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if !ok || p.Asleep.Equal(at) {
		return nil
	}
	p.Asleep = at
	return r.save()
}

// SetAutoWake turns waking the peer before tool calls on or off. It reports
// whether the peer exists.
func (r *Roster) SetAutoWake(id string, on bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if !ok {
		return false, nil
	}
	if p.AutoWake == on {
		return true, nil
	}
	p.AutoWake = on
	return true, r.save()
}

// saveAndNotify persists, releases the lock, then runs listeners.
func (r *Roster) saveAndNotify() error {
	err := r.save()
	listeners := slices.Clone(r.listeners)
	r.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
	return err
}

func (r *Roster) save() error {
	f := file{Peers: make([]*Peer, 0, len(r.peers))}
	for _, p := range r.peers {
		f.Peers = append(f.Peers, p)
	}
	slices.SortFunc(f.Peers, func(a, b *Peer) int { return strings.Compare(a.ID, b.ID) })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(r.path, append(data, '\n'), 0o600)
}

func pushAddr(addrs []string, addr string) []string {
	if addr == "" {
		return addrs
	}
	out := []string{addr}
	for _, a := range addrs {
		if a != addr && len(out) < maxAddrs {
			out = append(out, a)
		}
	}
	return out
}

func clone(p *Peer) Peer {
	c := *p
	c.Addrs = slices.Clone(p.Addrs)
	c.Tools = slices.Clone(p.Tools)
	c.Wake = slices.Clone(p.Wake) // replaced wholesale by SetWake, never mutated in place
	return c
}
