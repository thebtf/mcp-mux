// mock_modern_server.go is a deterministic MCP 2026-07-28 upstream fixture.
// It captures the exact inbound JSON text before responding with native modern
// results. It deliberately does not implement mcp-mux behavior.
//
// Usage: go run testdata/mock_modern_server.go
//
// Test-only environment controls:
//   - MCP_MUX_MODERN_CAPTURE_FILE writes each inbound frame exactly, with LF framing.
//   - MCP_MUX_MODERN_MODE selects input_required, request_log, server_request,
//     loss_before_result, or loss_after_result. The default is native results.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	fixtureProtocolVersion = "2026-07-28"
	fixtureCaptureEnv      = "MCP_MUX_MODERN_CAPTURE_FILE"
	fixtureModeEnv         = "MCP_MUX_MODERN_MODE"
	fixtureLossExitCode    = 23
	fixtureMaxFrameBytes   = 1024 * 1024
)

var errControlledLoss = errors.New("controlled modern fixture loss")

type fixtureMode string

const (
	fixtureModeNative           fixtureMode = ""
	fixtureModeInputRequired    fixtureMode = "input_required"
	fixtureModeRequestLog       fixtureMode = "request_log"
	fixtureModeServerRequest    fixtureMode = "server_request"
	fixtureModeLossBeforeResult fixtureMode = "loss_before_result"
	fixtureModeLossAfterResult  fixtureMode = "loss_after_result"
)

type modernRequest struct {
	ID       json.RawMessage
	Method   string
	LogLevel string
}

type fixtureRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type fixtureResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *fixtureRPCError `json:"error,omitempty"`
}

type frameCapture struct {
	file *os.File
}

func main() {
	if err := runModernFixture(os.Stdin, os.Stdout, os.Getenv); err != nil {
		if errors.Is(err, errControlledLoss) {
			os.Exit(fixtureLossExitCode)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runModernFixture(input io.Reader, output io.Writer, getenv func(string) string) error {
	mode, err := parseFixtureMode(getenv(fixtureModeEnv))
	if err != nil {
		return err
	}
	capture, err := openCapture(getenv(fixtureCaptureEnv))
	if err != nil {
		return err
	}
	if capture != nil {
		defer capture.file.Close()
	}

	reader := bufio.NewReaderSize(input, fixtureMaxFrameBytes)
	writer := bufio.NewWriter(output)
	defer writer.Flush()

	for {
		rawFrame, err := readFrame(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(rawFrame)) == 0 {
			continue
		}
		if capture != nil {
			if err := capture.write(rawFrame); err != nil {
				return err
			}
		}

		request, problem := parseModernRequest(rawFrame)
		if problem != nil {
			if err := writeError(writer, request.ID, problem); err != nil {
				return err
			}
			if err := writer.Flush(); err != nil {
				return err
			}
			continue
		}
		if mode == fixtureModeLossBeforeResult {
			return errControlledLoss
		}
		if mode == fixtureModeServerRequest {
			if err := writeServerRequest(writer); err != nil {
				return err
			}
		}
		if mode == fixtureModeRequestLog && request.LogLevel != "" {
			if err := writeRequestScopedLog(writer); err != nil {
				return err
			}
		}

		if mode == fixtureModeInputRequired {
			err = writeResult(writer, request.ID, inputRequiredResult())
		} else {
			result, methodError := nativeResult(request.Method)
			if methodError != nil {
				err = writeError(writer, request.ID, methodError)
			} else {
				err = writeResult(writer, request.ID, result)
			}
		}
		if err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		if mode == fixtureModeLossAfterResult {
			return errControlledLoss
		}
	}
}

func parseFixtureMode(raw string) (fixtureMode, error) {
	switch fixtureMode(raw) {
	case fixtureModeNative,
		fixtureModeInputRequired,
		fixtureModeRequestLog,
		fixtureModeServerRequest,
		fixtureModeLossBeforeResult,
		fixtureModeLossAfterResult:
		return fixtureMode(raw), nil
	default:
		return fixtureModeNative, fmt.Errorf("unknown %s %q", fixtureModeEnv, raw)
	}
}

func openCapture(path string) (*frameCapture, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open modern capture %q: %w", path, err)
	}
	return &frameCapture{file: file}, nil
}

func (capture *frameCapture) write(rawFrame []byte) error {
	if _, err := capture.file.Write(rawFrame); err != nil {
		return fmt.Errorf("write modern capture: %w", err)
	}
	if _, err := capture.file.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("frame modern capture: %w", err)
	}
	return nil
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	rawFrame, err := reader.ReadBytes('\n')
	if len(rawFrame) > fixtureMaxFrameBytes {
		return nil, fmt.Errorf("modern fixture frame exceeds %d bytes", fixtureMaxFrameBytes)
	}
	if errors.Is(err, io.EOF) {
		if len(rawFrame) == 0 {
			return nil, io.EOF
		}
	} else if err != nil {
		return nil, err
	}
	rawFrame = bytes.TrimSuffix(rawFrame, []byte{'\n'})
	return rawFrame, nil
}

func parseModernRequest(rawFrame []byte) (modernRequest, *fixtureRPCError) {
	parseFrame := bytes.TrimSuffix(rawFrame, []byte{'\r'})
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(parseFrame, &envelope); err != nil {
		return modernRequest{}, &fixtureRPCError{Code: -32700, Message: "Parse error"}
	}

	request := modernRequest{ID: envelope["id"]}
	var jsonrpc string
	if err := json.Unmarshal(envelope["jsonrpc"], &jsonrpc); err != nil || jsonrpc != "2.0" {
		return request, &fixtureRPCError{Code: -32600, Message: "Invalid Request"}
	}
	if len(request.ID) == 0 || bytes.Equal(bytes.TrimSpace(request.ID), []byte("null")) {
		return request, &fixtureRPCError{Code: -32600, Message: "Invalid Request"}
	}
	if err := json.Unmarshal(envelope["method"], &request.Method); err != nil || request.Method == "" {
		return request, &fixtureRPCError{Code: -32600, Message: "Invalid Request"}
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(envelope["params"], &params); err != nil || params == nil {
		return request, invalidModernParams()
	}
	metaRaw, found := params["_meta"]
	if !found {
		return request, invalidModernParams()
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(metaRaw, &meta); err != nil || meta == nil {
		return request, invalidModernParams()
	}

	var version string
	if err := json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &version); err != nil || version == "" {
		return request, invalidModernParams()
	}
	if version != fixtureProtocolVersion {
		return request, &fixtureRPCError{
			Code:    -32022,
			Message: "Unsupported protocol version",
			Data: map[string]any{
				"supported": []string{fixtureProtocolVersion},
				"requested": version,
			},
		}
	}

	capabilitiesRaw, found := meta["io.modelcontextprotocol/clientCapabilities"]
	if !found {
		return request, invalidModernParams()
	}
	var capabilities map[string]json.RawMessage
	if err := json.Unmarshal(capabilitiesRaw, &capabilities); err != nil || capabilities == nil {
		return request, invalidModernParams()
	}
	if clientInfoRaw, present := meta["io.modelcontextprotocol/clientInfo"]; present {
		var clientInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(clientInfoRaw, &clientInfo); err != nil || clientInfo.Name == "" || clientInfo.Version == "" {
			return request, invalidModernParams()
		}
	}
	if logLevelRaw, present := meta["io.modelcontextprotocol/logLevel"]; present {
		if err := json.Unmarshal(logLevelRaw, &request.LogLevel); err != nil || !validLogLevel(request.LogLevel) {
			return request, invalidModernParams()
		}
	}
	return request, nil
}

func invalidModernParams() *fixtureRPCError {
	return &fixtureRPCError{Code: -32602, Message: "Invalid params"}
}

func validLogLevel(level string) bool {
	switch level {
	case "debug", "info", "notice", "warning", "error", "critical", "alert", "emergency":
		return true
	default:
		return false
	}
}

func nativeResult(method string) (any, *fixtureRPCError) {
	switch method {
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{
			"tools": []map[string]any{
				{
					"name":        "modern_echo",
					"description": "Returns modern fixture input unchanged",
					"inputSchema": map[string]any{
						"type": "object",
					},
				},
			},
		}, nil
	case "tools/call":
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "modern fixture tool result"},
			},
		}, nil
	case "server/discover":
		return map[string]any{
			"resultType":        "complete",
			"supportedVersions": []string{fixtureProtocolVersion},
			"capabilities": map[string]any{
				"tools":   map[string]any{},
				"logging": map[string]any{},
			},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]string{
					"name":    "mock-modern-server",
					"version": "0.1.0",
				},
			},
		}, nil
	default:
		return nil, &fixtureRPCError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", method)}
	}
}

func inputRequiredResult() map[string]any {
	return map[string]any{
		"resultType": "input_required",
		"inputRequests": map[string]any{
			"fixture_confirmation": map[string]any{
				"method": "elicitation/create",
				"params": map[string]any{
					"mode":    "form",
					"message": "Confirm the deterministic fixture request",
					"requestedSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"confirmed": map[string]string{"type": "boolean"},
						},
						"required": []string{"confirmed"},
					},
				},
			},
		},
		"requestState": "fixture-opaque-request-state-v1",
	}
}

func writeRequestScopedLog(writer *bufio.Writer) error {
	return writeJSON(writer, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/message",
		"params": map[string]any{
			"level":  "info",
			"logger": "mock-modern-server",
			"data":   "request-scoped fixture log",
		},
	})
}

func writeServerRequest(writer *bufio.Writer) error {
	return writeJSON(writer, map[string]any{
		"jsonrpc": "2.0",
		"id":      "fixture-server-request-1",
		"method":  "sampling/createMessage",
		"params": map[string]any{
			"messages": []map[string]any{
				{
					"role":    "user",
					"content": map[string]string{"type": "text", "text": "This request must be contained by the mux."},
				},
			},
			"maxTokens": 16,
		},
	})
}

func writeResult(writer *bufio.Writer, id json.RawMessage, result any) error {
	return writeJSON(writer, fixtureResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func writeError(writer *bufio.Writer, id json.RawMessage, problem *fixtureRPCError) error {
	return writeJSON(writer, fixtureResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   problem,
	})
}

func writeJSON(writer *bufio.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
