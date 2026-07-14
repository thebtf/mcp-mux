//go:build !windows && !linux && !darwin

package main

import "fmt"

// Unsupported platforms intentionally fail closed: launcher-private frames are
// never sent unless the direct parent executable can be independently proved.
func directParentExecutable() (string, error) {
	return "", fmt.Errorf("direct parent executable attestation unsupported")
}
