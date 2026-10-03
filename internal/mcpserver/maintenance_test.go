package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func invokeMaintenanceTool(t *testing.T, baseDir, endpoint, tool, arguments string) (control.Response, bool) {
	t.Helper()
	input := fmt.Sprintf(`{"jsonrpc":"2.0","id":"maintenance-boundary","method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", tool, arguments)
	var output bytes.Buffer
	s := NewServer(strings.NewReader(input), &output, log.New(io.Discard, "", 0))
	s.BaseDir, s.DaemonCtlPath = baseDir, endpoint
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var frame struct {
		ID     string `json:"id"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := decoder.Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if frame.ID != "maintenance-boundary" || len(frame.Result.Content) != 1 {
		t.Fatalf("incorrect MCP tool framing: %+v", frame)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("duplicate MCP response: %v, %v", extra, err)
	}
	var response control.Response
	if err := json.Unmarshal([]byte(frame.Result.Content[0].Text), &response); err != nil {
		t.Fatalf("maintenance tool did not return safe JSON: %q", frame.Result.Content[0].Text)
	}
	return response, frame.Result.IsError
}

func TestMaintenanceMCPInvalidBeforeEndpointAccess(t *testing.T) {
	dir := shortBaseDir(t, "mmi-")
	endpoint := serverid.DaemonControlPath(dir, "mcp-mux")
	for _, tc := range []struct{ tool, arguments string }{
		{"mux_hold", `{}`},
		{"mux_hold", `{"server_id":" owner"}`},
		{"mux_hold", `{"server_id":"owner","hold_seconds":0}`},
		{"mux_hold", `{"server_id":"owner","hold_seconds":3601}`},
		{"mux_hold", `{"server_id":"owner","hold_seconds":1.5}`},
		{"mux_hold", `{"server_id":"owner","hold_seconds":null}`},
		{"mux_hold", `{"server_id":"owner","hold_seconds":9223372036854775808}`},
		{"mux_hold", `{"server_id":"owner","drain_timeout_ms":-1}`},
		{"mux_hold", `{"server_id":"owner","drain_timeout_ms":null}`},
		{"mux_hold", `{"server_id":"owner","drain_timeout_ms":9223372036854775807}`},
		{"mux_hold", `{"server_id":"owner","name":"substring"}`},
		{"mux_hold", `{"server_id":"owner","engine_name":"foreign"}`},
		{"mux_hold", `{"server_id":"owner","force":true}`},
		{"mux_resume", `{"hold_id":"lease","server_id":"owner"}`},
		{"mux_resume", `{"hold_id":"lease","hold_seconds":300}`},
		{"mux_resume", `{"hold_id":null}`},
		{"mux_renew", `{"hold_id":"lease ","hold_seconds":300}`},
		{"mux_renew", `{"hold_id":"lease","hold_seconds":-1}`},
		{"mux_renew", `{"hold_id":"lease","hold_seconds":"300"}`},
		{"mux_restart", `{"server_id":"owner","engine_name":"foreign"}`},
	} {
		t.Run(tc.tool+tc.arguments, func(t *testing.T) {
			response, isError := invokeMaintenanceTool(t, dir, endpoint, tc.tool, tc.arguments)
			if !isError || !errors.Is(response.Err(), control.ErrMaintenanceInvalid) || response.Maintenance != nil {
				t.Fatalf("invalid input escaped validation: response=%+v isError=%v", response, isError)
			}
		})
	}
}

func TestMaintenanceMCPUnsupportedEndpoint(t *testing.T) {
	dir := shortBaseDir(t, "mmu-")
	endpoint := serverid.DaemonControlPath(dir, "mcp-mux")
	srv, err := control.NewServer(endpoint, &fakeHandler{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	for _, tc := range []struct{ tool, arguments string }{
		{"mux_hold", `{"server_id":"exact-owner"}`},
		{"mux_hold", `{"server_id":"exact-owner","hold_seconds":3600,"drain_timeout_ms":0}`},
		{"mux_resume", `{"hold_id":"exact-lease"}`},
		{"mux_renew", `{"hold_id":"exact-lease","hold_seconds":1}`},
	} {
		response, isError := invokeMaintenanceTool(t, dir, endpoint, tc.tool, tc.arguments)
		if !isError || !errors.Is(response.Err(), control.ErrMaintenanceUnsupported) {
			t.Fatalf("unsupported operation response=%+v isError=%v", response, isError)
		}
	}
}

type maintenanceRefusingDaemon struct {
	fakeDaemonHandler
	refusal         error
	legacyCalls     atomic.Int32
	delay           time.Duration
	requests        chan control.Request
	restartResponse control.Response
}

func (h *maintenanceRefusingDaemon) HandleRestartOwner(req control.Request) (control.Response, error) {
	if h.requests != nil {
		h.requests <- req
	}
	time.Sleep(h.delay)
	return h.restartResponse, h.refusal
}

func (h *maintenanceRefusingDaemon) HandleStopOwner(control.Request) (string, error) {
	return "", h.refusal
}

func (h *maintenanceRefusingDaemon) HandleMaintenance(req control.Request) (control.MaintenanceResult, error) {
	if h.requests != nil {
		h.requests <- req
	}
	time.Sleep(h.delay)
	return control.MaintenanceResult{}, h.refusal
}

func (h *maintenanceRefusingDaemon) HandleSpawn(control.Request) (string, string, string, error) {
	h.legacyCalls.Add(1)
	return "", "", "", errors.New("unexpected fallback spawn")
}

type maintenanceLegacyTarget struct {
	fakeHandler
	shutdowns atomic.Int32
}

func (h *maintenanceLegacyTarget) HandleShutdown(int) string {
	h.shutdowns.Add(1)
	return "unexpected direct shutdown"
}

func TestMaintenanceMCPRestartAndStopRefusalsAreTerminal(t *testing.T) {
	for _, tool := range []string{"mux_restart", "mux_stop"} {
		t.Run(tool, func(t *testing.T) {
			dir := shortBaseDir(t, "mmr-")
			endpoint := serverid.DaemonControlPath(dir, "mcp-mux")
			const sid = "exact-local-owner"
			h := &maintenanceRefusingDaemon{
				fakeDaemonHandler: fakeDaemonHandler{listOwnersResp: control.ListOwnersResponse{Owners: []control.OwnerInfo{{ServerID: sid, ProtocolEra: "2026-07-28"}}}},
				refusal:           fmt.Errorf("private diagnostic: %w", control.ErrMaintenanceHeld),
			}
			srv, err := control.NewServer(endpoint, h, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(srv.Close)
			legacy := &maintenanceLegacyTarget{}
			ownerControl, err := control.NewServer(serverid.ControlPath(dir, "mcp-mux", sid), legacy, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(ownerControl.Close)
			response, isError := invokeMaintenanceTool(t, dir, endpoint, tool, `{"server_id":"`+sid+`","force":true}`)
			if !isError || !errors.Is(response.Err(), control.ErrMaintenanceHeld) || strings.Contains(response.Message, "private diagnostic") {
				t.Fatalf("refusal lost or unsafe: response=%+v isError=%v", response, isError)
			}
			if h.legacyCalls.Load() != 0 || legacy.shutdowns.Load() != 0 {
				t.Fatal("refusal performed direct shutdown or fallback spawn")
			}
		})
	}
}

func TestMaintenanceMCPRestartUnsupportedWithoutDirectOwner(t *testing.T) {
	dir := shortBaseDir(t, "mmold-")
	endpoint := serverid.DaemonControlPath(dir, "mcp-mux")
	startFakeDaemonControlServer(t, endpoint, control.ListOwnersResponse{Owners: []control.OwnerInfo{{ServerID: "exact-owner", Command: "old-server-command"}}})
	legacy := &maintenanceLegacyTarget{}
	ownerControl, err := control.NewServer(serverid.ControlPath(dir, "mcp-mux", "exact-owner"), legacy, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ownerControl.Close)
	for _, arguments := range []string{`{"server_id":"exact-owner","force":true}`, `{"name":"old-server-command","force":true}`} {
		response, isError := invokeMaintenanceTool(t, dir, endpoint, "mux_restart", arguments)
		if !isError || !errors.Is(response.Err(), control.ErrMaintenanceUnsupported) || legacy.shutdowns.Load() != 0 {
			t.Fatalf("old endpoint used direct fallback: response=%+v isError=%v shutdowns=%d", response, isError, legacy.shutdowns.Load())
		}
	}
}

func TestMaintenanceMCPForceRestartDefaultBudgetReceivesDelayedOutcome(t *testing.T) {
	dir := shortBaseDir(t, "mmrpc-")
	endpoint := serverid.DaemonControlPath(dir, "mcp-mux")
	const sid = "exact-local-owner"
	h := &maintenanceRefusingDaemon{
		fakeDaemonHandler: fakeDaemonHandler{listOwnersResp: control.ListOwnersResponse{Owners: []control.OwnerInfo{{ServerID: sid, ProtocolEra: "2026-07-28"}}}},
		delay:             5200 * time.Millisecond,
		requests:          make(chan control.Request, 4),
		restartResponse:   control.Response{OK: true, ServerID: "restarted-owner", ProtocolEra: "2026-07-28", IPCPath: "restarted-endpoint", Token: "reservation"},
	}
	srv, err := control.NewServer(endpoint, h, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	legacy := &maintenanceLegacyTarget{}
	ownerControl, err := control.NewServer(serverid.ControlPath(dir, "mcp-mux", sid), legacy, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ownerControl.Close)
	response, isError := invokeMaintenanceTool(t, dir, endpoint, "mux_restart", `{"server_id":"`+sid+`","force":true}`)
	if isError || !response.OK || response.ServerID != h.restartResponse.ServerID || response.ProtocolEra != h.restartResponse.ProtocolEra {
		t.Fatalf("delayed original restart outcome lost: response=%+v isError=%v", response, isError)
	}
	if len(h.requests) != 1 || h.legacyCalls.Load() != 0 || legacy.shutdowns.Load() != 0 {
		t.Fatalf("restart duplicated or fell back: requests=%d spawns=%d shutdowns=%d", len(h.requests), h.legacyCalls.Load(), legacy.shutdowns.Load())
	}
	if req := <-h.requests; req.Cmd != "restart_owner" || req.ServerID != sid || req.DrainTimeoutMs != 0 {
		t.Fatalf("force restart changed its selected operation: %+v", req)
	}
}

func TestMaintenanceMCPDefaultBudgetReceivesDelayedHoldRefusal(t *testing.T) {
	dir := shortBaseDir(t, "mmholdrpc-")
	endpoint := serverid.DaemonControlPath(dir, "mcp-mux")
	result := &control.MaintenanceResult{
		HoldID: "exact-lease", ServerID: "exact-owner", State: control.MaintenanceRetirementBlocked,
		ExpiresAt: time.Now().Add(time.Minute).UTC(), DrainDeadline: time.Now().UTC(),
	}
	h := &maintenanceRefusingDaemon{
		refusal:  &control.MaintenanceError{Code: control.ErrMaintenanceRetirementBlocked.Code, Result: result},
		delay:    5200 * time.Millisecond,
		requests: make(chan control.Request, 4),
	}
	srv, err := control.NewServer(endpoint, h, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	response, isError := invokeMaintenanceTool(t, dir, endpoint, "mux_hold", `{"server_id":"exact-owner","drain_timeout_ms":0}`)
	if !isError || !errors.Is(response.Err(), control.ErrMaintenanceRetirementBlocked) || response.Maintenance == nil || response.Maintenance.HoldID != result.HoldID || response.Maintenance.State != result.State || response.Maintenance.TreesRetired {
		t.Fatalf("delayed original blocked hold outcome lost: response=%+v isError=%v", response, isError)
	}
	if len(h.requests) != 1 || h.legacyCalls.Load() != 0 {
		t.Fatalf("hold duplicated or fell back: requests=%d spawns=%d", len(h.requests), h.legacyCalls.Load())
	}
	if req := <-h.requests; req.Cmd != "hold" || req.ServerID != result.ServerID || req.DrainTimeoutMs != 0 {
		t.Fatalf("hold retried, released, or changed its selection: %+v", req)
	}
}

func TestMaintenanceMCPRefusalReadbackIsSafe(t *testing.T) {
	dir := shortBaseDir(t, "mmread-")
	endpoint := serverid.DaemonControlPath(dir, "mcp-mux")
	result := &control.MaintenanceResult{HoldID: "exact-lease", ServerID: "exact-owner", State: control.MaintenanceRetirementBlocked, ExpiresAt: time.Now().Add(time.Minute).UTC(), DrainDeadline: time.Now().UTC()}
	h := &maintenanceRefusingDaemon{refusal: fmt.Errorf("private diagnostic: %w", &control.MaintenanceError{Code: control.ErrMaintenanceRetirementBlocked.Code, Result: result})}
	srv, err := control.NewServer(endpoint, h, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	response, isError := invokeMaintenanceTool(t, dir, endpoint, "mux_resume", `{"hold_id":"exact-lease"}`)
	if !isError || !errors.Is(response.Err(), control.ErrMaintenanceRetirementBlocked) || response.Maintenance == nil || response.Maintenance.HoldID != "exact-lease" || response.Maintenance.TreesRetired || strings.Contains(response.Message, "private diagnostic") {
		t.Fatalf("blocked retirement readback lost or unsafe: %+v", response)
	}
}
