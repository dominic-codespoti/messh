package node

import (
	"context"
	"net/http"
	"time"
)

// announceTools tells peers that were in contact recently that this device's
// tool list changed, so they refetch it now instead of at their next probe.
// It never contacts a peer that has been quiet or said it is going to sleep:
// a sleeping device with wake-on-pattern enabled could be woken by the traffic.
func (n *Node) announceTools() {
	for _, p := range n.roster.List() {
		if time.Since(n.peers.lastContact(p.ID)) > onlineWindow {
			continue
		}
		if asleep, _ := n.sleepState(p); asleep {
			continue
		}
		go n.pokePeer(p.ID)
	}
}

// pokePeer posts to the peer's /v1/tools-changed. Failure is fine: the peer
// still picks the change up at its next probe.
func (n *Node) pokePeer(id string) {
	ctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
	defer cancel()
	tr := n.pinnedTransport(id)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	for _, addr := range n.peers.candidates(id) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+"/v1/tools-changed", nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		return
	}
}

// handleToolsChanged is the receiving side: refetch the caller's tool list.
func (n *Node) handleToolsChanged(w http.ResponseWriter, r *http.Request) {
	n.peers.refreshAsync(r.Header.Get(hdrPeerID))
	w.WriteHeader(http.StatusNoContent)
}
