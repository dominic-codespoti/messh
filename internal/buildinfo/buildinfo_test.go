package buildinfo

import "testing"

func TestCurrentBuildNumberBoundaries(t *testing.T) {
	previous := Build
	t.Cleanup(func() { Build = previous })
	for _, test := range []struct {
		value string
		want  uint64
	}{
		{value: "18446744073709551615", want: ^uint64(0)},
		{value: "18446744073709551616", want: 0},
		{value: "-1", want: 0},
		{value: "not-a-build", want: 0},
	} {
		t.Run(test.value, func(t *testing.T) {
			Build = test.value
			if got := Current().Build; got != test.want {
				t.Fatalf("Current().Build = %d, want %d", got, test.want)
			}
		})
	}
}

func TestCurrentDistinguishesPublishedAndSourceIdentity(t *testing.T) {
	previousVersion, previousCommit, previousChannel, previousBuild := Version, Commit, Channel, Build
	t.Cleanup(func() {
		Version, Commit, Channel, Build = previousVersion, previousCommit, previousChannel, previousBuild
	})
	commit := "0123456789abcdef0123456789abcdef01234567"
	for _, test := range []struct {
		name        string
		commit      string
		channel     string
		wantChannel string
	}{
		{name: "published main", commit: commit, channel: "main", wantChannel: "main"},
		{name: "published stable", commit: commit, channel: "stable", wantChannel: "stable"},
		{name: "source at published commit", commit: commit, channel: "development", wantChannel: "development"},
		{name: "source metadata is not publication", channel: "main", wantChannel: "development"},
	} {
		t.Run(test.name, func(t *testing.T) {
			Version, Commit, Channel, Build = "0.1.0-main.20+0123456789ab", test.commit, test.channel, "20"
			info := Current()
			if info.Channel != test.wantChannel {
				t.Fatalf("identity channel = %q, want %q", info.Channel, test.wantChannel)
			}
			if test.commit != "" && info.Commit != test.commit {
				t.Fatalf("explicit commit changed: got %q, want %q", info.Commit, test.commit)
			}
			if info.Version != Version || info.Build != 20 {
				t.Fatalf("build metadata changed: %+v", info)
			}
		})
	}
}
