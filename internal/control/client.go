package control

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

const clientDeadline = 5 * time.Second

// Send connects to the control socket, sends a Request, reads one Response, and closes.
func Send(socketPath string, req Request) (*Response, error) {
	conn, err := net.DialTimeout("unix", socketPath, clientDeadline)
	if err != nil {
		return nil, fmt.Errorf("control: dial %s: %w", socketPath, err)
	}
	defer conn.Close()

	// Set overall deadline for the entire exchange
	if err := conn.SetDeadline(time.Now().Add(clientDeadline)); err != nil {
		return nil, fmt.Errorf("control: set deadline: %w", err)
	}

	// Send request
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("control: marshal request: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("control: write: %w", err)
	}

	// Read response
	dec := json.NewDecoder(conn)
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("control: read response: %w", err)
	}

	return &resp, nil
}

// SendWithTimeout is like Send but uses a custom deadline for long operations like drain.
func SendWithTimeout(socketPath string, req Request, timeout time.Duration) (*Response, error) {
	conn, err := net.DialTimeout("unix", socketPath, clientDeadline)
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
