# ADR 012 -- Versioned Launcher Upgrade

Status: Accepted

Date: 2026-05-20

## Context

The previous upgrade strategy assumed the configured `mcp-mux.exe` could be
renamed while active Windows processes were running from that image. The
release playbook for PR #116 falsified that assumption on the workstation:
`mcp-mux.exe upgrade --restart` failed with `rename current to old: Access is
denied` while many live shim/daemon processes held the configured executable.

This is a product-contract problem, not only a deploy note. The configured
binary is the stable entrypoint that MCP consumers launch, so it is exactly the
file most likely to be locked during an update.

## Decision

`mcp-mux.exe` becomes a stable launcher/fallback entrypoint. Runtime logic moves
behind a versioned engine selected by an active pointer:

```text
consumer -> mcp-mux.exe launcher -> mcp-mux.versions/<hash>/mcp-mux-engine.exe
                               \-> mcp-mux.versions/active.txt
```

`mcp-mux upgrade` no longer renames the configured launcher during ordinary
runtime updates. It installs the pending `mcp-mux.exe~` as a content-addressed
engine under `mcp-mux.versions/<hash>/mcp-mux-engine.exe`, updates
`active.txt`, removes the staged file, and keeps the launcher path stable.

Launcher replacement is a separate maintenance operation
(`upgrade --update-launcher`). It is not part of the ordinary engine update
path, because the host-facing launcher/shim is the durable stdio anchor and
must not become a release-to-release moving part.

`upgrade --restart` is safe by default: before sending `graceful-restart`, it
asks the daemon for status and sums live `session_count` values. If any live
session is attached, or if zero sessions cannot be proven, the daemon restart is
deferred and the existing host transports are preserved. A forced daemon
restart remains available only as explicit maintenance through
`--force-daemon-restart`.

When a daemon restart is actually allowed, successor daemon spawn resolves the
active engine pointer through `MCPMUX_ACTIVE_ENGINE_FILE` (or an explicit
`MCPMUX_SUCCESSOR_EXE`) instead of blindly spawning `os.Executable()`. The
`cmd/mcp-mux` daemon config explicitly sets `DaemonFlag: "daemon"` so the
successor enters the correct CLI subcommand.

## Consequences

Positive:

- Windows updates no longer depend on renaming a locked configured executable.
- Old shims/daemons can keep running from their old engine path while new shims
  use the active engine.
- The stable MCP config path does not change between releases.

Negative:

- There is one extra launcher process per shim while the active engine runs.
- Launcher changes themselves still require a maintenance replacement, but that
  should be rare compared with engine/runtime updates.
- Ordinary updates do not force old host-facing shims to prove reconnect logic.
  Existing sessions may continue on the old daemon until they drain; this is the
  transparency tradeoff that prevents raw host-visible `Transport closed` from
  an update.
- The first deployment from a pre-launcher binary may still need a maintenance
  replacement if live old-style `mcp-mux.exe` processes lock the configured
  executable. After the stable launcher is installed, normal engine updates no
  longer touch that path.
- A forced daemon restart with live sessions is a maintenance action, not a
  transparent update.
- `mcp-mux.versions/` is runtime state and must stay out of git.

## Verification

Fresh evidence for the accepting implementation:

- `go test .\cmd\mcp-mux -count=1`
- `go test .\muxcore\daemon -count=1`
- `go test ./... -count=1`
- `Push-Location muxcore; go test ./... -count=1; Pop-Location`
- `go vet ./...`
- `scripts\smoke-time-upstream.ps1` using a stable launcher with an installed
  active versioned engine returned `PASS`.
- Isolated `upgrade --restart` with repo-local `TEMP` installed the engine,
  started a daemon through the active engine, returned `status`, and stopped
  cleanly.
- `TestInstallVersionedEngineKeepsLauncherStableByDefault` proves ordinary
  engine install leaves the stable launcher bytes unchanged.
- `TestRestartDaemonAfterEngineSwitchDefersWhenLiveSessionsExist` proves
  ordinary `--restart` does not send `graceful-restart` while live sessions are
  attached.
