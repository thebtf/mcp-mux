package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

func maintenanceTools() []map[string]any {
	ttl := map[string]any{"type": "integer", "default": 300, "minimum": 1, "maximum": 3600, "description": "Hold duration in seconds, at most one hour."}
	return []map[string]any{
		{
			"name":        "mux_hold",
			"description": "Hold one exact locally managed server for executable replacement. Success requires full managed-tree retirement; no foreign-engine or stop fallback.",
			"inputSchema": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"server_id"},
				"properties": map[string]any{
					"server_id":        map[string]any{"type": "string", "minLength": 1, "description": "Exact full server_id from this daemon's mux_list."},
					"hold_seconds":     ttl,
					"drain_timeout_ms": map[string]any{"type": "integer", "default": 10000, "minimum": 0, "description": "Drain grace in milliseconds; zero forces retirement."},
				},
			},
		},
		{
			"name":        "mux_resume",
			"description": "Release one exact hold_id after proven managed-tree retirement, permitting fresh demand without replay.",
			"inputSchema": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"hold_id"},
				"properties": map[string]any{"hold_id": map[string]any{"type": "string", "minLength": 1}},
			},
		},
		{
			"name":        "mux_renew",
			"description": "Renew one exact active hold_id from serialized acceptance time; does not change retirement state.",
			"inputSchema": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"hold_id"},
				"properties": map[string]any{"hold_id": map[string]any{"type": "string", "minLength": 1}, "hold_seconds": ttl},
			},
		},
	}
}

func parseMaintenanceArguments(cmd string, args json.RawMessage) (control.Request, time.Duration, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || fields == nil {
		return control.Request{}, 0, control.ErrMaintenanceInvalid
	}
	identityField := "hold_id"
	if cmd == "hold" {
		identityField = "server_id"
	} else if cmd != "resume" && cmd != "renew" {
		return control.Request{}, 0, control.ErrMaintenanceInvalid
	}
	for field := range fields {
		if field != identityField && !(field == "hold_seconds" && cmd != "resume") && !(field == "drain_timeout_ms" && cmd == "hold") {
			return control.Request{}, 0, control.ErrMaintenanceInvalid
		}
	}
	var identity string
	if err := json.Unmarshal(fields[identityField], &identity); err != nil || identity == "" || strings.TrimSpace(identity) != identity {
		return control.Request{}, 0, control.ErrMaintenanceInvalid
	}
	req := control.Request{Cmd: cmd}
	timeout := 5 * time.Second
	if cmd == "hold" {
		req.ServerID = identity
		drain := int64(10000)
		if raw, present := fields["drain_timeout_ms"]; present {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &drain) != nil {
				return req, 0, control.ErrMaintenanceInvalid
			}
		}
		if drain < 0 || drain > int64(^uint(0)>>1) || drain > int64((time.Duration(1<<63-1)-timeout)/time.Millisecond) {
			return req, 0, control.ErrMaintenanceInvalid
		}
		req.DrainTimeoutMs = int(drain)
		timeout += time.Duration(drain) * time.Millisecond
	} else {
		req.HoldID = identity
	}
	if cmd != "resume" {
		seconds := int64(300)
		if raw, present := fields["hold_seconds"]; present {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &seconds) != nil {
				return req, 0, control.ErrMaintenanceInvalid
			}
		}
		if seconds < 1 || seconds > 3600 {
			return req, 0, control.ErrMaintenanceInvalid
		}
		milliseconds := seconds * 1000
		req.HoldTTLMS = &milliseconds
	}
	return req, timeout, nil
}

func (s *Server) toolMuxMaintenance(id json.RawMessage, cmd string, args json.RawMessage) {
	req, timeout, err := parseMaintenanceArguments(cmd, args)
	if err != nil {
		s.sendControlToolError(id, err)
		return
	}
	result, err := control.SendMaintenance(s.daemonCtlPath(), req, timeout)
	if err != nil {
		s.sendControlToolError(id, err)
		return
	}
	s.sendJSONToolResult(id, control.Response{OK: true, Maintenance: result})
}

func (s *Server) sendControlToolError(id json.RawMessage, err error) {
	var maintenanceErr *control.MaintenanceError
	if !errors.As(err, &maintenanceErr) {
		s.sendToolError(id, "local daemon control operation failed")
		return
	}
	payload := control.Response{OK: false, Message: maintenanceErr.Error(), ErrorCode: maintenanceErr.Code, Maintenance: maintenanceErr.Result}
	text, _ := json.Marshal(payload)
	s.sendResult(id, map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": string(text)}},
	})
}
