package control

// WakeRequest is the body of POST /v1/wake/{peer}. With Auto set it only
// turns waking the peer before tool calls on or off; otherwise it wakes the
// peer and waits up to WaitSeconds (0 = 60, max 300) for it to answer.
type WakeRequest struct {
	WaitSeconds int   `json:"wait_seconds,omitempty"`
	Auto        *bool `json:"auto,omitempty"`
}

// WakeResult reports a wake attempt; it is also the mesh_wake tool's result.
// Before is the peer's state when asked: online, asleep (it announced sleep),
// or offline. MACs are partly masked. Routes lists "source -> destination"
// pairs the magic packets went out on; NoSharedSubnet means no local
// interface is in the peer's subnets, so only 255.255.255.255 was used.
type WakeResult struct {
	Device         string   `json:"device"`
	ID             string   `json:"id"`
	Before         string   `json:"before,omitempty"`
	Awake          bool     `json:"awake"`
	AutoWake       bool     `json:"auto_wake"`
	MACs           []string `json:"macs,omitempty"`
	PacketsSent    int      `json:"packets_sent"`
	PacketsFailed  int      `json:"packets_failed,omitempty"`
	Routes         []string `json:"routes,omitempty"`
	NoSharedSubnet bool     `json:"no_shared_subnet,omitempty"`
	Seconds        float64  `json:"seconds_to_wake,omitempty"`
	Error          string   `json:"error,omitempty"`
}
