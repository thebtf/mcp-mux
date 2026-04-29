package muxcore

// ConnInfo carries OS-level peer identity for an established session connection.
// Populated once at accept time via peerCreds(conn) and cached for the lifetime
// of the session. ConnInfo holds OS facts only; consumer-policy fields such as
// TenantID live on SessionMeta.
//
// Zero-value semantics: PeerPid==0 / PeerUid==0 are valid runtime states that
// signal extraction failure (unsupported transport, syscall error, peer process
// exited before extraction, or a Windows named pipe with no UID concept).
// Callers MUST treat zero values as "unavailable", not as "process 0".
type ConnInfo struct {
	// PeerPid is the OS process ID of the connected peer; 0 if unavailable.
	PeerPid int
	// PeerUid is the Unix UID of the connected peer; 0 on Windows or when
	// SO_PEERCRED / getpeereid extraction failed.
	PeerUid int
	// Platform identifies the transport family of this connection. Use the
	// PlatformXxx constants below for stable comparisons.
	Platform string
}

// Platform identifiers carried by ConnInfo.Platform. Consumers MUST compare
// against these constants rather than literal strings — values are stable
// across muxcore versions but the source of truth lives here.
const (
	// PlatformLinuxUnix marks an SO_PEERCRED-capable Unix domain socket
	// connection accepted on Linux.
	PlatformLinuxUnix = "linux-unix-stream"

	// PlatformWindowsNamedPipe marks a winio named-pipe connection on Windows.
	// PeerUid is always 0 on this platform (no UID concept comparable to Unix).
	PlatformWindowsNamedPipe = "windows-named-pipe"

	// PlatformDarwinUnix marks a LOCAL_PEERPID-capable Unix domain socket
	// connection accepted on macOS.
	PlatformDarwinUnix = "darwin-unix-stream"

	// PlatformUnknown is the default when peerCreds cannot determine the
	// transport family (e.g., the dispatcher was passed a net.Conn that does
	// not match any known extractor).
	PlatformUnknown = "unknown"
)
