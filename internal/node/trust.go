package node

import (
	"errors"
	"net/http"

	"messh/internal/trust"
)

// Trust is owner-controlled authorization, separate from authenticated pairing.
func (n *Node) registerTrustAPI(api *http.ServeMux) {
	api.HandleFunc("GET /v1/trust", n.apiTrustList)
	api.HandleFunc("POST /v1/trust", n.apiTrustAdd)
	api.HandleFunc("DELETE /v1/trust/{device}", n.apiTrustRemove)
}

func (n *Node) apiTrustList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, n.trust.List())
}

func (n *Node) apiTrustAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Device string `json:"device"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	n.trustMu.Lock()
	defer n.trustMu.Unlock()
	peer, err := resolvePeer(n.roster.List(), in.Device)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	device, err := n.trust.Add(peer.ID, peer.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, device)
}

func (n *Node) apiTrustRemove(w http.ResponseWriter, r *http.Request) {
	n.trustMu.Lock()
	defer n.trustMu.Unlock()
	deviceID := r.PathValue("device")
	if !n.trust.Allows(deviceID) {
		peer, err := resolvePeer(n.roster.List(), deviceID)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		deviceID = peer.ID
	}
	switch err := n.trust.Remove(deviceID); {
	case errors.Is(err, trust.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
