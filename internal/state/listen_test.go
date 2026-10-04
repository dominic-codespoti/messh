package state

import (
	"os"
	"testing"
)

func writeConfigFile(t *testing.T, p Paths, data string) {
	t.Helper()
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}


func TestConfigLegacyWithoutListenersLoadsEmpty(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	writeConfigFile(t, p, `{"name":"legacy","unknown":"keep"}`+"\n")
	got, err := p.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "legacy" || got.Listen != "" || got.Local != "" {
		t.Fatalf("legacy config = %+v, want empty listen/local", got)
	}
}

func TestValidateListenAddressRanges(t *testing.T) {
	for _, ok := range []string{"", ":7519", ":0", "0.0.0.0:7521", "127.0.0.1:0", "192.168.1.31:7521", "[::]:7521", "example-host:7521"} {
		if err := ValidateListenAddress(ok); err != nil {
			t.Errorf("ValidateListenAddress(%q) = %v, want success", ok, err)
		}
	}
	for _, bad := range []string{"7519", ":abc", ":-1", ":65536", ":99999", "host with space:7519", "[::]:abc", "no-port:"} {
		if err := ValidateListenAddress(bad); err == nil {
			t.Errorf("ValidateListenAddress(%q) succeeded, want rejection", bad)
		}
	}
}

func TestValidateLocalAddressLoopbackInvariant(t *testing.T) {
	for _, ok := range []string{"", "127.0.0.1:7520", "127.0.0.2:0", "[::1]:7522"} {
		if err := ValidateLocalAddress(ok); err != nil {
			t.Errorf("ValidateLocalAddress(%q) = %v, want success", ok, err)
		}
	}
	for _, bad := range []string{"localhost:7520", "LOCALHOST:7520", "example.com:7520", "0.0.0.0:7520", ":7520", "192.168.1.31:7520", "127.0.0.1", "127.0.0.1:abc", "127.0.0.1:65536", "127.0.0.1:-1"} {
		if err := ValidateLocalAddress(bad); err == nil {
			t.Errorf("ValidateLocalAddress(%q) succeeded, want rejection", bad)
		}
	}
}


