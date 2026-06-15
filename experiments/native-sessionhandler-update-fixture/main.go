package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/engine"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

const (
	engineName = "native-sessionhandler-update-fixture"
	daemonFlag = "--fixture-daemon"
	envBaseDir = "NATIVE_FIXTURE_BASE_DIR"
	envLogPath = "NATIVE_FIXTURE_LOG_PATH"
)

var productVersion = "dev"

type fixtureHandler struct {
	eng    *engine.MuxEngine
	logger *log.Logger
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func main() {
	var successorExe string
	var showStatus bool
	var shutdown bool
	var daemonMode bool
	var ipcSelfTest bool
	var launchDaemonProbe bool
	flag.StringVar(&successorExe, "fixture-apply-successor", "", "restart the fixture daemon with this successor executable")
	flag.BoolVar(&showStatus, "fixture-status", false, "print fixture daemon status")
	flag.BoolVar(&shutdown, "fixture-shutdown", false, "shutdown the fixture daemon")
	flag.BoolVar(&daemonMode, "fixture-daemon", false, "run muxcore daemon mode")
	flag.BoolVar(&ipcSelfTest, "fixture-ipc-self-test", false, "run an IPC listen/dial/accept self-test")
	flag.BoolVar(&launchDaemonProbe, "fixture-launch-daemon-probe", false, "launch daemon from this binary and query status")
	flag.Parse()

	logger, closeLog := fixtureLogger()
	defer closeLog()

	handler := &fixtureHandler{logger: logger}
	eng, err := engine.New(engine.Config{
		Name:           engineName,
		SessionHandler: handler,
		BaseDir:        os.Getenv(envBaseDir),
		DaemonFlag:     daemonFlag,
		Persistent:     true,
		IdleTimeout:    10 * time.Second,
		Logger:         logger,
	})
	if err != nil {
		fatalJSON(err)
	}
	handler.eng = eng

	switch {
	case ipcSelfTest:
		runIPCSelfTest(eng)
	case launchDaemonProbe:
		runLaunchDaemonProbe(eng)
	case successorExe != "":
		applySuccessor(eng, successorExe)
	case showStatus:
		printStatus(eng)
	case shutdown:
		sendShutdown(eng)
	default:
		if err := eng.Run(context.Background()); err != nil {
			fatalJSON(err)
		}
	}
}

func runLaunchDaemonProbe(eng *engine.MuxEngine) {
	exe, err := os.Executable()
	if err != nil {
		fatalJSON(err)
	}
	cmd := exec.Command(exe, daemonFlag)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		fatalJSON(err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		fatalJSON(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		fatalJSON(err)
	}
	time.Sleep(500 * time.Millisecond)
	status, err := statusPayload(eng)
	if err != nil {
		fatalJSON(err)
	}
	status["launched_pid"] = pid
	writeJSON(status)
}

func runIPCSelfTest(eng *engine.MuxEngine) {
	path := eng.ControlSocketPath() + ".self-test"
	ln, err := ipc.Listen(path)
	if err != nil {
		fatalJSON(fmt.Errorf("listen: %w", err))
	}
	defer ln.Close()

	accepted := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accepted <- err
			return
		}
		conn.Close()
		accepted <- nil
	}()

	conn, err := ipc.DialTimeout(path, 5*time.Second)
	if err != nil {
		fatalJSON(fmt.Errorf("dial: %w", err))
	}
	conn.Close()

	select {
	case err := <-accepted:
		if err != nil {
			fatalJSON(fmt.Errorf("accept: %w", err))
		}
	case <-time.After(5 * time.Second):
		fatalJSON(errors.New("accept timeout"))
	}
	writeJSON(map[string]any{"ok": true, "path": path, "product_version": productVersion})
}

func fixtureLogger() (*log.Logger, func()) {
	writer := io.Writer(os.Stderr)
	closeFn := func() {}
	if logPath := os.Getenv(envLogPath); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			writer = io.MultiWriter(os.Stderr, f)
			closeFn = func() { _ = f.Close() }
		}
	}
	return log.New(writer, "[native-fixture] ", log.LstdFlags|log.Lmicroseconds), closeFn
}

func applySuccessor(eng *engine.MuxEngine, successorExe string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := eng.RestartWithSuccessor(ctx, engine.RestartWithSuccessorOptions{
		SuccessorExe:    successorExe,
		DrainTimeout:    2 * time.Second,
		RestartTimeout:  10 * time.Second,
		ShutdownTimeout: 30 * time.Second,
		ReadyTimeout:    60 * time.Second,
	})
	payload := map[string]any{
		"ok":              err == nil,
		"product_version": productVersion,
		"successor_exe":   successorExe,
		"result":          result,
	}
	if err != nil {
		payload["error"] = err.Error()
		var updateErr *engine.UpdateAndRestartError
		if errors.As(err, &updateErr) {
			payload["phase"] = updateErr.Phase
			payload["partial_result"] = updateErr.Result
		}
		writeJSON(payload)
		os.Exit(1)
	}
	writeJSON(payload)
}

func printStatus(eng *engine.MuxEngine) {
	status, err := statusPayload(eng)
	if err != nil {
		fatalJSON(err)
	}
	writeJSON(status)
}

func sendShutdown(eng *engine.MuxEngine) {
	resp, err := control.Send(eng.ControlSocketPath(), control.Request{Cmd: "shutdown"})
	if err != nil {
		fatalJSON(err)
	}
	writeJSON(map[string]any{"ok": resp.OK, "message": resp.Message})
}

func (h *fixtureHandler) HandleRequest(ctx context.Context, project muxcore.ProjectContext, request []byte) ([]byte, error) {
	var req rpcRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return rpcError(nil, -32700, "parse error", err.Error()), nil
	}
	if h.logger != nil {
		exe, _ := os.Executable()
		h.logger.Printf("handler.request method=%q pid=%d exe=%q product_version=%q project_id=%q", req.Method, os.Getpid(), exe, productVersion, project.ID)
	}
	switch req.Method {
	case "initialize":
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{
				"name":    engineName,
				"version": productVersion,
			},
		}), nil
	case "tools/list":
		return rpcResult(req.ID, map[string]any{
			"tools": []map[string]any{
				{
					"name":        "fixture_status",
					"description": "Report native muxcore fixture update status.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
				},
			},
		}), nil
	case "tools/call":
		return h.handleToolCall(req), nil
	default:
		return rpcError(req.ID, -32601, "method not found", req.Method), nil
	}
}

func (h *fixtureHandler) handleToolCall(req rpcRequest) []byte {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, -32602, "invalid params", err.Error())
	}
	if params.Name != "fixture_status" {
		return rpcError(req.ID, -32602, "unknown tool", params.Name)
	}
	payload, err := statusPayload(h.eng)
	if err != nil {
		return rpcError(req.ID, -32000, "status failed", err.Error())
	}
	text, err := json.Marshal(payload)
	if err != nil {
		return rpcError(req.ID, -32000, "status marshal failed", err.Error())
	}
	return rpcResult(req.ID, map[string]any{
		"content": []map[string]string{
			{"type": "text", "text": string(text)},
		},
	})
}

func statusPayload(eng *engine.MuxEngine) (map[string]any, error) {
	resp, err := control.Send(eng.ControlSocketPath(), control.Request{Cmd: "status"})
	if err != nil {
		return nil, err
	}
	exe, _ := os.Executable()
	payload := map[string]any{
		"product_version": productVersion,
		"engine_name":     engineName,
		"handler_pid":     os.Getpid(),
		"executable":      exe,
		"control_ok":      resp.OK,
		"control_message": resp.Message,
		"status":          resp.Data,
	}
	return payload, nil
}

func rpcResult(id json.RawMessage, result any) []byte {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"result":  result,
	}
	if len(id) > 0 {
		resp["id"] = id
	}
	data, _ := json.Marshal(resp)
	return data
}

func rpcError(id json.RawMessage, code int, message, dataText string) []byte {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    code,
			"message": message,
			"data":    dataText,
		},
	}
	if len(id) > 0 {
		resp["id"] = id
	}
	data, _ := json.Marshal(resp)
	return data
}

func writeJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal JSON: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

func fatalJSON(err error) {
	data, marshalErr := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	if marshalErr != nil {
		fmt.Fprintf(os.Stderr, "marshal JSON: %v\n", marshalErr)
	} else {
		fmt.Fprintln(os.Stderr, string(data))
	}
	os.Exit(1)
}
