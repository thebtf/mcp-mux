package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const (
	envShimIdleTimeout  = "MCPMUX_SHIM_IDLE_TIMEOUT"
	envShimDormantGrace = "MCPMUX_SHIM_DORMANT_GRACE"

	defaultShimIdleTimeout  = 10 * time.Minute
	defaultShimDormantGrace = 30 * time.Second
)

func shimLifecycleDurations(getenv func(string) string) (time.Duration, time.Duration) {
	parse := func(key string, fallback time.Duration) time.Duration {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return fallback
		}
		value, err := time.ParseDuration(raw)
		if err != nil {
			return fallback
		}
		return value
	}
	return parse(envShimIdleTimeout, defaultShimIdleTimeout),
		parse(envShimDormantGrace, defaultShimDormantGrace)
}

func resilientClientExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, owner.ErrIdleDormant):
		return launcherDormantExitCode
	default:
		return 1
	}
}

func canSuspendViaDaemon(token, serverID string) (bool, string, error) {
	resp, err := control.SendWithTimeout(
		serverid.DaemonControlPath("", engineName),
		control.Request{Cmd: "can_suspend", PrevToken: token, ServerID: serverID},
		2*time.Second,
	)
	if err != nil {
		return false, "", err
	}
	if !resp.OK {
		// A shutting-down daemon is expected to come back. Every other negative
		// protocol verdict is unsafe to retry for this connected shim, including
		// v0.26.13's "unknown command: can_suspend" response.
		if resp.Message == "daemon shutting down" {
			return false, resp.Message, fmt.Errorf("can_suspend: %s", resp.Message)
		}
		return false, resp.Message, owner.ErrIdleSuspendGateUnavailable
	}
	var verdict struct {
		Allowed *bool  `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(resp.Data, &verdict); err != nil {
		return false, "", owner.ErrIdleSuspendGateUnavailable
	}
	if verdict.Allowed == nil || (!*verdict.Allowed && verdict.Reason == "") || verdict.Reason == "persistent" {
		return false, verdict.Reason, owner.ErrIdleSuspendGateUnavailable
	}
	return *verdict.Allowed, verdict.Reason, nil
}
