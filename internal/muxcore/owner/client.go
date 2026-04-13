package owner

import (
	"fmt"
	"io"
	"sync"

	"github.com/thebtf/mcp-mux/internal/muxcore/ipc"
)

// RunClient connects to an existing owner via IPC and bridges
// the caller's stdio to the IPC connection.
//
// This is a blocking call that returns when either side disconnects.
func RunClient(ipcPath string, stdin io.Reader, stdout io.Writer) error {
	conn, err := ipc.Dial(ipcPath)
	if err != nil {
		return fmt.Errorf("client: connect to %s: %w", ipcPath, err)
	}
	defer conn.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	// stdin → IPC (send requests to owner)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := io.Copy(conn, stdin)
		if err != nil {
			errCh <- fmt.Errorf("client: stdin→ipc: %w", err)
		}
		// Signal write-side close so owner knows we're done
		if closer, ok := conn.(interface{ CloseWrite() error }); ok {
			closer.CloseWrite()
		}
	}()

	// IPC → stdout (receive responses from owner)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := io.Copy(stdout, conn)
		if err != nil {
			errCh <- fmt.Errorf("client: ipc→stdout: %w", err)
		}
	}()

	wg.Wait()
	close(errCh)

	// Return first error if any
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}
