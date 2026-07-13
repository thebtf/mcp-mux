package upstream

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

const handoffStreamQuiesceTimeout = 5 * time.Second

// streamReadControl makes a blocking stdout/stderr reader joinable at the
// transactional handoff boundary. Detach first marks the reader as pausing,
// then platform code interrupts the exact pending read. The reader never
// starts another read after the pause bit is visible.
type streamReadControl struct {
	mu      sync.Mutex
	ready   chan struct{}
	done    chan struct{}
	pausing bool
	reading bool
	cancel  func() error
	clear   func() error
	initErr error
}

func newStreamReadControl() *streamReadControl {
	return &streamReadControl{
		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}
}

func (c *streamReadControl) bind(cancel, clear func() error, err error) {
	c.mu.Lock()
	c.cancel = cancel
	c.clear = clear
	c.initErr = err
	close(c.ready)
	c.mu.Unlock()
}

func (c *streamReadControl) scan(scanner *bufio.Scanner) bool {
	c.mu.Lock()
	if c.pausing {
		c.mu.Unlock()
		return false
	}
	c.reading = true
	c.mu.Unlock()

	ok := scanner.Scan()

	c.mu.Lock()
	c.reading = false
	c.mu.Unlock()
	return ok
}

func (c *streamReadControl) isPausing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pausing
}

func (c *streamReadControl) quiesce(timeout time.Duration) error {
	if c == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-c.ready:
	case <-timer.C:
		return errors.New("stream reader did not publish cancellation authority")
	}

	c.mu.Lock()
	c.pausing = true
	reading := c.reading
	cancel := c.cancel
	clear := c.clear
	initErr := c.initErr
	c.mu.Unlock()

	if initErr != nil {
		return initErr
	}
	var cancelErr error
	if reading && cancel != nil {
		// reading becomes true immediately before Scanner.Scan. The kernel
		// request may not exist yet when the first cancellation lands, so keep
		// cancellation pressure active until the reader publishes done. The
		// outer timer is the hard stop; this loop is never unbounded.
		retry := time.NewTicker(time.Millisecond)
		defer retry.Stop()
		for {
			if err := cancel(); err != nil {
				cancelErr = err
				break
			}
			select {
			case <-c.done:
				goto quiesced
			case <-retry.C:
			case <-timer.C:
				return errors.New("stream reader did not quiesce")
			}
		}
	}

	select {
	case <-c.done:
	case <-timer.C:
		return errors.Join(cancelErr, errors.New("stream reader did not quiesce"))
	}

quiesced:
	if clear != nil {
		return errors.Join(cancelErr, clear())
	}
	return cancelErr
}

func rawFileHandle(file *os.File) (uintptr, error) {
	if file == nil {
		return 0, errors.New("nil file")
	}
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var handle uintptr
	if err := raw.Control(func(fd uintptr) { handle = fd }); err != nil {
		return 0, err
	}
	if handle == 0 {
		return 0, errors.New("zero file handle")
	}
	return handle, nil
}

func (p *Process) startStdoutDrain(file *os.File) <-chan struct{} {
	control := newStreamReadControl()
	p.stdoutDrain = control
	go func() {
		defer close(control.done)
		cancel, clear, cleanup, err := prepareStreamReadCancel(file)
		control.bind(cancel, clear, err)
		defer cleanup()

		// Cancellation authority is needed only for live handoff. If the
		// platform cannot prepare it, keep ordinary MCP stdout functional;
		// quiesce will surface initErr and the failed handoff will abort the
		// still-owned tree instead of exposing a racing handle.
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for control.scan(scanner) {
			line := scanner.Bytes()
			cp := make([]byte, len(line))
			copy(cp, line)
			p.lineBuf.push(cp)
		}
		if control.isPausing() {
			// The successor owns the unread tail after handoff. Wake any old-owner
			// ReadLine waiter without closing the transferable OS handle.
			p.lineBuf.markDoneWithErr(nil)
			return
		}
		_ = file.Close()
		scanErr := scanner.Err()
		if errors.Is(scanErr, os.ErrClosed) {
			scanErr = nil
		}
		p.lineBuf.markDoneWithErr(scanErr)
	}()
	return control.done
}

func (p *Process) startStderrDrain(file *os.File, logger *log.Logger, pid int) <-chan struct{} {
	if file == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	control := newStreamReadControl()
	p.stderrDrain = control
	go func() {
		defer close(control.done)
		cancel, clear, cleanup, err := prepareStreamReadCancel(file)
		control.bind(cancel, clear, err)
		defer cleanup()

		// As with stdout, cancellation setup is a handoff capability, not a
		// prerequisite for serving the upstream's ordinary stderr stream.
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for control.scan(scanner) {
			if logger != nil {
				logger.Printf("[upstream:%d] %s", pid, scanner.Text())
			} else {
				fmt.Fprintf(os.Stderr, "[upstream:%d] %s\n", pid, scanner.Text())
			}
		}
		if !control.isPausing() {
			_ = file.Close()
		}
	}()
	return control.done
}

func (p *Process) quiesceStreamsForHandoff() error {
	controls := []*streamReadControl{p.stdoutDrain, p.stderrDrain}
	errCh := make(chan error, len(controls))
	var wg sync.WaitGroup
	for _, control := range controls {
		if control == nil {
			continue
		}
		wg.Add(1)
		go func(c *streamReadControl) {
			defer wg.Done()
			errCh <- c.quiesce(handoffStreamQuiesceTimeout)
		}(control)
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for err := range errCh {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
