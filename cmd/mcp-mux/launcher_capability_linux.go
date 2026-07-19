//go:build linux

package main

import (
	"fmt"
	"os"
)

func directParentExecutable() (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/%d/exe", os.Getppid()))
}
