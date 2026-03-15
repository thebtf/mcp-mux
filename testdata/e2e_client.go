// e2e_test.go runs an end-to-end test of the mcp-mux binary.
//
// Usage: go run testdata/e2e_test.go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	binary := ".\\mcp-mux.exe"
	if _, err := os.Stat(binary); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "Build mcp-mux.exe first: go build -o mcp-mux.exe ./cmd/mcp-mux")
		os.Exit(1)
	}

	fmt.Println("=== E2E Test: mcp-mux with mock_server ===")

	// Start mcp-mux wrapping mock_server
	cmd := exec.Command(binary, "go", "run", "testdata/mock_server.go")
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatal("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		fatal("start: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Give upstream time to start
	time.Sleep(2 * time.Second)

	// Test 1: initialize
	fmt.Print("Test 1: initialize... ")
	send(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	resp := recv(scanner)
	assertContains(resp, "mock-server", "initialize response")
	assertID(resp, 1)
	fmt.Println("PASS")

	// Test 2: tools/list
	fmt.Print("Test 2: tools/list... ")
	send(stdin, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	resp = recv(scanner)
	assertContains(resp, "echo", "tools/list response")
	assertID(resp, 2)
	fmt.Println("PASS")

	// Test 3: tools/call
	fmt.Print("Test 3: tools/call... ")
	send(stdin, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hello-e2e"}}}`)
	resp = recv(scanner)
	assertContains(resp, "hello-e2e", "tools/call response")
	assertID(resp, 3)
	fmt.Println("PASS")

	// Test 4: ping
	fmt.Print("Test 4: ping... ")
	send(stdin, `{"jsonrpc":"2.0","id":4,"method":"ping","params":{}}`)
	resp = recv(scanner)
	assertID(resp, 4)
	fmt.Println("PASS")

	// Clean up
	stdin.Close()
	cmd.Wait()

	fmt.Println("\n=== All E2E tests passed! ===")
}

func send(w io.Writer, msg string) {
	_, err := fmt.Fprintln(w, msg)
	if err != nil {
		fatal("send: %v", err)
	}
}

func recv(s *bufio.Scanner) string {
	done := make(chan bool, 1)
	var line string
	go func() {
		if s.Scan() {
			line = s.Text()
		}
		done <- true
	}()

	select {
	case <-done:
		return line
	case <-time.After(10 * time.Second):
		fatal("recv timeout — no response within 10s")
		return ""
	}
}

func assertContains(resp, substr, label string) {
	if !strings.Contains(resp, substr) {
		fatal("%s: response missing %q:\n%s", label, substr, resp)
	}
}

func assertID(resp string, expected int) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(resp), &obj); err != nil {
		fatal("parse response: %v\nraw: %s", err, resp)
	}
	var id int
	if err := json.Unmarshal(obj["id"], &id); err != nil {
		fatal("parse id: %v (raw: %s)", err, string(obj["id"]))
	}
	if id != expected {
		fatal("id = %d, want %d\nfull: %s", id, expected, resp)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}
