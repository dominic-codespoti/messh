package node

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"messh/internal/control"
	"messh/internal/roster"
)

// cliAgent is the agent name recorded for calls made with the control token.
const cliAgent = "cli"

func (n *Node) localHandler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /v1/status", n.apiStatus)
	api.HandleFunc("GET /v1/peers", n.apiPeers)
	api.HandleFunc("DELETE /v1/peers/{peer}", n.apiUnpair)
	api.HandleFunc("GET /v1/discovered", n.apiDiscovered)
	api.HandleFunc("POST /v1/pair/accept", n.apiPairOpen)
	api.HandleFunc("DELETE /v1/pair/accept", n.apiPairClose)
	api.HandleFunc("GET /v1/pair/incoming", n.apiIncoming)
	api.HandleFunc("POST /v1/pair/incoming/{id}", n.apiDecide)
	api.HandleFunc("POST /v1/pair/outgoing", n.apiPairStart)
	api.HandleFunc("GET /v1/pair/outgoing/{id}", n.apiPairGet)
	api.HandleFunc("POST /v1/pair/outgoing/{id}", n.apiPairConfirm)
	n.registerApprovalAPI(api)
	n.registerBrowserAPI(api)
	n.registerLLMAPI(api)
	n.registerScheduleAPI(api)
	n.registerWakeAPI(api)
	n.registerUpdateAPI(api)

	mux := http.NewServeMux()
	mux.Handle("/mcp", n.gateway.Handler(n.verifyAgent, n.agentMode))
	mux.Handle("/v1/", n.requireControl(api))
	n.mountRespond(mux) // authenticated by a per-request nonce, not the control token
	n.mountLLM(mux)     // agent token as API key: Authorization: Bearer or x-api-key
	return n.admitHTTP(mux)
}

// verifyAgent maps a bearer token to the agent it was issued to.
func (n *Node) verifyAgent(_ context.Context, tok string, _ *http.Request) (*auth.TokenInfo, error) {
	if subtle.ConstantTimeCompare([]byte(tok), []byte(n.controlToken)) == 1 {
		return &auth.TokenInfo{UserID: cliAgent}, nil
	}
	if name, ok := n.paths.AgentForToken(tok); ok {
		return &auth.TokenInfo{UserID: name}, nil
	}
	return nil, auth.ErrInvalidToken
}

// agentMode reads the agent's tool mode on every request so changes apply
// at once; an unreadable setting falls back to compact ("" is not full).
func (n *Node) agentMode(agent string) string {
	mode, err := n.paths.AgentMode(agent)
	if err != nil {
		n.log.Warn("agent tool mode unreadable; using compact", "agent", agent, "error", err)
		return ""
	}
	return mode
}

func (n *Node) requireControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(n.controlToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "control token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (n *Node) apiStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, control.Status{
		ID: n.id.ID, Name: n.name, Version: n.build.Version, Build: n.build,
		Mesh: n.MeshAddr(), Local: n.LocalAddr(), Started: n.started,
	})
}

func (n *Node) apiPeers(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	out := []control.PeerStatus{}
	for _, p := range n.roster.List() {
		tools := make([]string, 0, len(p.Tools))
		for _, t := range p.Tools {
			tools = append(tools, t.Name)
		}
		last := p.LastSeen
		contact := n.peers.lastContact(p.ID)
		if contact.After(last) {
			last = contact
		}
		asleep, _ := n.sleepState(p)
		out = append(out, control.PeerStatus{
			ID: p.ID, Name: p.Name, Online: !asleep && now.Sub(contact) < onlineWindow, Asleep: asleep, AutoWake: p.AutoWake,
			LastSeen: last, Addrs: p.Addrs, PairedAt: p.PairedAt, Tools: tools,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (n *Node) apiUnpair(w http.ResponseWriter, r *http.Request) {
	p, err := resolvePeer(n.roster.List(), r.PathValue("peer"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if _, err := n.roster.Remove(p.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n.peers.forget(p.ID)
	n.log.Info("unpaired", "peer", p.Name, "id", p.ID)
	writeJSON(w, http.StatusOK, control.PeerStatus{ID: p.ID, Name: p.Name, Addrs: p.Addrs, Tools: []string{}})
}

func (n *Node) apiDiscovered(w http.ResponseWriter, _ *http.Request) {
	out := []control.Sighting{}
	for _, s := range n.sightingList() {
		_, paired := n.roster.Get(s.ID)
		out = append(out, control.Sighting{ID: s.ID, Name: s.Name, Addr: s.Addr.String(), Seen: s.Seen, Paired: paired})
	}
	writeJSON(w, http.StatusOK, out)
}

func (n *Node) apiPairOpen(w http.ResponseWriter, r *http.Request) {
	var req control.PairAcceptRequest
	if !readJSON(w, r, &req) {
		return
	}
	d := 2 * time.Minute
	if req.TimeoutSeconds > 0 {
		d = time.Duration(req.TimeoutSeconds) * time.Second
	}
	until := n.pairing.open(d, time.Now())
	n.log.Info("pairing window open", "until", until.Format(time.TimeOnly))
	writeJSON(w, http.StatusOK, control.PairWindow{Until: until})
}

func (n *Node) apiPairClose(w http.ResponseWriter, _ *http.Request) {
	n.pairing.close()
	writeJSON(w, http.StatusOK, control.PairWindow{})
}

func (n *Node) apiIncoming(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, n.pairing.listIncoming())
}

func (n *Node) apiDecide(w http.ResponseWriter, r *http.Request) {
	var d control.Decision
	if !readJSON(w, r, &d) {
		return
	}
	if err := n.decideIncoming(r.PathValue("id"), d.Accept); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (n *Node) apiPairStart(w http.ResponseWriter, r *http.Request) {
	var req control.PairRequest
	if !readJSON(w, r, &req) {
		return
	}
	v, err := n.startPairing(r.Context(), req.Target)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (n *Node) apiPairGet(w http.ResponseWriter, r *http.Request) {
	v, ok := n.outgoingView(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such pairing")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (n *Node) apiPairConfirm(w http.ResponseWriter, r *http.Request) {
	var d control.Decision
	if !readJSON(w, r, &d) {
		return
	}
	v, err := n.confirmPairing(r.PathValue("id"), d.Accept)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// resolvePeer finds a paired device by exact ID, name (case-insensitive), or unique ID prefix.
func resolvePeer(peers []roster.Peer, ref string) (roster.Peer, error) {
	ref = strings.TrimSpace(ref)
	var byName, byPrefix []roster.Peer
	for _, p := range peers {
		if p.ID == ref {
			return p, nil
		}
		if strings.EqualFold(p.Name, ref) {
			byName = append(byName, p)
		}
		if len(ref) >= 4 && strings.HasPrefix(p.ID, strings.ToLower(ref)) {
			byPrefix = append(byPrefix, p)
		}
	}
	for _, set := range [][]roster.Peer{byName, byPrefix} {
		switch len(set) {
		case 1:
			return set[0], nil
		case 0:
		default:
			return roster.Peer{}, fmt.Errorf("%q matches %d paired devices; use the device ID", ref, len(set))
		}
	}
	return roster.Peer{}, fmt.Errorf("no paired device matches %q", ref)
}
