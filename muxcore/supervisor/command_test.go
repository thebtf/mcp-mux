package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/procgroup"
)

func TestCommandChildHelper(t *testing.T) {
	if os.Getenv("MCP_MUX_SUPERVISOR_COMMAND_HELPER") != "1" {
		return
	}
	if os.Getenv("MCP_MUX_SUPERVISOR_COMMAND_EXIT") == "75" {
		os.Exit(75)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Println("echo:" + line)
		if line == "exit" {
			return
		}
	}
}

func commandHelperSpec() Command {
	return Command{
		Path:   os.Args[0],
		Args:   []string{"-test.run=TestCommandChildHelper"},
		Env:    append(os.Environ(), "MCP_MUX_SUPERVISOR_COMMAND_HELPER=1"),
		Stderr: io.Discard,
	}
}

func TestStartCommandExposesPipesAndExit(t *testing.T) {
	child, err := StartCommand(context.Background(), commandHelperSpec())
	if err != nil {
		t.Fatal(err)
	}
	if child.PID() <= 0 {
		t.Fatalf("PID = %d", child.PID())
	}
	if _, err := io.WriteString(child.Stdin(), "ping\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(child.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "echo:ping" {
		t.Fatalf("child output = %q", line)
	}
	if _, err := io.WriteString(child.Stdin(), "exit\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-child.Wait():
		if exit.Code != 0 || exit.Err != nil {
			t.Fatalf("exit = %#v", exit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit")
	}
	if err := child.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after exit: %v", err)
	}
}

func TestCommandChildStopProducesTerminalProof(t *testing.T) {
	child, err := StartCommand(context.Background(), commandHelperSpec())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := child.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.Wait():
	case <-time.After(time.Second):
		t.Fatal("Stop returned before Wait became available")
	}
}

func TestCommandChildReportsNonzeroStatusWithoutWaitFailure(t *testing.T) {
	spec := commandHelperSpec()
	spec.Env = append(spec.Env, "MCP_MUX_SUPERVISOR_COMMAND_EXIT=75")
	child, err := StartCommand(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-child.Wait():
		if exit.Code != 75 || exit.Err != nil {
			t.Fatalf("exit = %#v", exit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not report nonzero exit")
	}
}

func TestStartCommandValidation(t *testing.T) {
	if _, err := StartCommand(context.Background(), Command{}); err == nil {
		t.Fatal("empty command unexpectedly started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := StartCommand(ctx, commandHelperSpec()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled StartCommand error = %v", err)
	}
}

func TestCommandStartErrorMapsUnprovenRollback(t *testing.T) {
	err := commandStartError(procgroup.ErrTreeRetirementUnproven)
	if !errors.Is(err, ErrStartRollbackUnproven) {
		t.Fatalf("command start error = %v", err)
	}
}
