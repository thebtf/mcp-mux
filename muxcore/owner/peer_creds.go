package owner

import (
	"net"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
)

// peerCreds extracts OS-level peer credentials from a connected net.Conn and
// returns a normalised muxcore.ConnInfo. Platform-specific readPeerPID and
// readPeerUID helpers (selected by build tag) deliver the raw values; this
// dispatcher normalises the -1 sentinel returned by failing extractors to 0
// per the "0 == unavailable" contract documented on muxcore.ConnInfo.
//
// Called once per session in Owner.acceptLoop after the IPC handshake
// completes (FR-5). The result is cached on Session.meta and reused for the
// lifetime of the connection.
func peerCreds(conn net.Conn) muxcore.ConnInfo {
	return muxcore.ConnInfo{
		PeerPid:  normaliseCred(readPeerPID(conn)),
		PeerUid:  normaliseCred(readPeerUID(conn)),
		Platform: peerPlatform(),
	}
}

// normaliseCred maps the -1 "extraction failed" sentinel emitted by the
// platform-specific extractors to 0, matching ConnInfo's documented
// zero-value-means-unavailable contract.
func normaliseCred(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
