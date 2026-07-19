//go:build !windows

package procgroup

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestProcessGroupRetirementFinalProofSupersedesSignalErrors(t *testing.T) {
	if err := processGroupRetirementError(nil, syscall.EPERM, syscall.EPERM); err != nil {
		t.Fatalf("successful final proof retained signal error: %v", err)
	}

	proofErr := fmt.Errorf("probe process group: %w", syscall.EPERM)
	err := processGroupRetirementError(proofErr, syscall.EPERM, syscall.EPERM)
	if !errors.Is(err, syscall.EPERM) || !errors.Is(err, proofErr) {
		t.Fatalf("failed final proof lost fail-closed errors: %v", err)
	}
}
