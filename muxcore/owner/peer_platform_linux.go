//go:build linux
// +build linux

package owner

import muxcore "github.com/thebtf/mcp-mux/muxcore"

func peerPlatform() string { return muxcore.PlatformLinuxUnix }
