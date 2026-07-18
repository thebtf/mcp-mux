package attest

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func TestParentChildDirectPairAttests(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	advertisement := parent.Advertisement()
	if advertisement.Version != VersionV2 || advertisement.ParentPID != os.Getpid() || advertisement.Endpoint == "" {
		t.Fatalf("advertisement = %#v", advertisement)
	}
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   advertisement,
		DirectParentPID: os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parent.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("parent receipt did not finish")
	}
	if !parent.Verified() {
		t.Fatal("parent did not record exact child receipt")
	}
}

func TestParentAcceptsChildThatConnectsBeforeBind(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	result := make(chan error, 1)
	go func() {
		result <- VerifyParent(context.Background(), VerifyConfig{
			Advertisement:   parent.Advertisement(),
			DirectParentPID: os.Getpid(),
			IOTimeout:       500 * time.Millisecond,
		})
	}()
	time.Sleep(25 * time.Millisecond)
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("pre-bind connection failed: %v", err)
	}
	if !parent.Verified() {
		t.Fatal("pre-bind connection did not produce receipt")
	}
}

func TestParentRejectsWrongChildPIDAndContinues(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid() + 1000); err != nil {
		t.Fatal(err)
	}
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   parent.Advertisement(),
		DirectParentPID: os.Getpid(),
		IOTimeout:       100 * time.Millisecond,
	}); err == nil {
		t.Fatal("wrong child PID unexpectedly attested")
	}
	if parent.Verified() {
		t.Fatal("wrong child PID produced a receipt")
	}
	select {
	case <-parent.Done():
		t.Fatal("wrong child PID closed the reusable receipt before expiry")
	default:
	}
}

func TestParentRejectsMalformedExchangeThenAcceptsValidChild(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	conn, err := ipc.DialTimeout(parent.Advertisement().Endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "not-attestation\n")
	_ = conn.Close()
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   parent.Advertisement(),
		DirectParentPID: os.Getpid(),
	}); err != nil {
		t.Fatalf("valid child after malformed peer: %v", err)
	}
	if !parent.Verified() {
		t.Fatal("valid child after malformed peer did not verify")
	}
}

func TestVerifyParentRejectsForwardedAdvertisement(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   parent.Advertisement(),
		DirectParentPID: os.Getpid() + 1,
	}); err == nil {
		t.Fatal("forwarded parent advertisement unexpectedly verified")
	}
}

func TestVerifyParentRejectsWrongServerPID(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	advertisement := parent.Advertisement()
	advertisement.ParentPID++
	if err := VerifyParent(context.Background(), VerifyConfig{
		Advertisement:   advertisement,
		DirectParentPID: advertisement.ParentPID,
	}); err == nil {
		t.Fatal("endpoint server PID mismatch unexpectedly verified")
	}
	if parent.Verified() {
		t.Fatal("server PID mismatch produced a parent receipt")
	}
}

func TestParentExpiresAndCleansEndpoint(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{Lifetime: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := parent.Advertisement().Endpoint
	select {
	case <-parent.Done():
	case <-time.After(time.Second):
		t.Fatal("parent did not expire")
	}
	if parent.Verified() {
		t.Fatal("expired parent unexpectedly verified")
	}
	if conn, err := ipc.DialTimeout(endpoint, 50*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("expired endpoint remained connectable")
	}
}

func TestParentCannotVerifyAfterLifetimeThroughAcceptedConnection(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{
		Lifetime:  40 * time.Millisecond,
		IOTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.BindChildPID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	conn, err := ipc.DialTimeout(parent.Advertisement().Endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, attestationRequest[:len(attestationRequest)-1]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parent.Done():
	case <-time.After(time.Second):
		t.Fatal("parent did not expire with an accepted connection")
	}
	_, _ = io.WriteString(conn, attestationRequest[len(attestationRequest)-1:])
	time.Sleep(25 * time.Millisecond)
	if parent.Verified() {
		t.Fatal("accepted connection produced a receipt after expiry")
	}
}

func TestBindChildPIDAfterCloseIsRejected(t *testing.T) {
	parent, err := StartParent(context.Background(), ParentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.BindChildPID(os.Getpid()); err == nil {
		t.Fatal("closed parent accepted a child PID")
	}
}

// TestRandomEndpointFitsDarwinPathCeiling is the regression guard for
// https://github.com/thebtf/mcp-mux/pull/145 CI run 29645121822.
//
// Root cause: the macOS GitHub Actions runner uses
//   /var/folders/8j/sfr9qqcj73j4p6nhwcfpr0th0000gn/T
// as os.TempDir() (48 bytes). The pre-fix randomEndpoint combined that with
// "mcp-supervisor-attest-<32hex>.sock" producing a 108-byte path, exceeding
// Darwin's RawSockaddrUnix.Path ceiling of 104 bytes and causing
// bind(2) to fail with EINVAL.
//
// The test models the exact CI temp-dir length and verifies that
// randomEndpointIn produces a path ≤ unixPathMax-1 (103 usable bytes).
func TestRandomEndpointFitsDarwinPathCeiling(t *testing.T) {
	// Reproduce the exact 48-byte macOS CI temp directory from run 29645121822.
	darwinRunnerTempDir := "/var/folders/8j/sfr9qqcj73j4p6nhwcfpr0th0000gn/T"
	if len(darwinRunnerTempDir) != 48 {
		t.Fatalf("test setup: expected 48-byte dir, got %d", len(darwinRunnerTempDir))
	}

	endpoint, err := randomEndpointIn(darwinRunnerTempDir)
	if err != nil {
		t.Fatalf("randomEndpointIn failed for Darwin runner temp dir: %v", err)
	}

	const usable = unixPathMax - 1 // 103 bytes; the kernel reserves one byte for the null terminator
	if got := len(endpoint); got >= unixPathMax {
		t.Errorf("endpoint length %d >= unixPathMax %d (would fail bind on Darwin): %q",
			got, unixPathMax, endpoint)
	} else {
		t.Logf("endpoint length %d / %d usable: %q", got, usable, endpoint)
	}
}

// TestRandomEndpointPreFixWouldExceedDarwinCeiling confirms that the pre-fix
// construction (32 hex chars from a 16-byte random value) produces a path that
// exceeds the Darwin ceiling when rooted in the CI runner temp directory,
// encoding the exact defect that caused CI run 29645121822 to fail.
func TestRandomEndpointPreFixWouldExceedDarwinCeiling(t *testing.T) {
	darwinRunnerTempDir := "/var/folders/8j/sfr9qqcj73j4p6nhwcfpr0th0000gn/T"
	// Pre-fix basename: "mcp-supervisor-attest-" (22) + 32 hex chars + ".sock" (5) = 59 bytes.
	// Full pre-fix path: 48 (dir) + 1 (sep) + 59 = 108 bytes.
	preFix32HexChars := "00000000000000000000000000000000" // 32 chars, representative of hex.EncodeToString([16]byte)
	prePath := darwinRunnerTempDir + "/" + "mcp-supervisor-attest-" + preFix32HexChars + ".sock"
	if got := len(prePath); got < unixPathMax {
		t.Errorf("pre-fix path length %d < unixPathMax %d; test assumption wrong — re-check the CI path", got, unixPathMax)
	} else {
		t.Logf("confirmed: pre-fix path length %d >= unixPathMax %d (would cause EINVAL on Darwin)", got, unixPathMax)
	}
}
