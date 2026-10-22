package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

type wrapperOptions struct {
	Args          []string
	ProtocolEra   era.ProtocolEra
	ProtocolWire  string
	Mode          serverid.SharingMode
	ModernSharing bool
	Partition     *control.PartitionAssertion
	Standalone    bool
}

func parseWrapperOptions(args []string, output io.Writer) (wrapperOptions, error) {
	var options wrapperOptions
	if output == nil {
		output = io.Discard
	}
	flags := flag.NewFlagSet("mcp-mux", flag.ContinueOnError)
	flags.SetOutput(output)
	isolated := flags.Bool("isolated", false, "Run in isolated mode (dedicated upstream per client)")
	stateless := flags.Bool("stateless", false, "Ignore cwd in server identity (for stateless servers like time, tavily)")
	flags.BoolVar(&options.Standalone, "daemon", false, "Unsupported: direct headless owners require managed admission")
	protocol := flags.String("mcp-protocol", "", "MCP protocol era (2026-07-28)")
	sharing := flags.String("mcp-sharing", "", "Explicit modern owner sharing (shared)")
	partition := flags.String("mcp-partition", "", "Non-secret stable authorization partition for modern sharing")
	if err := flags.Parse(args); err != nil {
		return wrapperOptions{}, err
	}
	if options.Standalone {
		return wrapperOptions{}, control.ErrMaintenanceUnsupported
	}
	options.Args = flags.Args()
	var err error
	options.ProtocolEra, err = era.ParseProtocolEra(*protocol)
	if err != nil {
		return wrapperOptions{}, fmt.Errorf("unsupported MCP protocol %q", *protocol)
	}
	options.ProtocolWire, err = options.ProtocolEra.Wire()
	if err != nil {
		return wrapperOptions{}, err
	}
	options.Mode = serverid.ModeGlobal
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MCP_MUX_DEFAULT_MODE"))) {
	case "cwd":
		options.Mode = serverid.ModeCwd
	case "git":
		options.Mode = serverid.ModeGit
	case "isolated":
		options.Mode = serverid.ModeIsolated
	}
	if *stateless || os.Getenv("MCP_MUX_STATELESS") == "1" {
		options.Mode = serverid.ModeGlobal
	}
	if *isolated || os.Getenv("MCP_MUX_ISOLATED") == "1" ||
		(options.ProtocolEra == era.EraModern20260728 && strings.EqualFold(strings.TrimSpace(os.Getenv("MCP_MUX_DEFAULT_MODE")), "isolated")) {
		options.Mode = serverid.ModeIsolated
	}
	options.ModernSharing, options.Partition, err = modernSharingOptions(options.ProtocolEra, *sharing, *partition, options.Mode)
	if err != nil {
		return wrapperOptions{}, err
	}
	if options.ModernSharing {
		options.Mode = serverid.ModeGlobal
	}
	return options, nil
}

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
