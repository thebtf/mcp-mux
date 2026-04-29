//go:build darwin
// +build darwin

package owner

import muxcore "github.com/thebtf/mcp-mux/muxcore"

func peerPlatform() string { return muxcore.PlatformDarwinUnix }
