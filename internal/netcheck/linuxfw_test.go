package netcheck

import (
	"testing"
)

func TestParseUFWActive(t *testing.T) {
	s, ok := ParseUFW(string(readFixture(t, "ufw_active.txt")))
	if !ok || !s.Active || s.DefaultIncoming != "deny" {
		t.Fatalf("status = %+v ok=%v", s, ok)
	}
	if len(s.Rules) != 8 { // the ALLOW OUT row is dropped
		t.Fatalf("got %d rules: %+v", len(s.Rules), s.Rules)
	}
	if r := s.Rules[1]; r.To != "7519/tcp" || r.Action != "ALLOW" || r.From != "192.168.1.0/24" || r.V6 {
		t.Errorf("rule with comment = %+v", r)
	}
	if !s.Rules[6].V6 {
		t.Errorf("v6 row not marked: %+v", s.Rules[6])
	}
	for _, c := range []struct {
		proto string
		port  int
		want  bool
	}{
		{"tcp", 7519, true},  // 7519/tcp from the LAN
		{"udp", 7519, true},  // 7000:7600/udp
		{"tcp", 8080, false}, // DENY comes before the 8000:9000 allow: first match wins
		{"tcp", 8500, true},
		{"tcp", 1234, false}, // default deny
		{"udp", 22, false},
	} {
		if _, got := s.Allows(c.proto, c.port); got != c.want {
			t.Errorf("Allows(%s %d) = %v", c.proto, c.port, got)
		}
	}
}

func TestParseUFWOther(t *testing.T) {
	s, ok := ParseUFW(string(readFixture(t, "ufw_inactive.txt")))
	if !ok || s.Active {
		t.Fatalf("inactive: %+v ok=%v", s, ok)
	}
	if _, allowed := s.Allows("tcp", 7519); !allowed {
		t.Error("inactive ufw blocks nothing")
	}
	if _, ok := ParseUFW("ERROR: You need to be root to run this script\n"); ok {
		t.Error("root error parsed as status")
	}
	plain := "Status: active\n\nTo                         Action      From\n--                         ------      ----\n7519                       ALLOW       Anywhere\n192.168.1.25 22/tcp        ALLOW       Anywhere\n"
	s, ok = ParseUFW(plain)
	if !ok || len(s.Rules) != 2 {
		t.Fatalf("plain: %+v", s)
	}
	if _, allowed := s.Allows("udp", 7519); !allowed {
		t.Error("port without protocol covers udp")
	}
	if _, allowed := s.Allows("tcp", 7520); allowed {
		t.Error("unknown default incoming must not count as allow")
	}
}

func TestParseFirewalld(t *testing.T) {
	z, ok := ParseFirewalld(string(readFixture(t, "firewalld_list_all.txt")))
	if !ok || z.Name != "public" || !z.Active || z.Target != "default" {
		t.Fatalf("zone = %+v ok=%v", z, ok)
	}
	if len(z.Interfaces) != 1 || z.Interfaces[0] != "wlan0" || len(z.Ports) != 2 || len(z.RichRules) != 2 || len(z.Sources) != 0 {
		t.Fatalf("zone = %+v", z)
	}
	for _, c := range []struct {
		proto string
		port  int
		want  bool
	}{
		{"tcp", 7519, true},  // ports
		{"udp", 7519, true},  // rich rule
		{"udp", 8050, true},  // port range
		{"tcp", 9000, false}, // rich rule rejects
		{"tcp", 22, false},   // only as the ssh service, which is not resolved
	} {
		if _, got := z.Allows(c.proto, c.port); got != c.want {
			t.Errorf("Allows(%s %d) = %v", c.proto, c.port, got)
		}
	}
	if _, ok := ParseFirewalld("FirewallD is not running\n"); ok {
		t.Error("not-running message parsed as a zone")
	}
	if _, ok := ParseFirewalld("Authorization failed.\n"); ok {
		t.Error("authorization error parsed as a zone")
	}
	trusted, ok := ParseFirewalld("trusted (active)\n  target: ACCEPT\n  interfaces: eth0\n")
	if _, allowed := trusted.Allows("udp", 7519); !ok || !allowed {
		t.Error("target ACCEPT allows everything")
	}
}

func TestParseNft(t *testing.T) {
	chains := ParseNft(string(readFixture(t, "nft_ruleset.txt")))
	if len(chains) != 3 {
		t.Fatalf("chains = %+v", chains)
	}
	in := chains[0]
	if in.Family != "inet" || in.Table != "filter" || in.Name != "input" || in.Hook != "input" || in.Policy != "drop" || len(in.Rules) != 6 {
		t.Fatalf("input = %+v", in)
	}
	if chains[2].Policy != "accept" || chains[2].Hook != "output" {
		t.Errorf("output = %+v", chains[2])
	}
	for _, c := range []struct {
		proto string
		port  int
		want  bool
	}{
		{"tcp", 7519, true},  // ip saddr ... tcp dport 7519 accept
		{"udp", 7519, true},  // th dport { 5353, 7000-7600 }
		{"tcp", 7300, true},  // th covers tcp too
		{"tcp", 22, false},   // named set is not resolved
		{"tcp", 9000, false}, // explicit drop
	} {
		if _, got := NftAllows(chains, c.proto, c.port); got != c.want {
			t.Errorf("NftAllows(%s %d) = %v", c.proto, c.port, got)
		}
	}
}

func TestNftAcceptPolicyWithCatchAll(t *testing.T) {
	open := ParseNft("table ip t {\n\tchain in {\n\t\ttype filter hook input priority 0; policy accept;\n\t\tct state invalid drop\n\t}\n}\n")
	if _, ok := NftAllows(open, "tcp", 7519); !ok {
		t.Error("accept policy without a catch-all drop lets 7519 in")
	}
	closed := ParseNft("table ip t {\n\tchain in {\n\t\ttype filter hook input priority 0; policy accept;\n\t\ttcp dport 22 accept\n\t\treject with icmpx type port-unreachable\n\t}\n}\n")
	if _, ok := NftAllows(closed, "tcp", 7519); ok {
		t.Error("catch-all reject must close the chain")
	}
	if _, ok := NftAllows(nil, "tcp", 7519); !ok {
		t.Error("no input chain filters nothing")
	}
}
