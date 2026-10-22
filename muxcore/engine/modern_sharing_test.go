package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func TestModernSharingEngine_NewRequiresExplicitModernPolicy(t *testing.T) {
	for _, policy := range []era.ProtocolPolicy{era.PolicyLegacyOnly, era.PolicyModern20260728} {
		eng, err := New(Config{
			Name:           "modern-sharing-policy",
			Command:        "modern-sharing-fixture",
			ProtocolPolicy: policy,
			ModernSharing:  true,
		})
		if policy == era.PolicyLegacyOnly {
			if err == nil || eng != nil {
				t.Fatal("New accepted modern sharing without an explicit modern policy")
			}
		} else if err != nil || eng == nil {
			t.Fatalf("New rejected explicit modern sharing: %v", err)
		}
	}
}

func TestModernSharingEngine_UnknownResolverAndExplicitIsolationDoNotReuseOwner(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		isolated string
		resolver func(context.Context, muxcore.ProjectContext) (control.PartitionAssertion, error)
	}{
		{name: "absent resolver", isolated: "0"},
		{name: "unknown resolver", isolated: "0", resolver: func(context.Context, muxcore.ProjectContext) (control.PartitionAssertion, error) {
			return control.PartitionAssertion{}, nil
		}},
		{name: "explicit isolation", isolated: "1", resolver: func(context.Context, muxcore.ProjectContext) (control.PartitionAssertion, error) {
			return control.PartitionAssertion{Known: true, Key: "known-library-partition-fixture"}, nil
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("MCP_MUX_DEFAULT_MODE", "global")
			t.Setenv("MCP_MUX_STATELESS", "0")
			t.Setenv("MCP_MUX_ISOLATED", testCase.isolated)
			baseDir := shortEngineBaseDir(t)
			const name = "r2engine"
			ctlPath := serverid.DaemonControlPath(baseDir, name)
			d, err := daemon.New(daemon.Config{
				Name:        name,
				Namespace:   name,
				ControlPath: ctlPath,
				HandlerFunc: func(_ context.Context, stdin io.Reader, _ io.Writer) error {
					_, err := io.Copy(io.Discard, stdin)
					return err
				},
				SkipSnapshot: true,
				Logger:       log.New(io.Discard, "", 0),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(d.Shutdown)
			eng, err := New(Config{
				Name:              name,
				Namespace:         name,
				Command:           "modern-sharing-library-fixture",
				BaseDir:           baseDir,
				ProtocolPolicy:    era.PolicyModern20260728,
				ModernSharing:     true,
				PartitionResolver: testCase.resolver,
				Logger:            log.New(io.Discard, "", 0),
			})
			if err != nil {
				t.Fatal(err)
			}

			// Stop after real daemon admission; this test does not exercise the wire bridge.
			admitted := errors.New("real daemon admission reached the bridge")
			originalBridge := runResilientClient
			runResilientClient = func(owner.ResilientClientConfig) error { return admitted }
			t.Cleanup(func() { runResilientClient = originalBridge })
			for range 2 {
				redirectEngineClientStdio(t, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
				if err := eng.runClient(context.Background()); !errors.Is(err, admitted) {
					t.Fatalf("runClient failed before real daemon admission: %v", err)
				}
			}
			if got := d.OwnerCount(); got != 2 {
				t.Fatalf("two identical isolated library admissions created %d owners, want 2", got)
			}
			response, err := control.SendWithTimeout(ctlPath, control.Request{Cmd: "list_owners"}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Err(); err != nil {
				t.Fatal(err)
			}
			var listing control.ListOwnersResponse
			if err := json.Unmarshal(response.Data, &listing); err != nil {
				t.Fatal(err)
			}
			if len(listing.Owners) != 2 {
				t.Fatalf("public status lists %d isolated owners, want 2", len(listing.Owners))
			}
			for _, info := range listing.Owners {
				if info.ProtocolEra != "2026-07-28" || info.SharingPolicy != "forced-isolated" {
					t.Fatal("unknown or explicitly isolated library admission created a shared or legacy owner")
				}
			}
		})
	}
}

func TestModernSharingEngine_InvalidResolverCannotCreateOwner(t *testing.T) {
	const secret = "private-library-resolver-fixture"
	for _, testCase := range []struct {
		name      string
		assertion control.PartitionAssertion
		err       error
	}{
		{name: "resolver error", err: errors.New(secret)},
		{name: "unknown partition with key", assertion: control.PartitionAssertion{Key: secret}},
		{name: "known partition without key", assertion: control.PartitionAssertion{Known: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("MCP_MUX_DEFAULT_MODE", "global")
			t.Setenv("MCP_MUX_STATELESS", "0")
			t.Setenv("MCP_MUX_ISOLATED", "0")
			baseDir := shortEngineBaseDir(t)
			const name = "r2resolver"
			d, err := daemon.New(daemon.Config{
				Name:         name,
				Namespace:    name,
				ControlPath:  serverid.DaemonControlPath(baseDir, name),
				HandlerFunc:  noopHandler,
				SkipSnapshot: true,
				Logger:       log.New(io.Discard, "", 0),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(d.Shutdown)
			eng, err := New(Config{
				Name:           name,
				Namespace:      name,
				Command:        "modern-sharing-library-fixture",
				BaseDir:        baseDir,
				ProtocolPolicy: era.PolicyModern20260728,
				ModernSharing:  true,
				PartitionResolver: func(context.Context, muxcore.ProjectContext) (control.PartitionAssertion, error) {
					return testCase.assertion, testCase.err
				},
				Logger: log.New(io.Discard, "", 0),
			})
			if err != nil {
				t.Fatal(err)
			}
			bridgeCalls := 0
			originalBridge := runResilientClient
			runResilientClient = func(owner.ResilientClientConfig) error {
				bridgeCalls++
				return errors.New("unexpected bridge call after resolver refusal")
			}
			t.Cleanup(func() { runResilientClient = originalBridge })
			redirectEngineClientStdio(t, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
			if err := eng.runClient(context.Background()); err == nil {
				t.Fatal("runClient accepted an invalid partition resolver result")
			} else if strings.Contains(err.Error(), secret) {
				t.Fatal("resolver refusal disclosed private partition material")
			}
			if got := d.OwnerCount(); got != 0 {
				t.Fatalf("resolver refusal created %d owners", got)
			}
			if bridgeCalls != 0 {
				t.Fatal("resolver refusal reached the wire bridge")
			}
		})
	}
}
