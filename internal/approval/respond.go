package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Toast buttons cannot call into the node: Windows launches a protocol URL,
// which starts `messh respond URL`, which POSTs the decision to the node's
// loopback API. The URL carries the request's one-time nonce, which is the
// only credential (see Engine.Activate).

// ProtocolScheme is the URL scheme `messh respond` is registered for.
const ProtocolScheme = "messh-approve"

// RespondTarget is what a respond URL says: which node, which request, and
// what the person chose.
type RespondTarget struct {
	Port   int    // the node's loopback API port
	ID     string // request ID
	Nonce  string // the request's one-time secret
	Action string // ActionOnce, ActionAlways, ActionDeny or ActionOptions
}

// RespondBody is the JSON body of POST /v1/approvals/{id}/respond.
type RespondBody struct {
	Nonce    string `json:"nonce"`
	Decision string `json:"decision"`
}

const maxRespondURL = 512

// RespondURL renders t as the URL a toast button launches. The keys come in
// a fixed order, which keeps the toast XML stable.
func RespondURL(t RespondTarget) string {
	return ProtocolScheme + "://respond?port=" + strconv.Itoa(t.Port) + "&id=" + url.QueryEscape(t.ID) +
		"&nonce=" + url.QueryEscape(t.Nonce) + "&decision=" + url.QueryEscape(t.Action)
}

// ParseRespondURL validates a respond URL strictly: the URL arrives from the
// OS shell, where any web page or program can ask for it to be opened, so
// nothing outside the exact expected shape is accepted.
func ParseRespondURL(raw string) (RespondTarget, error) {
	var t RespondTarget
	if len(raw) > maxRespondURL {
		return t, errors.New("URL too long")
	}
	for _, r := range raw {
		if r < ' ' || r > '~' {
			return t, errors.New("URL contains characters that are not allowed")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return t, fmt.Errorf("invalid URL: %w", err)
	}
	switch {
	case !strings.EqualFold(u.Scheme, ProtocolScheme):
		return t, fmt.Errorf("not a %s:// URL", ProtocolScheme)
	case !strings.EqualFold(u.Host, "respond") || u.User != nil:
		return t, errors.New("unexpected URL host")
	case u.Path != "" && u.Path != "/", u.Fragment != "", u.Opaque != "":
		return t, errors.New("unexpected URL path")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return t, fmt.Errorf("invalid query: %w", err)
	}
	if len(q) != 4 {
		return t, errors.New("URL needs exactly port, id, nonce and decision")
	}
	get := func(key string) (string, error) {
		v := q[key]
		if len(v) != 1 {
			return "", fmt.Errorf("URL needs exactly one %q", key)
		}
		return v[0], nil
	}
	port, err := get("port")
	if err != nil {
		return t, err
	}
	if t.ID, err = get("id"); err != nil {
		return t, err
	}
	if t.Nonce, err = get("nonce"); err != nil {
		return t, err
	}
	if t.Action, err = get("decision"); err != nil {
		return t, err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || port != strconv.Itoa(n) {
		return t, errors.New("invalid port")
	}
	t.Port = n
	if !isLowerHex(t.ID, 4, 32) {
		return t, errors.New("invalid request ID")
	}
	if !isLowerHex(t.Nonce, 32, 32) {
		return t, errors.New("invalid token")
	}
	switch t.Action {
	case ActionOnce, ActionAlways, ActionDeny, ActionOptions:
	default:
		return t, fmt.Errorf("unknown decision %q", t.Action)
	}
	return t, nil
}

func isLowerHex(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Send delivers the decision to the node on loopback. It returns nil only
// when the node accepted it.
func (t RespondTarget) Send(ctx context.Context) error {
	body, err := json.Marshal(RespondBody{Nonce: t.Nonce, Decision: t.Action})
	if err != nil {
		return err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.Port))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/v1/approvals/"+t.ID+"/respond", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("the messh node is not reachable on port %d (is it still running?)", t.Port)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	return fmt.Errorf("the node answered %s", resp.Status)
}
