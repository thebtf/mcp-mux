// mock_server_crash.go is a minimal MCP server that responds to one initialize
// request and then exits, simulating an upstream crash after initial handshake.
//
// Used by TestUpstreamCrashDisconnectsSession to verify that Session.Close
// propagates EOF to connected IPC clients when the upstream dies.
//
// Usage: go run testdata/mock_server_crash.go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Read one request (initialize) and respond, then exit after a short delay.
	if scanner.Scan() {
		line := scanner.Bytes()

		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			os.Exit(1)
		}

		if req.Method == "initialize" {
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"protocolVersion": "2025-11-25",
					"capabilities":    map[string]interface{}{},
					"serverInfo": map[string]interface{}{
						"name":    "mock-server-crash",
						"version": "0.1.0",
					},
				},
			}
			data, _ := json.Marshal(resp)
			fmt.Fprintln(os.Stdout, string(data))
		}
	}

	// Wait briefly so the owner has time to cache the initialize response
	// before the process exits (simulates crash shortly after startup).
	time.Sleep(200 * time.Millisecond)
	os.Exit(0)
}
