package ipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const dialTimeout = 500 * time.Millisecond
const pipeBufferSize = 64 * 1024

var (
	staleSocketRemoveAttempts = 40
	staleSocketRemoveDelay    = 25 * time.Millisecond
	pipeListenRetryAttempts   = 40
	pipeListenRetryDelay      = 25 * time.Millisecond
	pipeListenerCloseTimeout  = 2 * time.Second
	removeSocketFile          = os.Remove
	renameSocketFile          = os.Rename
	sleepBeforeRemoveRetry    = time.Sleep
	sleepBeforeListenRetry    = time.Sleep
	listenNamedPipe           = winio.ListenPipe
)

// Listen creates an IPC listener for path.
//
// Windows maps the stable filesystem-style muxcore path to a named pipe. This
// avoids AF_UNIX socket reparse-point files that can remain locked by the old
// process during hot restart.
func Listen(path string) (net.Listener, error) {
	if IsAvailable(path) {
		return nil, fmt.Errorf("ipc: listener already active at %s (another process is serving)", path)
	}
	cfg, err := pipeConfig()
	if err != nil {
		return nil, fmt.Errorf("ipc: build pipe security %s: %w", path, err)
	}
	ln, err := listenPipeWithRetry(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen %s: %w", path, err)
	}
	return &pipeListener{Listener: ln, path: path}, nil
}

func listenPipeWithRetry(path string, cfg *winio.PipeConfig) (net.Listener, error) {
	attempts := pipeListenRetryAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		ln, err := listenNamedPipe(pipeName(path), cfg)
		if err == nil {
			return ln, nil
		}
		lastErr = err
		if !isRetryablePipeListenError(err) {
			return nil, err
		}
		if attempt+1 < attempts && pipeListenRetryDelay > 0 {
			sleepBeforeListenRetry(pipeListenRetryDelay)
		}
	}
	return nil, lastErr
}

func isRetryablePipeListenError(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_PIPE_BUSY)
}

func pipeConfig() (*winio.PipeConfig, error) {
	sddl, err := currentUserSDDL()
	if err != nil {
		return nil, err
	}
	return &winio.PipeConfig{
		SecurityDescriptor: sddl,
		InputBufferSize:    pipeBufferSize,
		OutputBufferSize:   pipeBufferSize,
		MessageMode:        false,
	}, nil
}

func currentUserSDDL() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)", user.User.Sid.String()), nil
}

type pipeListener struct {
	net.Listener
	path      string
	closeOnce sync.Once
	closeErr  error
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() {
		l.closeErr = l.close()
	})
	return l.closeErr
}

func (l *pipeListener) close() error {
	// go-winio aborts a pending Accept by closing the in-flight pipe handle.
	// On Windows that can wait on overlapped ConnectNamedPipe cancellation.
	// Wake the Accept path first so the listener routine returns to its close
	// select before we call Close.
	if conn, err := DialTimeout(l.path, 100*time.Millisecond); err == nil {
		_ = conn.Close()
	}
	time.Sleep(10 * time.Millisecond)

	closed := make(chan error, 1)
	go func() {
		closed <- l.Listener.Close()
	}()

	select {
	case err := <-closed:
		return err
	case <-time.After(pipeListenerCloseTimeout):
		return fmt.Errorf("ipc: close named pipe listener %s timed out after %s", l.path, pipeListenerCloseTimeout)
	}
}

func Dial(path string) (net.Conn, error) {
	return DialTimeout(path, dialTimeout)
}

func DialTimeout(path string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = dialTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, pipeName(path))
	if err != nil {
		return nil, fmt.Errorf("ipc: dial %s: %w", path, err)
	}
	return conn, nil
}

func IsAvailable(path string) bool {
	conn, err := Dial(path)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func Cleanup(path string) {
	_ = removeSocketFile(path)
}

func pipeName(path string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(path)))
	return `\\.\pipe\mcp-mux-` + hex.EncodeToString(sum[:16])
}
