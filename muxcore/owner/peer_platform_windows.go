//go:build windows
// +build windows

package owner

import muxcore "github.com/thebtf/mcp-mux/muxcore"

func peerPlatform() string { return muxcore.PlatformWindowsNamedPipe }
