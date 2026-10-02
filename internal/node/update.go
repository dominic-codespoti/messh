package node

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"messh/internal/control"
	"messh/internal/state"
)

const updateLeaseDuration = state.UpdateStartupDuration

type maintenanceState struct {
	mu       sync.Mutex
	active   int
	lease    string
	expires  time.Time
	stopping bool
	paths    state.Paths
	startup  *state.UpdateStartup
}

// expireLocked is also called by admission, so a lost updater never needs a
// timer or another control request to restore service.
func (m *maintenanceState) expireLocked(now time.Time) {
	if !m.stopping && m.lease != "" && !now.Before(m.expires) {
		if m.startup != nil {
			if err := m.paths.RemoveUpdateStartup(*m.startup); err != nil {
				// Never release admission for a changed transaction. Transient
				// lock failures are retried by the next admission attempt.
				return
			}
			m.startup = nil
		}
		m.lease = ""
		m.expires = time.Time{}
	}
}

// loadStartupUpdate runs before providers, schedulers or HTTP serving starts.
// A valid expired transaction is safe to clear; a foreign or malformed one is
// a startup error, not implicit permission to run jobs.
func loadStartupUpdate(paths state.Paths, id, executable string, now time.Time) (*state.UpdateStartup, error) {
	marker, err := paths.LoadUpdateStartup()
	if err != nil || marker == nil {
		return marker, err
	}
	if err := marker.Validate(id, executable, now); err != nil {
		if !errors.Is(err, state.ErrUpdateStartupExpired) {
			return nil, err
		}
		if err := paths.RemoveUpdateStartup(*marker); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return marker, nil
}

func (n *Node) beginWork() bool {
	m := &n.maintenance
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(time.Now())
	if m.lease != "" || m.stopping {
		return false
	}
	m.active++
	return true
}

func (n *Node) endWork() {
	n.maintenance.mu.Lock()
	n.maintenance.active--
	n.maintenance.mu.Unlock()
}

// admitHTTP accounts for the entire request, including streamed transfers and
// model responses. MCP GET is an idle server-push channel, not a tool call;
// actual tools are admitted separately by dispatch/Call and MCP POST. No
// ResponseWriter wrapping is needed, preserving streaming and flushing.
func (n *Node) admitHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/update/prepare" || r.URL.Path == "/v1/update/stop" ||
			r.Method == http.MethodGet && (r.URL.Path == "/v1/status" || r.URL.Path == "/mcp") ||
			r.URL.Path == "/v1/tools-changed" {
			next.ServeHTTP(w, r)
			return
		}
		if !n.beginWork() {
			writeError(w, http.StatusServiceUnavailable, "node is preparing for an update")
			return
		}
		defer n.endWork()
		next.ServeHTTP(w, r)
	})
}

func (n *Node) registerUpdateAPI(api *http.ServeMux) {
	api.HandleFunc("POST /v1/update/prepare", n.apiUpdatePrepare)
	api.HandleFunc("DELETE /v1/update/prepare", n.apiUpdateAbort)
	api.HandleFunc("POST /v1/update/stop", n.apiUpdateStop)
}

func (n *Node) prepareUpdate(now time.Time) (control.UpdatePreparation, error) {
	m := &n.maintenance
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	if m.lease != "" || m.stopping {
		return control.UpdatePreparation{}, fmt.Errorf("node already has an update lease")
	}
	if m.active != 0 {
		return control.UpdatePreparation{}, fmt.Errorf("node is busy: %d active requests or scheduled calls", m.active)
	}
	// Admission and the idle check share m.mu. Every submit remains admitted
	// until its job record is committed, so no job can slip between this count
	// and the lease. Counts are intentionally independent of caller identity.
	n.toolsMu.RLock()
	defer n.toolsMu.RUnlock()
	for _, p := range n.providers {
		if jobs, ok := p.(interface{ ActiveCount() int }); ok {
			if count := jobs.ActiveCount(); count != 0 {
				return control.UpdatePreparation{}, fmt.Errorf("node is busy: %d nonterminal jobs", count)
			}
		}
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return control.UpdatePreparation{}, fmt.Errorf("create update lease: %w", err)
	}
	m.lease = hex.EncodeToString(token[:])
	m.expires = now.Add(updateLeaseDuration)
	preparation := control.UpdatePreparation{Lease: m.lease, Expires: m.expires, ID: n.id.ID, Executable: n.executable}
	if n.paths.Root != "" {
		m.paths = n.paths
		m.startup = &preparation
	}
	return preparation, nil
}

func (n *Node) apiUpdatePrepare(w http.ResponseWriter, r *http.Request) {
	result, err := n.prepareUpdate(time.Now())
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (n *Node) consumeUpdateLease(lease string, stop bool, now time.Time) bool {
	m := &n.maintenance
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(now)
	if m.stopping || m.lease == "" || subtle.ConstantTimeCompare([]byte(lease), []byte(m.lease)) != 1 {
		return false
	}
	if stop {
		if m.startup != nil {
			actual, err := m.paths.LoadUpdateStartup()
			if err != nil || actual != nil && (actual.Lease != m.startup.Lease ||
				actual.ID != m.startup.ID || actual.Executable != m.startup.Executable ||
				!actual.Expires.Equal(m.startup.Expires)) {
				return false
			}
		}
		// Once stopping, expiry must never reopen admission before shutdown.
		m.stopping = true
	} else {
		if m.startup != nil {
			if err := m.paths.RemoveUpdateStartup(*m.startup); err != nil {
				return false
			}
			m.startup = nil
		}
		m.lease = ""
		m.expires = time.Time{}
	}
	return true
}

func (n *Node) apiUpdateAbort(w http.ResponseWriter, r *http.Request) {
	var req control.UpdateLeaseRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !n.consumeUpdateLease(req.Lease, false, time.Now()) {
		writeError(w, http.StatusConflict, "invalid or expired update lease")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (n *Node) apiUpdateStop(w http.ResponseWriter, r *http.Request) {
	var req control.UpdateLeaseRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !n.consumeUpdateLease(req.Lease, true, time.Now()) {
		writeError(w, http.StatusConflict, "invalid or expired update lease")
		return
	}
	// A complete fixed-length response is flushed before cancellation. This is
	// important on Windows, where the process may exit as soon as shutdown ends.
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
	_ = http.NewResponseController(w).Flush()
	n.cancel()
}
