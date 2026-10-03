package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/daemon"
	"github.com/thebtf/mcp-mux/muxcore/ipc"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

func isMaintenanceError(err error) bool {
	var maintenanceErr *control.MaintenanceError
	return errors.As(err, &maintenanceErr)
}

func standaloneAdmissionError(noDaemon, headless bool) error {
	if noDaemon || headless {
		return control.ErrMaintenanceUnsupported
	}
	return nil
}

func parseMaintenanceCommand(cmd string, args []string) (control.Request, time.Duration, bool, error) {
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "Emit one JSON response")
	ttl := 5 * time.Minute
	drain := 10 * time.Second
	switch cmd {
	case "hold":
		flags.DurationVar(&ttl, "ttl", ttl, "Hold duration, at most one hour")
		flags.DurationVar(&drain, "drain-timeout", drain, "Drain grace; zero forces retirement")
	case "renew":
		flags.DurationVar(&ttl, "ttl", ttl, "Hold duration, at most one hour")
	case "resume":
	default:
		return control.Request{}, 0, false, control.ErrMaintenanceInvalid
	}
	// Preserve JSON framing even when an earlier flag or duration is invalid.
	for _, arg := range args {
		if arg == "--json" || arg == "-json" || arg == "--json=true" || arg == "-json=true" {
			*jsonOutput = true
		}
	}
	var identity string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		identity, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return control.Request{}, 0, *jsonOutput, control.ErrMaintenanceInvalid
	}
	if identity == "" && flags.NArg() == 1 {
		identity = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return control.Request{}, 0, *jsonOutput, control.ErrMaintenanceInvalid
	}
	if identity == "" || strings.TrimSpace(identity) != identity {
		return control.Request{}, 0, *jsonOutput, control.ErrMaintenanceInvalid
	}
	req := control.Request{Cmd: cmd}
	timeout := 5 * time.Second
	if cmd == "hold" {
		req.ServerID = identity
		maxMillis := int64(^uint(0) >> 1)
		if drain < 0 || drain%time.Millisecond != 0 || drain.Milliseconds() > maxMillis || drain > time.Duration(1<<63-1)-timeout {
			return req, 0, *jsonOutput, control.ErrMaintenanceInvalid
		}
		req.DrainTimeoutMs = int(drain.Milliseconds())
		timeout += drain
	} else {
		req.HoldID = identity
	}
	if cmd != "resume" {
		if ttl <= 0 || ttl > time.Hour || ttl%time.Millisecond != 0 {
			return req, 0, *jsonOutput, control.ErrMaintenanceInvalid
		}
		milliseconds := ttl.Milliseconds()
		req.HoldTTLMS = &milliseconds
	}
	return req, timeout, *jsonOutput, nil
}

func runMaintenanceCommand(cmd string, args []string, stdout, stderr io.Writer) int {
	req, timeout, jsonOutput, err := parseMaintenanceCommand(cmd, args)
	var result *control.MaintenanceResult
	if err == nil {
		result, err = control.SendMaintenance(serverid.DaemonControlPath("", engineName), req, timeout)
	}
	response := control.Response{OK: err == nil, Maintenance: result}
	if err != nil {
		var maintenanceErr *control.MaintenanceError
		if errors.As(err, &maintenanceErr) {
			response.ErrorCode = maintenanceErr.Code
			response.Maintenance = maintenanceErr.Result
			response.Message = maintenanceErr.Error()
		} else {
			response.Message = "maintenance endpoint unavailable"
		}
	}
	if jsonOutput {
		if encodeErr := json.NewEncoder(stdout).Encode(response); encodeErr != nil {
			return 1
		}
	} else if err != nil {
		fmt.Fprintln(stderr, response.Message)
	} else {
		fmt.Fprintf(stdout, "%s %s: %s (expires %s)\n", cmd, result.HoldID, result.State, result.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if err != nil {
		return 1
	}
	return 0
}

// acquireMaintenanceMutation serializes product activation with every durable
// maintenance mutation. Status must be a read-only projection of aware authority.
func acquireMaintenanceMutation() (io.Closer, error) {
	lock, err := ipc.AcquireFileLock(serverid.DaemonLockPath("", engineName))
	if err != nil {
		return nil, fmt.Errorf("acquire daemon namespace mutation lock: %w", err)
	}
	if err := daemon.CheckMaintenanceForActivation(engineName, serverid.DaemonControlPath("", engineName)); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := checkMaintenanceMutationStatus(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func checkMaintenanceMutationStatus() error {
	response, err := launcherControlSendWithTimeout(serverid.DaemonControlPath("", engineName), control.Request{Cmd: "status"}, 5*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil // Persisted authority was proven clear under the same lock.
		}
		return control.ErrMaintenanceUnsupported
	}
	if err := response.Err(); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Data, &fields); err != nil || fields == nil {
		return control.ErrMaintenanceInvalid
	}
	if raw, present := fields["maintenance_error_code"]; present {
		var code control.MaintenanceErrorCode
		if strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &code) != nil {
			return control.ErrMaintenanceInvalid
		}
		if code != "" {
			return (&control.Response{ErrorCode: code}).Err()
		}
	}
	raw, present := fields["maintenance"]
	if !present {
		return nil // An old status endpoint is safe only after persisted proof.
	}
	var leases []control.MaintenanceResult
	if err := json.Unmarshal(raw, &leases); err != nil || leases == nil {
		return control.ErrMaintenanceInvalid
	}
	for index := range leases {
		lease := &leases[index]
		if err := (&control.Response{OK: true, Maintenance: lease}).Err(); err != nil || lease.State == control.MaintenanceReleased {
			return control.ErrMaintenanceInvalid
		}
	}
	if len(leases) != 0 {
		return &control.MaintenanceError{Code: control.ErrMaintenanceHeld.Code, Result: &leases[0]}
	}
	return nil
}

func lifecycleErrorText(err error) string {
	var maintenanceErr *control.MaintenanceError
	if errors.As(err, &maintenanceErr) {
		return fmt.Sprintf("%s: %s", maintenanceErr.Code, maintenanceErr.Error())
	}
	return err.Error()
}
