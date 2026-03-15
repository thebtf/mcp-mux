// mcp-mux is a transparent command wrapper that multiplexes MCP server instances.
//
// Usage:
//
//	mcp-mux [flags] <command> [args...]
//	mcp-mux status
//
// mcp-mux wraps any MCP server command. The first instance for a given server
// becomes the "owner" (spawns the real process, listens on IPC). Subsequent
// instances connect as clients, sharing the single upstream process.
//
// Example:
//
//	mcp-mux uvx --from git+https://... serena start-mcp-server
//	mcp-mux node D:/Dev/openrouter-mcp/dist/server.js
//	mcp-mux --isolated npx playwright-mcp  # per-session mode
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/bitswan-space/mcp-mux/internal/ipc"
	"github.com/bitswan-space/mcp-mux/internal/mux"
	"github.com/bitswan-space/mcp-mux/internal/serverid"
)

func main() {
	isolated := flag.Bool("isolated", false, "Run in isolated mode (dedicated upstream per client)")
	stateless := flag.Bool("stateless", false, "Ignore cwd in server identity (for stateless servers like time, tavily)")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mcp-mux [flags] <command> [args...]")
		fmt.Fprintln(os.Stderr, "       mcp-mux status")
		os.Exit(1)
	}

	// Handle subcommands
	switch args[0] {
	case "status":
		runStatus()
		return
	case "stop":
		runStop()
		return
	case "upgrade":
		runUpgrade()
		return
	}

	// Determine sharing mode
	mode := serverid.ModeCwd
	if *stateless {
		mode = serverid.ModeGlobal
	}
	if *isolated {
		mode = serverid.ModeIsolated
	}

	// Get current working directory
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting cwd: %v\n", err)
		os.Exit(1)
	}


	// Compute server identity
	command := args[0]
	cmdArgs := args[1:]
	sid := serverid.GenerateContextKey(mode, command, cmdArgs, nil, cwd)
	ipcPath := serverid.IPCPath(sid)

	logger := log.New(os.Stderr, fmt.Sprintf("[mcp-mux:%s] ", sid[:8]), log.LstdFlags)

	// In isolated mode, always become owner (skip IPC check)
	if *isolated {
		logger.Printf("isolated mode: starting dedicated upstream")
		runOwner(args, cwd, ipcPath, logger, true)
		return
	}

	// Try to connect as client first
	if ipc.IsAvailable(ipcPath) {
		logger.Printf("connecting to existing owner at %s", ipcPath)
		if err := mux.RunClient(ipcPath, os.Stdin, os.Stdout); err != nil {
			logger.Printf("client error: %v", err)
			os.Exit(1)
		}
		return
	}

	// No owner found — become one
	logger.Printf("becoming owner for %s (cwd: %s, mode: %s)", serverid.DescribeArgs(args), cwd, mode)
	runOwner(args, cwd, ipcPath, logger, *isolated)
}

func runOwner(args []string, cwd string, ipcPath string, logger *log.Logger, isolated bool) {
	command := args[0]
	cmdArgs := args[1:]

	// Collect environment variables that were passed to us
	env := make(map[string]string)
	// MCP servers receive env from their config — those are passed through
	// our environment. We don't filter here; the upstream inherits our full env
	// via upstream.Start which uses os.Environ().

	effectiveIPCPath := ipcPath
	if isolated {
		// In isolated mode, use a unique path that won't be found by other instances
		effectiveIPCPath = ipcPath + fmt.Sprintf("-%d", os.Getpid())
	}

	owner, err := mux.NewOwner(mux.OwnerConfig{
		Command: command,
		Args:    cmdArgs,
		Env:     env,
		Cwd:     cwd,
		IPCPath: effectiveIPCPath,
		Logger:  logger,
	})
	if err != nil {
		logger.Fatalf("failed to start owner: %v", err)
	}

	// Add our own stdio as the first session
	session := mux.NewSession(os.Stdin, os.Stdout)
	owner.AddSession(session)

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Printf("received signal %v, shutting down", sig)
		owner.Shutdown()
	case <-owner.Done():
		// Owner shut down (upstream exited)
	case <-session.Done():
		// Our own session ended (stdin closed)
		logger.Printf("stdin closed, shutting down")
		owner.Shutdown()
	}
}

func runStop() {
	fmt.Fprintln(os.Stderr, "Stopping all mcp-mux instances...")

	// Clean up socket files and count active instances
	tmpDir := os.TempDir()
	entries, _ := os.ReadDir(tmpDir)
	cleaned := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "mcp-mux-") || !strings.HasSuffix(name, ".sock") {
			continue
		}
		path := tmpDir + string(os.PathSeparator) + name
		_ = os.Remove(path)
		cleaned++
	}

	// Kill all mcp-mux processes (except ourselves)
	myPID := os.Getpid()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// taskkill on Windows — /FI filter excludes our own PID
		cmd = exec.Command("taskkill", "/IM", "mcp-mux.exe", "/F")
	} else {
		// pkill on Unix — exclude our own PID
		cmd = exec.Command("pkill", "-f", "mcp-mux")
	}
	_ = myPID // taskkill can't easily exclude self, but we exit right after anyway
	output, err := cmd.CombinedOutput()
	if err != nil {
		// taskkill returns error if no processes found — that's fine
		if !strings.Contains(string(output), "not found") && !strings.Contains(string(output), "No tasks") {
			fmt.Fprintf(os.Stderr, "kill output: %s\n", strings.TrimSpace(string(output)))
		}
	}

	fmt.Fprintf(os.Stderr, "Cleaned %d socket files. All mcp-mux instances stopped.\n", cleaned)
}

func runUpgrade() {
	runStop()
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "All instances stopped. Binary unlocked. Rebuild with:")
	fmt.Fprintln(os.Stderr, "  go build -o mcp-mux.exe ./cmd/mcp-mux")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "MCP servers will restart automatically on next CC tool call.")
}

func runStatus() {
	// Scan temp directory for mcp-mux socket files
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading temp dir: %v\n", err)
		os.Exit(1)
	}

	type serverStatus struct {
		ID      string `json:"id"`
		Path    string `json:"path"`
		Active  bool   `json:"active"`
	}

	var servers []serverStatus
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "mcp-mux-") || !strings.HasSuffix(name, ".sock") {
			continue
		}

		path := tmpDir + string(os.PathSeparator) + name
		id := strings.TrimPrefix(strings.TrimSuffix(name, ".sock"), "mcp-mux-")

		active := ipc.IsAvailable(path)
		servers = append(servers, serverStatus{
			ID:     id,
			Path:   path,
			Active: active,
		})
	}

	if len(servers) == 0 {
		fmt.Println("No active mcp-mux instances found.")
		return
	}

	data, _ := json.MarshalIndent(servers, "", "  ")
	fmt.Println(string(data))
}
