//go:build !windows

package main

import (
	"fmt"
	"os"
)

func directParentExecutable() (string, error) {
	// /proc is available on the Unix platforms used by the product test suite.
	// Platforms without it fail closed rather than trusting an inherited env.
	return os.Readlink(fmt.Sprintf("/proc/%d/exe", os.Getppid()))
}
