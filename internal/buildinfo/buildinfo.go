// Package buildinfo describes the identity of the running executable.
package buildinfo

import (
	"runtime/debug"
	"strconv"
)

// These strings are set by the release build's linker flags. A source build is
// deliberately development-channel even when its commit was also published.
var (
	Version = "0.1.0-dev"
	Commit  = ""
	Channel = "development"
	Build   = "0"
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Channel string `json:"channel"`
	Build   uint64 `json:"build"`
}

func Current() Info {
	build, err := strconv.ParseUint(Build, 10, 64)
	if err != nil {
		build = 0
	}
	info := Info{Version: Version, Commit: Commit, Channel: Channel, Build: build}
	if info.Commit == "" {
		// VCS metadata identifies local source, not a published artifact. In
		// particular, its revision does not capture uncommitted changes.
		info.Channel = "development"
		if metadata, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range metadata.Settings {
				if setting.Key == "vcs.revision" {
					info.Commit = setting.Value
					break
				}
			}
		}
	}
	return info
}
