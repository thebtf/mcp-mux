//go:build windows

package attest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

func TestRandomEndpointLongLogicalTempListensOnHashedPipe(t *testing.T) {
	logicalTemp := filepath.Join(`C:\logical-temp`, strings.Repeat("very-long-segment-", 24))
	endpoint, err := randomEndpointIn(logicalTemp)
	if err != nil {
		t.Fatalf("randomEndpointIn long logical temp: %v", err)
	}
	if len(endpoint) <= unixPathMax {
		t.Fatalf("test endpoint length = %d, want longer than Unix ceiling", len(endpoint))
	}
	base := filepath.Base(endpoint)
	randomHex := strings.TrimSuffix(strings.TrimPrefix(base, "mcp-supervisor-attest-"), ".sock")
	if len(randomHex) != 32 {
		t.Fatalf("random suffix length = %d, want 32 hex chars", len(randomHex))
	}
	listener, err := ipc.Listen(endpoint)
	if err != nil {
		t.Fatalf("listen hashed pipe for long logical temp: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close hashed pipe: %v", err)
	}
	ipc.Cleanup(endpoint)
}
