// mock_server.go is a minimal MCP server for testing mcp-mux.
// It reads JSON-RPC from stdin, responds on stdout.
//
// Supported methods:
//   - initialize: returns server info + capabilities
//   - tools/list: returns a list of mock tools
//   - tools/call: echoes back the tool name and arguments
//   - ping: returns pong
//
// Usage: go run testdata/mock_server.go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			writeError(nil, -32700, fmt.Sprintf("Parse error: %v", err))
			continue
		}

		switch req.Method {
		case "initialize":
			writeResult(req.ID, map[string]interface{}{
				"protocolVersion": "2025-11-25",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "mock-server",
					"version": "0.1.0",
				},
			})

		case "notifications/initialized":
			// Notification — no response needed
			continue

		case "tools/list":
			writeResult(req.ID, map[string]interface{}{
				"tools": []map[string]interface{}{
					{
						"name":        "echo",
						"description": "Echoes input back",
						"inputSchema": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"message": map[string]interface{}{"type": "string"},
							},
						},
					},
					{
						"name":        "add",
						"description": "Adds two numbers",
						"inputSchema": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"a": map[string]interface{}{"type": "number"},
								"b": map[string]interface{}{"type": "number"},
							},
						},
					},
					{
						"name":        "trigger_ping",
						"description": "Sends a server→client ping before responding",
						"inputSchema": map[string]interface{}{
							"type": "object",
						},
					},
					{
						"name":        "request_sampling",
						"description": "Sends a sampling/createMessage to the client and uses the result",
						"inputSchema": map[string]interface{}{
							"type": "object",
						},
					},
				},
			})

		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if req.Params != nil {
				_ = json.Unmarshal(req.Params, &params)
			}

			switch params.Name {
			case "trigger_ping":
				// Send a server→client ping before responding; the mux owner handles it locally
				fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","id":"ping-1","method":"ping","params":{}}`)
				// Read the ping response (owner will respond; we consume it here)
				if scanner.Scan() {
					// ping response consumed — proceed
				}
				writeResult(req.ID, map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": "ping acknowledged"},
					},
				})

			case "request_sampling":
				// Send sampling/createMessage to client and wait for response
				fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","id":"s1","method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"sample this"}}],"maxTokens":100}}`)
				// Read the sampling response from client (forwarded by mux)
				sampledText := "no response"
				if scanner.Scan() {
					var samplingResp struct {
						Result struct {
							Content struct {
								Text string `json:"text"`
							} `json:"content"`
						} `json:"result"`
					}
					if err := json.Unmarshal(scanner.Bytes(), &samplingResp); err == nil {
						sampledText = samplingResp.Result.Content.Text
					}
				}
				writeResult(req.ID, map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": fmt.Sprintf("sampled: %s", sampledText)},
					},
				})

			default:
				writeResult(req.ID, map[string]interface{}{
					"content": []map[string]interface{}{
						{
							"type": "text",
							"text": fmt.Sprintf("Tool %s called with args: %s", params.Name, string(params.Arguments)),
						},
					},
				})
			}

		case "prompts/list":
			writeResult(req.ID, map[string]interface{}{
				"prompts": []map[string]interface{}{
					{"name": "greeting", "description": "A greeting prompt"},
				},
			})

		case "ping":
			writeResult(req.ID, map[string]interface{}{})

		default:
			writeError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
		}
	}
}

func writeResult(id json.RawMessage, result interface{}) {
	resp := response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintln(os.Stdout, string(data))
}

func writeError(id json.RawMessage, code int, message string) {
	resp := response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintln(os.Stdout, string(data))
}
