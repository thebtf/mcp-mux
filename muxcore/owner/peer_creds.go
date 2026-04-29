package owner

import (
	"net"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
)

// peerCreds extracts OS-level peer credentials from a connected net.Conn and
// returns a normalised muxcore.ConnInfo. Platform-specific readPeerCreds
// helpers (selected by build tag) deliver pid+uid in a SINGLE syscall where
// the underlying OS supports it (Linux SO_PEERCRED returns the full ucred
// struct; on macOS/Windows pid and uid come from separate getsockopt calls
// because the kernels expose them separately). The dispatcher normalises the
// -1 sentinel returned by failing extractors to 0 per the "0 == unavailable"
// contract documented on muxcore.ConnInfo.
//
// Called once per session in Owner.acceptLoop after the IPC handshake
// completes (FR-5). The result is cached on Session.meta and reused for the
// lifetime of the connection.
func peerCreds(conn net.Conn) muxcore.ConnInfo {
	pid, uid := readPeerCreds(conn)
	return muxcore.ConnInfo{
		PeerPid:  normaliseCred(pid),
		PeerUid:  normaliseCred(uid),
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
