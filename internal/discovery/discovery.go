// Package discovery announces this node on the LAN via IPv4 UDP multicast and
// reports announcements from other nodes.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/ipv4"
)

// Announcement is the UDP multicast payload (JSON, <= 1024 bytes).
type Announcement struct {
	V        int    `json:"v"`                  // protocol version, always 1
	ID       string `json:"id"`                 // device ID (52-char base32, a-z2-7)
	Name     string `json:"name"`               // <= 64 bytes
	Port     int    `json:"port"`               // mesh TCP port, 1..65535
	Query    bool   `json:"query,omitempty"`    // ask others to announce back now
	Sleeping bool   `json:"sleeping,omitempty"` // about to sleep; an unauthenticated hint that may only pause probes
}

// Sighting is a validated announcement received from another node.
type Sighting struct {
	Announcement
	Addr netip.AddrPort // packet source IP + Announcement.Port
	Seen time.Time
}

// Config configures Run.
type Config struct {
	Group      netip.AddrPort // zero → DefaultGroup
	Self       Announcement   // V forced to 1; Query and Sleeping ignored (managed internally)
	Interval   time.Duration  // zero → 30s
	Interfaces []string       // interface names; nil → all up, multicast-capable, non-loopback interfaces with an IPv4 address
	OnSighting func(Sighting) // called for every valid announcement from another ID; must not block long
	Logger     *slog.Logger   // nil → slog.Default()
	Sleep      <-chan bool    // true: this device is about to sleep, announce Sleeping now and until false (resumed)
}

// DefaultGroup is the multicast group and port announcements are sent to.
var DefaultGroup = netip.MustParseAddrPort("239.255.75.19:7519")

const (
	protocolVersion = 1
	maxPacket       = 1024
	maxName         = 64
	defaultInterval = 30 * time.Second
	queryJitter     = 500 * time.Millisecond
	queryRateLimit  = 2 * time.Second
)

var idPattern = regexp.MustCompile(`^[a-z2-7]{52}$`)

func validate(a Announcement) error {
	switch {
	case a.V != protocolVersion:
		return fmt.Errorf("unsupported version %d", a.V)
	case !idPattern.MatchString(a.ID):
		return fmt.Errorf("invalid id %q", a.ID)
	case len(a.Name) < 1 || len(a.Name) > maxName:
		return fmt.Errorf("invalid name length %d", len(a.Name))
	case a.Port < 1 || a.Port > 65535:
		return fmt.Errorf("invalid port %d", a.Port)
	}
	return nil
}

// link is the pair of sockets used on one network interface.
type link struct {
	ifi    net.Interface
	ip     netip.Addr       // interface IPv4 address
	recv   *ipv4.PacketConn // bound to the group port, joined to the group on ifi
	send   *ipv4.PacketConn // bound to ip:0, multicast interface ifi
	rc     net.PacketConn   // underlying recv socket
	sc     net.PacketConn   // underlying send socket
	smu    sync.Mutex       // serializes SetMulticastInterface+WriteTo and close
	fail   bool             // last send failed (guarded by smu); limits log spam
	closed bool             // sockets closed (guarded by smu)
}

func (l *link) close() {
	l.smu.Lock()
	defer l.smu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	l.rc.Close()
	l.sc.Close()
}

// candidate is an interface eligible for a link, with the IPv4 address to use.
type candidate struct {
	ifi net.Interface
	ip  netip.Addr
}

// linkKey identifies a link's binding; a change means the link must be rebuilt.
type linkKey struct {
	index int
	ip    netip.Addr
}

func (c candidate) key() linkKey { return linkKey{c.ifi.Index, c.ip} }

// rescanInterval is how often auto-selected interfaces are re-enumerated.
var rescanInterval = 30 * time.Second

type runner struct {
	cfg   Config
	log   *slog.Logger
	group *net.UDPAddr
	ctx   context.Context
	wg    sync.WaitGroup // read loops

	// links is keyed by interface name. Only the Run goroutine mutates it
	// (under lmu); other goroutines read it under lmu.
	lmu   sync.Mutex
	links map[string]*link

	// Auto-mode rescan state, owned by the Run goroutine.
	joinFailed map[string]linkKey // last failed join per interface, to log once
	idleLogged bool               // "no usable interface" already logged

	cbMu sync.Mutex // serializes OnSighting calls

	sleeping atomic.Bool // announcements carry Sleeping; set from Config.Sleep

	qMu       sync.Mutex
	qPending  bool
	qLast     time.Time
	qTimer    *time.Timer
	qShutdown bool
}

// Run joins the group on the selected interfaces, announces, listens, and blocks until ctx is done.
// It returns an error only if the config is invalid or, with explicit Interfaces, none could be joined.
// With nil Interfaces it re-scans every rescanInterval, joining interfaces that appear or change
// address and dropping those that disappear; having none is not an error.
func Run(ctx context.Context, cfg Config) error {
	if !cfg.Group.IsValid() {
		cfg.Group = DefaultGroup
	}
	if !cfg.Group.Addr().Is4() || !cfg.Group.Addr().IsMulticast() || cfg.Group.Port() == 0 {
		return fmt.Errorf("discovery: group %v is not an IPv4 multicast address with a port", cfg.Group)
	}
	if cfg.Interval == 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.Interval < 0 {
		return fmt.Errorf("discovery: negative interval %v", cfg.Interval)
	}
	cfg.Self.V = protocolVersion
	cfg.Self.Query = false
	cfg.Self.Sleeping = false
	if err := validate(cfg.Self); err != nil {
		return fmt.Errorf("discovery: invalid self announcement: %w", err)
	}
	if b, _ := json.Marshal(Announcement{V: 1, ID: cfg.Self.ID, Name: cfg.Self.Name, Port: cfg.Self.Port, Query: true, Sleeping: true}); len(b) > maxPacket {
		return fmt.Errorf("discovery: announcement exceeds %d bytes", maxPacket)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "discovery")

	r := &runner{
		cfg:        cfg,
		log:        log,
		group:      net.UDPAddrFromAddrPort(cfg.Group),
		ctx:        ctx,
		links:      make(map[string]*link),
		joinFailed: make(map[string]linkKey),
	}
	auto := cfg.Interfaces == nil
	if auto {
		r.rescan()
	} else if err := r.joinExplicit(); err != nil {
		return fmt.Errorf("discovery: %w", err)
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	var rescanC <-chan time.Time
	if auto {
		rt := time.NewTicker(rescanInterval)
		defer rt.Stop()
		rescanC = rt.C
	}
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			r.announce(false)
		case <-rescanC:
			r.rescan()
		case s := <-cfg.Sleep:
			r.sleeping.Store(s)
			if s {
				// Multicast over Wi-Fi is lossy and there is no second
				// chance once the NIC is down: send it twice.
				r.announce(false)
				time.Sleep(50 * time.Millisecond)
				r.announce(false)
			} else {
				r.announce(true) // back: ask peers to answer so addresses refresh at once
			}
		}
	}

	r.qMu.Lock()
	r.qShutdown = true
	if r.qTimer != nil {
		r.qTimer.Stop()
	}
	r.qMu.Unlock()

	r.lmu.Lock()
	for _, l := range r.links {
		l.close()
	}
	r.lmu.Unlock()
	r.wg.Wait()
	return nil
}

// joinExplicit opens links on the configured interface names and sends the initial query.
// An unknown name, or failing to join any interface, is an error.
func (r *runner) joinExplicit() error {
	var joinErrs []error
	for _, name := range r.cfg.Interfaces {
		ifi, err := net.InterfaceByName(name)
		if err != nil {
			return fmt.Errorf("interface %q: %w", name, err)
		}
		ip, ok := interfaceIPv4(ifi)
		if !ok {
			r.log.Warn("interface has no IPv4 address", "iface", name)
			joinErrs = append(joinErrs, fmt.Errorf("%s: no IPv4 address", name))
			continue
		}
		if err := r.addLink(candidate{*ifi, ip}); err != nil {
			r.log.Warn("cannot join multicast group on interface", "iface", name, "err", err)
			joinErrs = append(joinErrs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if len(r.links) == 0 {
		if len(joinErrs) == 0 {
			return errors.New("no usable interfaces")
		}
		return fmt.Errorf("no interface could be joined: %w", errors.Join(joinErrs...))
	}
	r.announce(true)
	return nil
}

// rescan re-enumerates auto-selected interfaces, drops links whose interface vanished or
// changed, and joins (announcing with Query=true) on new ones.
func (r *runner) rescan() {
	all, err := net.Interfaces()
	if err != nil {
		r.log.Warn("list interfaces", "err", err)
		return
	}
	want := autoCandidates(all, interfaceIPv4)
	have := make(map[string]linkKey, len(r.links))
	for name, l := range r.links {
		have[name] = linkKey{l.ifi.Index, l.ip}
	}
	add, drop := diffLinks(have, want)

	for _, name := range drop {
		r.lmu.Lock()
		l := r.links[name]
		delete(r.links, name)
		r.lmu.Unlock()
		l.close()
		r.log.Info("left multicast group on interface", "iface", name, "ip", l.ip)
	}

	present := make(map[string]bool, len(want))
	for _, c := range want {
		present[c.ifi.Name] = true
	}
	for name := range r.joinFailed {
		if !present[name] {
			delete(r.joinFailed, name)
		}
	}

	for _, c := range add {
		if err := r.addLink(c); err != nil {
			if prev, ok := r.joinFailed[c.ifi.Name]; ok && prev == c.key() {
				r.log.Debug("cannot join multicast group on interface", "iface", c.ifi.Name, "ip", c.ip, "err", err)
			} else {
				r.log.Warn("cannot join multicast group on interface", "iface", c.ifi.Name, "ip", c.ip, "err", err)
			}
			r.joinFailed[c.ifi.Name] = c.key()
			continue
		}
		delete(r.joinFailed, c.ifi.Name)
		r.announceOn(r.links[c.ifi.Name], true)
	}

	if len(r.links) == 0 {
		if !r.idleLogged {
			r.log.Info("no usable multicast interface; waiting for one to appear")
			r.idleLogged = true
		}
	} else {
		r.idleLogged = false
	}
}

// addLink opens sockets on c, registers the link and starts its read loop.
func (r *runner) addLink(c candidate) error {
	if _, dup := r.links[c.ifi.Name]; dup {
		return nil
	}
	l, err := r.openLink(r.ctx, c)
	if err != nil {
		return err
	}
	r.lmu.Lock()
	r.links[c.ifi.Name] = l
	r.lmu.Unlock()
	r.log.Info("joined multicast group", "iface", c.ifi.Name, "ip", c.ip, "group", r.cfg.Group)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.readLoop(l)
	}()
	return nil
}

// autoCandidates picks up, multicast-capable, non-loopback interfaces with an IPv4 address.
func autoCandidates(all []net.Interface, addrOf func(*net.Interface) (netip.Addr, bool)) []candidate {
	var out []candidate
	for i := range all {
		ifi := &all[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ip, ok := addrOf(ifi); ok {
			out = append(out, candidate{*ifi, ip})
		}
	}
	return out
}

// diffLinks compares current links (by interface name) with the wanted candidates.
// Links whose interface vanished or whose index/address changed are dropped; changed
// and new interfaces are added.
func diffLinks(have map[string]linkKey, want []candidate) (add []candidate, drop []string) {
	wanted := make(map[string]linkKey, len(want))
	for _, c := range want {
		wanted[c.ifi.Name] = c.key()
	}
	for name, k := range have {
		if wk, ok := wanted[name]; !ok || wk != k {
			drop = append(drop, name)
		}
	}
	for _, c := range want {
		if k, ok := have[c.ifi.Name]; !ok || k != c.key() {
			add = append(add, c)
		}
	}
	slices.Sort(drop)
	return add, drop
}

// interfaceIPv4 returns the first IPv4 address on ifi, preferring non-link-local.
func interfaceIPv4(ifi *net.Interface) (netip.Addr, bool) {
	addrs, err := ifi.Addrs()
	if err != nil {
		return netip.Addr{}, false
	}
	var fallback netip.Addr
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.Is4() {
			continue
		}
		if !ip.IsLinkLocalUnicast() {
			return ip, true
		}
		if !fallback.IsValid() {
			fallback = ip
		}
	}
	return fallback, fallback.IsValid()
}

func (r *runner) openLink(ctx context.Context, c candidate) (_ *link, err error) {
	ifi, ip := c.ifi, c.ip
	l := &link{ifi: ifi, ip: ip}
	defer func() {
		if err != nil {
			if l.rc != nil {
				l.rc.Close()
			}
			if l.sc != nil {
				l.sc.Close()
			}
		}
	}()

	lc := net.ListenConfig{Control: controlReuse}
	bind := recvBindAddr(r.cfg.Group, ip)
	l.rc, err = lc.ListenPacket(ctx, "udp4", bind.String())
	if err != nil {
		return nil, fmt.Errorf("bind %v: %w", bind, err)
	}
	l.recv = ipv4.NewPacketConn(l.rc)
	if err := l.recv.JoinGroup(&ifi, r.group); err != nil {
		return nil, fmt.Errorf("join %v: %w", r.cfg.Group.Addr(), err)
	}
	// Windows applies IP_MULTICAST_LOOP on the receiving socket; Unix on the sender.
	// Setting both is harmless and covers either semantic.
	if err := l.recv.SetMulticastLoopback(true); err != nil {
		r.log.Debug("set multicast loopback on receive socket", "iface", ifi.Name, "err", err)
	}

	l.sc, err = (&net.ListenConfig{}).ListenPacket(ctx, "udp4", netip.AddrPortFrom(ip, 0).String())
	if err != nil {
		return nil, fmt.Errorf("bind send socket %v: %w", ip, err)
	}
	l.send = ipv4.NewPacketConn(l.sc)
	if err := l.send.SetMulticastTTL(1); err != nil {
		return nil, fmt.Errorf("set multicast ttl: %w", err)
	}
	if err := l.send.SetMulticastLoopback(true); err != nil {
		return nil, fmt.Errorf("set multicast loopback: %w", err)
	}
	if err := l.send.SetMulticastInterface(&ifi); err != nil {
		return nil, fmt.Errorf("set multicast interface: %w", err)
	}
	return l, nil
}

// announce sends the own announcement on every link.
func (r *runner) announce(query bool) {
	r.lmu.Lock()
	links := make([]*link, 0, len(r.links))
	for _, l := range r.links {
		links = append(links, l)
	}
	r.lmu.Unlock()
	for _, l := range links {
		r.announceOn(l, query)
	}
}

// announceOn sends the own announcement on one link.
func (r *runner) announceOn(l *link, query bool) {
	a := r.cfg.Self
	a.Query = query
	a.Sleeping = r.sleeping.Load()
	b, err := json.Marshal(a)
	if err != nil {
		r.log.Error("marshal announcement", "err", err)
		return
	}
	r.sendOn(l, b)
}

func (r *runner) sendOn(l *link, b []byte) {
	l.smu.Lock()
	defer l.smu.Unlock()
	if l.closed {
		return
	}
	err := l.send.SetMulticastInterface(&l.ifi)
	if err == nil {
		_, err = l.send.WriteTo(b, nil, r.group)
	}
	if err != nil {
		if r.ctx.Err() != nil {
			return
		}
		if !l.fail {
			r.log.Warn("multicast send failed; skipping interface until it recovers", "iface", l.ifi.Name, "err", err)
		} else {
			r.log.Debug("multicast send failed", "iface", l.ifi.Name, "err", err)
		}
		l.fail = true
		return
	}
	if l.fail {
		r.log.Info("multicast send recovered", "iface", l.ifi.Name)
		l.fail = false
	}
}

func (r *runner) readLoop(l *link) {
	buf := make([]byte, maxPacket+1)
	backoff := 100 * time.Millisecond
	for {
		n, _, src, err := l.recv.ReadFrom(buf)
		if err != nil {
			if r.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			r.log.Warn("multicast receive failed", "iface", l.ifi.Name, "err", err)
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = 100 * time.Millisecond
		r.handle(l, buf[:n], src)
	}
}

func (r *runner) handle(l *link, pkt []byte, src net.Addr) {
	ua, ok := src.(*net.UDPAddr)
	if !ok {
		return
	}
	srcIP, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return
	}
	srcIP = srcIP.Unmap()
	if len(pkt) > maxPacket {
		r.log.Debug("dropping oversized announcement", "iface", l.ifi.Name, "src", srcIP, "size", len(pkt))
		return
	}
	var a Announcement
	if err := json.Unmarshal(pkt, &a); err != nil {
		r.log.Debug("dropping malformed announcement", "iface", l.ifi.Name, "src", srcIP, "err", err)
		return
	}
	if err := validate(a); err != nil {
		r.log.Debug("dropping invalid announcement", "iface", l.ifi.Name, "src", srcIP, "err", err)
		return
	}
	if a.ID == r.cfg.Self.ID {
		return
	}
	if a.Query {
		r.scheduleResponse()
	}
	if r.cfg.OnSighting == nil {
		return
	}
	s := Sighting{
		Announcement: a,
		Addr:         netip.AddrPortFrom(srcIP, uint16(a.Port)),
		Seen:         time.Now(),
	}
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	r.cfg.OnSighting(s)
}

// scheduleResponse answers a query with a group announcement after random jitter,
// at most once per queryRateLimit.
func (r *runner) scheduleResponse() {
	r.qMu.Lock()
	defer r.qMu.Unlock()
	if r.qShutdown || r.qPending || (!r.qLast.IsZero() && time.Since(r.qLast) < queryRateLimit) {
		return
	}
	r.qPending = true
	r.qLast = time.Now()
	jitter := rand.N(queryJitter + 1)
	r.qTimer = time.AfterFunc(jitter, func() {
		r.qMu.Lock()
		r.qPending = false
		stop := r.qShutdown
		r.qMu.Unlock()
		if stop {
			return
		}
		r.announce(false)
	})
}
