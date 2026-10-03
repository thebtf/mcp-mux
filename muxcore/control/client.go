package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/ipc"
)

const (
	clientDeadline     = 5 * time.Second
	operationalTimeout = 180 * time.Second
)

// Send connects to the control socket, sends a Request, reads one Response, and closes.
func Send(socketPath string, req Request) (*Response, error) {
	return SendWithTimeout(socketPath, req, 0)
}

// SendWithTimeout honors positive deadlines. Otherwise hold and restart_owner
// use the operational completion allowance plus one requested drain; other
// commands retain the short exchange deadline. This does not limit server work.
func SendWithTimeout(socketPath string, req Request, timeout time.Duration) (*Response, error) {
	timeout, err := controlTimeout(req, timeout)
	if err != nil {
		return nil, err
	}
	dialTimeout := clientDeadline
	if timeout > 0 && timeout < dialTimeout {
		dialTimeout = timeout
	}
	conn, err := ipc.DialTimeout(socketPath, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("control: dial %s: %w", socketPath, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("control: set deadline: %w", err)
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("control: marshal request: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("control: write: %w", err)
	}

	dec := json.NewDecoder(conn)
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("control: read response: %w", err)
	}

	return &resp, nil
}

func controlTimeout(req Request, timeout time.Duration) (time.Duration, error) {
	if timeout > 0 {
		return timeout, nil
	}
	if req.Cmd != "hold" && req.Cmd != "restart_owner" {
		return clientDeadline, nil
	}
	if req.DrainTimeoutMs < 0 || int64(req.DrainTimeoutMs) > int64((time.Duration(1<<63-1)-operationalTimeout)/time.Millisecond) {
		return 0, ErrMaintenanceInvalid
	}
	return operationalTimeout + time.Duration(req.DrainTimeoutMs)*time.Millisecond, nil
}

// SendMaintenance performs one maintenance exchange without lifecycle fallback.
// Untyped old endpoints are unsupported; malformed typed replies fail closed.
func SendMaintenance(socketPath string, req Request, timeout time.Duration) (*MaintenanceResult, error) {
	req, err := prepareMaintenanceRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := SendWithTimeout(socketPath, req, timeout)
	if err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var timeErr *time.ParseError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.As(err, &timeErr) ||
			errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrMaintenanceInvalid
		}
		return nil, err
	}
	return maintenanceResponse(req, resp)
}
