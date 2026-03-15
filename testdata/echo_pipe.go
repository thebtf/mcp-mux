// echo_pipe.go reads lines from stdin and writes them back to stdout.
// Used as a test helper for upstream process tests.
package main

import (
	"bufio"
	"fmt"
	"os"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		fmt.Fprintln(os.Stdout, scanner.Text())
	}
}
