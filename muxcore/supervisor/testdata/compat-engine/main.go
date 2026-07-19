package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/supervisor"
	"github.com/thebtf/mcp-mux/muxcore/supervisor/attest"
)

func main() {
	mode := os.Getenv("MCP_MUX_COMPAT_ENGINE_MODE")
	verified := mode == "new" && verifyParent()
	protocol := supervisor.ProtocolV2()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method,omitempty"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			os.Exit(2)
		}
		switch message.Method {
		case "initialize":
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"compat","version":%q}}}`+"\n", message.ID, mode)
		case "notifications/initialized":
			if mode == "old-colliding" || verified {
				fmt.Printf("%s\n", protocol.Frame(supervisor.ControlDormantReady))
			}
		case controlMethod(protocol, supervisor.ControlCommitDormant):
			if verified {
				fmt.Printf("%s\n", protocol.Frame(supervisor.ControlDormantNack))
			}
		case "tools/call":
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":%q}]}}`+"\n", message.ID, mode)
		default:
			if len(message.ID) > 0 {
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", message.ID)
			}
		}
	}
}

func verifyParent() bool {
	parentPID, err := strconv.Atoi(os.Getenv("MCP_MUX_COMPAT_ATTEST_PARENT"))
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return attest.VerifyParent(ctx, attest.VerifyConfig{
		Advertisement: attest.Advertisement{
			Version:   os.Getenv("MCP_MUX_COMPAT_ATTEST_VERSION"),
			ParentPID: parentPID,
			Endpoint:  os.Getenv("MCP_MUX_COMPAT_ATTEST_ENDPOINT"),
		},
		DirectParentPID: os.Getppid(),
		IOTimeout:       time.Second,
	}) == nil
}

func controlMethod(protocol supervisor.Protocol, control supervisor.Control) string {
	frame := protocol.Frame(control)
	const prefix = `{"jsonrpc":"2.0","method":"`
	if len(frame) < len(prefix)+2 {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(frame), prefix), `"}`)
}
