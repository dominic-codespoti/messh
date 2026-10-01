package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"messh/internal/control"
	"messh/internal/identity"
)

const (
	pairRequestTTL  = 3 * time.Minute
	maxPairWindow   = 10 * time.Minute
	maxIncoming     = 4
	outgoingKeepFor = 10 * time.Minute
)

var errWindowClosed = errors.New("pairing window closed")

type pairing struct {
	mu       sync.Mutex
	until    time.Time
	incoming map[string]*incoming
	outgoing map[string]*outgoing
}

type incoming struct {
	control.Incoming
	deadline time.Time
	decided  chan bool // buffered; receives exactly one decision
	once     sync.Once
}

func (in *incoming) decide(accept bool) {
	in.once.Do(func() { in.decided <- accept })
}

type outgoing struct {
	id, addr, peerID, peerName, code string
	created                          time.Time
	cancel                           context.CancelFunc

	// guarded by pairing.mu
	confirmed  *bool  // local user's verdict on the code
	remote     string // "", "accepted", "rejected", "failed"
	remoteName string
	paired     bool
	err        string
}

func newPairing() *pairing {
	return &pairing{incoming: map[string]*incoming{}, outgoing: map[string]*outgoing{}}
}

func (p *pairing) open(d time.Duration, now time.Time) time.Time {
	d = min(max(d, time.Second), maxPairWindow)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.until = now.Add(d)
	return p.until
}

func (p *pairing) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.until = time.Time{}
	for _, in := range p.incoming {
		in.decide(false)
	}
}

func (p *pairing) addIncoming(peerID, name, addr, code string, now time.Time) (*incoming, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.After(p.until) {
		return nil, errWindowClosed
	}
	if len(p.incoming) >= maxIncoming {
		return nil, errors.New("too many pending pairing requests")
	}
	in := &incoming{
		Incoming: control.Incoming{ID: randomID(), PeerID: peerID, PeerName: name, Addr: addr, Code: code, Created: now},
		deadline: now.Add(pairRequestTTL),
		decided:  make(chan bool, 1),
	}
	p.incoming[in.ID] = in
	return in, nil
}

func (p *pairing) removeIncoming(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.incoming, id)
}

func (p *pairing) getIncoming(id string) (*incoming, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	in, ok := p.incoming[id]
	return in, ok
}

func (p *pairing) listIncoming() []control.Incoming {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]control.Incoming, 0, len(p.incoming))
	for _, in := range p.incoming {
		out = append(out, in.Incoming)
	}
	return out
}

func (p *pairing) gc(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, og := range p.outgoing {
		if now.Sub(og.created) > outgoingKeepFor {
			og.cancel()
			delete(p.outgoing, id)
		}
	}
}

// view renders an outgoing pairing; callers hold p.mu.
func (og *outgoing) view() control.Outgoing {
	v := control.Outgoing{ID: og.id, Addr: og.addr, PeerID: og.peerID, PeerName: og.peerName, Code: og.code, Error: og.err}
	if og.remoteName != "" {
		v.PeerName = og.remoteName
	}
	switch {
	case og.paired:
		v.State = control.OutPaired
	case og.confirmed != nil && !*og.confirmed:
		v.State = control.OutCancelled
	case og.remote == "rejected":
		v.State = control.OutRejected
	case og.remote == "failed" || og.err != "":
		v.State = control.OutFailed
	case og.confirmed == nil:
		v.State = control.OutAwaitingConfirmation
	default:
		v.State = control.OutAwaitingRemote
	}
	return v
}

func (n *Node) outgoingView(id string) (control.Outgoing, bool) {
	n.pairing.mu.Lock()
	defer n.pairing.mu.Unlock()
	og, ok := n.pairing.outgoing[id]
	if !ok {
		return control.Outgoing{}, false
	}
	return og.view(), true
}

// startPairing contacts target, learns its identity, and sends the pairing
// request in the background. The returned code must be compared by both users.
func (n *Node) startPairing(ctx context.Context, target string) (control.Outgoing, error) {
	addr, err := n.resolveTarget(target)
	if err != nil {
		return control.Outgoing{}, err
	}
	peerID, cn, err := n.probe(ctx, addr)
	if err != nil {
		return control.Outgoing{}, fmt.Errorf("contact %s: %w", addr, err)
	}
	if peerID == n.id.ID {
		return control.Outgoing{}, errors.New("that address is this device")
	}
	hctx, cancel := context.WithTimeout(n.ctx, pairRequestTTL)
	og := &outgoing{
		id: randomID(), addr: addr, peerID: peerID, peerName: cn,
		code: identity.PairingCode(n.id.ID, peerID), created: time.Now(), cancel: cancel,
	}
	n.pairing.mu.Lock()
	n.pairing.outgoing[og.id] = og
	v := og.view()
	n.pairing.mu.Unlock()

	go n.sendHello(hctx, og)
	return v, nil
}

func (n *Node) sendHello(ctx context.Context, og *outgoing) {
	body, _ := json.Marshal(control.Hello{Name: n.name, Port: n.meshPort()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+og.addr+"/v1/pair", bytes.NewReader(body))
	if err != nil {
		n.finishHello(og, "failed", "", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	tr := n.pinnedTransport(og.peerID)
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return // cancelled locally
		}
		n.finishHello(og, "failed", "", err.Error())
		return
	}
	defer resp.Body.Close()
	var reply struct {
		control.HelloReply
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&reply)
	switch {
	case resp.StatusCode == http.StatusOK && reply.Accepted && reply.ID == og.peerID:
		name := reply.Name
		if !ValidName(name) {
			name = og.peerName
		}
		n.finishHello(og, "accepted", name, "")
	case resp.StatusCode == http.StatusForbidden && reply.Error == "":
		n.finishHello(og, "rejected", "", "")
	default:
		msg := reply.Error
		if msg == "" {
			msg = resp.Status
		}
		n.finishHello(og, "failed", "", msg)
	}
}

func (n *Node) finishHello(og *outgoing, remote, name, errMsg string) {
	n.pairing.mu.Lock()
	og.remote = remote
	og.remoteName = name
	og.err = errMsg
	n.pairing.mu.Unlock()
	n.completePairing(og)
}

// confirmPairing records the local user's verdict on the displayed code.
func (n *Node) confirmPairing(id string, accept bool) (control.Outgoing, error) {
	n.pairing.mu.Lock()
	og, ok := n.pairing.outgoing[id]
	if !ok {
		n.pairing.mu.Unlock()
		return control.Outgoing{}, fmt.Errorf("no pairing %s", id)
	}
	if og.confirmed == nil {
		og.confirmed = &accept
	}
	n.pairing.mu.Unlock()
	if !accept {
		og.cancel()
	}
	n.completePairing(og)
	v, _ := n.outgoingView(id)
	return v, nil
}

// completePairing adds the peer once both users have accepted.
func (n *Node) completePairing(og *outgoing) {
	n.pairing.mu.Lock()
	ready := !og.paired && og.confirmed != nil && *og.confirmed && og.remote == "accepted"
	if ready {
		og.paired = true
	}
	name := og.remoteName
	n.pairing.mu.Unlock()
	if !ready {
		return
	}
	if err := n.roster.Pair(og.peerID, name, og.addr, time.Now()); err != nil {
		n.pairing.mu.Lock()
		og.paired = false
		og.err = "save peer: " + err.Error()
		n.pairing.mu.Unlock()
		return
	}
	n.log.Info("paired", "peer", name, "id", identity.Short(og.peerID), "addr", og.addr)
	n.peers.refreshSoon(og.peerID)
}

// decideIncoming applies the local user's answer to a pairing request.
func (n *Node) decideIncoming(id string, accept bool) error {
	in, ok := n.pairing.getIncoming(id)
	if !ok {
		return fmt.Errorf("no pending pairing request %s", id)
	}
	if accept {
		if err := n.roster.Pair(in.PeerID, in.PeerName, in.Addr, time.Now()); err != nil {
			in.decide(false)
			return fmt.Errorf("save peer: %w", err)
		}
		n.log.Info("paired", "peer", in.PeerName, "id", identity.Short(in.PeerID), "addr", in.Addr)
		n.peers.refreshSoon(in.PeerID)
	}
	in.decide(accept)
	return nil
}

// resolveTarget maps a discovered name, ID prefix, host, or host:port to an address.
func (n *Node) resolveTarget(target string) (string, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", errors.New("empty pairing target")
	}
	var matches []string
	for _, s := range n.sightingList() {
		if strings.EqualFold(s.Name, t) || (len(t) >= 4 && strings.HasPrefix(s.ID, strings.ToLower(t))) {
			matches = append(matches, s.Addr.String())
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
	default:
		return "", fmt.Errorf("%q matches %d discovered devices; use an address or a longer ID prefix", t, len(matches))
	}
	if _, _, err := net.SplitHostPort(t); err == nil {
		return t, nil
	}
	return net.JoinHostPort(t, strconv.Itoa(defaultMeshPort)), nil
}

// probe connects to addr and returns the device ID and certificate name it presents.
func (n *Node) probe(ctx context.Context, addr string) (id, name string, err error) {
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config: &tls.Config{
			Certificates:       []tls.Certificate{n.id.Cert},
			InsecureSkipVerify: true, // the user verifies the identity by comparing pairing codes
			MinVersion:         tls.VersionTLS13,
		},
	}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", "", err
	}
	defer c.Close()
	certs := c.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", "", errors.New("no certificate presented")
	}
	return identity.IDFromCert(certs[0]), certs[0].Subject.CommonName, nil
}

func randomID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
