//go:build !windows && !linux && !darwin

package owner

import muxcore "github.com/thebtf/mcp-mux/muxcore"

// Unsupported peer-credential platforms retain the documented zero-value
// identity semantics rather than blocking builds or inventing an OS identity.
func peerPlatform() string { return muxcore.PlatformUnknown }
