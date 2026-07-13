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
		switch resp.Message {
		case "can_suspend not supported", "unknown token", "owner gone":
			return false, "", owner.ErrIdleSuspendGateUnavailable
		}
		return false, resp.Message, fmt.Errorf("can_suspend: %s", resp.Message)
	}
	var verdict control.SuspendCheckResponse
	if err := json.Unmarshal(resp.Data, &verdict); err != nil {
		return false, "", fmt.Errorf("can_suspend response: %w", err)
	}
	return verdict.Allowed, verdict.Reason, nil
}
