package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func modernSharingOptions(protocolEra era.ProtocolEra, sharing, partition string, mode serverid.SharingMode) (bool, *control.PartitionAssertion, error) {
	if sharing == "" {
		if partition != "" {
			return false, nil, fmt.Errorf("--mcp-partition requires --mcp-sharing=shared")
		}
		return false, nil, nil
	}
	if sharing != "shared" {
		return false, nil, fmt.Errorf("--mcp-sharing must be shared")
	}
	if protocolEra != era.EraModern20260728 || partition == "" {
		return false, nil, fmt.Errorf("--mcp-sharing=shared requires --mcp-protocol=2026-07-28 and --mcp-partition")
	}
	if mode == serverid.ModeIsolated {
		return false, nil, nil
	}
	return true, &control.PartitionAssertion{Known: true, Key: partition}, nil
}

func launcherTraceArgs(args []string) []string {
	var redacted []string
	for i := 0; i < len(args); i++ {
		name, _, inline := strings.Cut(args[i], "=")
		if name != "--mcp-partition" && name != "-mcp-partition" {
			continue
		}
		if redacted == nil {
			redacted = slices.Clone(args)
		}
		if inline {
			redacted[i] = name + "=[redacted]"
		} else if i+1 < len(args) {
			i++
			redacted[i] = "[redacted]"
		}
	}
	if redacted != nil {
		return redacted
	}
	return args
}
