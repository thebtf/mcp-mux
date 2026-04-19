//go:build unix

package daemon

import (
	"errors"
	"os"
	"testing"
)

func TestVerifyPIDOwner_OwnProcess(t *testing.T) {
	if err := verifyPIDOwner(os.Getpid()); err != nil {
		t.Errorf("own process rejected: %v", err)
	}
}

func TestVerifyPIDOwner_InvalidPID(t *testing.T) {
	err := verifyPIDOwner(-1)
	if !errors.Is(err, ErrPIDNotFound) {
		t.Errorf("expected ErrPIDNotFound for pid -1, got %v", err)
	}
	err = verifyPIDOwner(0)
	if !errors.Is(err, ErrPIDNotFound) {
		t.Errorf("expected ErrPIDNotFound for pid 0, got %v", err)
	}
}

func TestVerifyPIDOwner_PID1_NonRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; PID 1 ownership check is meaningless")
	}
	err := verifyPIDOwner(1)
	if !errors.Is(err, ErrPIDForeignOwner) {
		t.Errorf("expected ErrPIDForeignOwner for pid 1 as non-root, got %v", err)
	}
}
