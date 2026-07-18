package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "daemon" {
		os.Exit(2)
	}
	readyFile := os.Getenv("MCPMUX_DETACHED_DAEMON_READY_FILE")
	token := os.Getenv("MCPMUX_DETACHED_DAEMON_TOKEN")
	if readyFile == "" || token == "" {
		os.Exit(3)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(4)
	}
	defer listener.Close()
	if err := os.WriteFile(readyFile, []byte(fmt.Sprintf("%d\n%s\n", os.Getpid(), listener.Addr().String())), 0o600); err != nil {
		os.Exit(5)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if tcp, ok := listener.(*net.TCPListener); ok {
			_ = tcp.SetDeadline(time.Now().Add(250 * time.Millisecond))
		}
		conn, err := listener.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}
		stop := handle(conn, token)
		_ = conn.Close()
		if stop {
			return
		}
	}
}

func handle(conn net.Conn, token string) bool {
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return false
	}
	switch strings.TrimSpace(scanner.Text()) {
	case token + " status":
		_, _ = fmt.Fprintln(conn, "ok")
	case token + " shutdown":
		_, _ = fmt.Fprintln(conn, "bye")
		return true
	default:
		_, _ = fmt.Fprintln(conn, "denied")
	}
	return false
}
