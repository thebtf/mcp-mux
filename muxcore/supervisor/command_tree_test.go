package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCommandTreeHelper(t *testing.T) {
	if os.Getenv("MCP_MUX_SUPERVISOR_TREE_GRANDCHILD") == "1" {
		for {
			time.Sleep(time.Second)
		}
	}
	if os.Getenv("MCP_MUX_SUPERVISOR_TREE_PARENT") != "1" {
		return
	}
	command := exec.Command(os.Args[0], "-test.run=TestCommandTreeHelper")
	command.Env = append(os.Environ(), "MCP_MUX_SUPERVISOR_TREE_GRANDCHILD=1")
	if err := command.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println(command.Process.Pid)
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestCommandChildStopRetiresDescendantTree(t *testing.T) {
	child, err := StartCommand(context.Background(), Command{
		Path:   os.Args[0],
		Args:   []string{"-test.run=TestCommandTreeHelper"},
		Env:    append(os.Environ(), "MCP_MUX_SUPERVISOR_TREE_PARENT=1"),
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(child.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	grandchildPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("grandchild PID %q: %v", line, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := child.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !processGone(grandchildPID) {
		t.Fatalf("grandchild pid %d survived terminal proof", grandchildPID)
	}
}
