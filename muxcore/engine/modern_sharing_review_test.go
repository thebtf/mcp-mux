package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	muxcore "github.com/thebtf/mcp-mux/muxcore"
	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/owner"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func TestModernSharingReview_EngineNormalizesNonisolatedDefaultModes(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		defaultMode string
		isolated    string
		sharing     bool
		wantOwners  int
		wantPolicy  string
	}{
		{name: "cwd shared", defaultMode: "cwd", isolated: "0", sharing: true, wantOwners: 1, wantPolicy: "shared-exact-context"},
		{name: "git shared", defaultMode: "git", isolated: "0", sharing: true, wantOwners: 1, wantPolicy: "shared-exact-context"},
		{name: "cwd explicit isolation", defaultMode: "cwd", isolated: "1", sharing: true, wantOwners: 2, wantPolicy: "forced-isolated"},
		{name: "git explicit isolation", defaultMode: "git", isolated: "1", sharing: true, wantOwners: 2, wantPolicy: "forced-isolated"},
		{name: "isolated default", defaultMode: "isolated", isolated: "0", sharing: true, wantOwners: 2, wantPolicy: "forced-isolated"},
		{name: "cwd R1", defaultMode: "cwd", isolated: "0", wantOwners: 2, wantPolicy: "forced-isolated"},
		{name: "git R1", defaultMode: "git", isolated: "0", wantOwners: 2, wantPolicy: "forced-isolated"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("MCP_MUX_DEFAULT_MODE", testCase.defaultMode)
			t.Setenv("MCP_MUX_STATELESS", "0")
			t.Setenv("MCP_MUX_ISOLATED", testCase.isolated)
			baseDir := shortEngineBaseDir(t)
			const name = "r2review"
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
				Name:           name,
				Namespace:      name,
				Command:        "modern-sharing-review-fixture",
				BaseDir:        baseDir,
				ProtocolPolicy: era.PolicyModern20260728,
				ModernSharing:  testCase.sharing,
				PartitionResolver: func(context.Context, muxcore.ProjectContext) (control.PartitionAssertion, error) {
					return control.PartitionAssertion{Known: true, Key: "review-known-library-partition"}, nil
				},
				Logger: log.New(io.Discard, "", 0),
			})
			if err != nil {
				t.Fatal(err)
			}

			// Stop at the wire bridge only after the real daemon has admitted the owner.
			admitted := errors.New("real daemon admission reached the bridge")
			originalBridge := runResilientClient
			runResilientClient = func(owner.ResilientClientConfig) error { return admitted }
			t.Cleanup(func() { runResilientClient = originalBridge })
			for range 2 {
				redirectEngineClientStdio(t, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`+"\n")
				if err := eng.runClient(context.Background()); !errors.Is(err, admitted) {
					t.Fatalf("supported default mode failed before real daemon admission: %v", err)
				}
			}
			if got := d.OwnerCount(); got != testCase.wantOwners {
				t.Errorf("two engine admissions created %d owners, want %d", got, testCase.wantOwners)
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
			if len(listing.Owners) != testCase.wantOwners {
				t.Fatalf("public status lists %d owners, want %d", len(listing.Owners), testCase.wantOwners)
			}
			for _, info := range listing.Owners {
				if info.ProtocolEra != "2026-07-28" || info.SharingPolicy != testCase.wantPolicy {
					t.Errorf("engine admission changed modern sharing policy: era=%q policy=%q, want %q", info.ProtocolEra, info.SharingPolicy, testCase.wantPolicy)
				}
			}
		})
	}
}
