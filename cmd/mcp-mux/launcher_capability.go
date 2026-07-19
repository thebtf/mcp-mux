package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/supervisor/attest"
	"github.com/thebtf/mcp-mux/muxcore/upgrade"
)

const (
	launcherAttestationLifetime = 5 * time.Second
	launcherAttestationIO       = 2 * time.Second
)

var (
	launcherCurrentExecutable = os.Executable
	launcherParentExecutable  = directParentExecutable
	launcherActivePointer     = resolveActiveEnginePointer
	launcherAttestationStart  = startLauncherAttestation
	launcherAttestationBind   = func(parent *attest.Parent, pid int) error {
		return parent.BindChildPID(pid)
	}
	launcherAttestationProbe = verifyLauncherAttestation
)

// launcherLifecycleCapable proves that a current engine was directly launched
// by the stable launcher installed beside its version store. Environment paths
// are only advertisements: old launchers must never receive private dormant
// frames on host stdout.
func launcherLifecycleCapable() bool {
	attestationPath, launcherPID, ok := launcherAttestationAdvertisement()
	if !ok {
		return false
	}
	enginePath, launcherPath, activeFile, ok := currentEngineInstallLayout()
	if !ok || !launcherActiveEngineMatches(enginePath, activeFile) {
		return false
	}
	parentPath, err := launcherParentExecutable()
	return err == nil && samePath(parentPath, launcherPath) && launcherAttestationProbe(attestationPath, launcherPID)
}

func launcherAttestationAdvertisement() (string, int, bool) {
	parts := strings.Split(strings.TrimSpace(os.Getenv(envLauncherProtocol)), ":")
	if len(parts) != 2 || parts[0] != attest.VersionV2 {
		return "", 0, false
	}
	launcherPID, err := strconv.Atoi(parts[1])
	attestationPath := strings.TrimSpace(os.Getenv(envLauncherAttestation))
	if err != nil || launcherPID <= 0 || launcherPID != os.Getppid() || attestationPath == "" {
		return "", 0, false
	}
	return attestationPath, launcherPID, true
}

func launcherAttestationCapable() bool {
	attestationPath, launcherPID, ok := launcherAttestationAdvertisement()
	return ok && launcherAttestationProbe(attestationPath, launcherPID)
}

func startLauncherAttestation() (*attest.Parent, error) {
	return attest.StartParent(context.Background(), attest.ParentConfig{
		Lifetime:  launcherAttestationLifetime,
		IOTimeout: launcherAttestationIO,
	})
}

func verifyLauncherAttestation(path string, launcherPID int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), launcherAttestationIO)
	defer cancel()
	err := attest.VerifyParent(ctx, attest.VerifyConfig{
		Advertisement: attest.Advertisement{
			Version:   attest.VersionV2,
			ParentPID: launcherPID,
			Endpoint:  path,
		},
		DirectParentPID: launcherPID,
		IOTimeout:       launcherAttestationIO,
	})
	return err == nil
}

// launcherBootstrapEligible is deliberately stricter than an env check. It
// permits one active engine child of an old launcher to update the stable
// launcher for future invocations, but it does not let a copied/inherited env
// replace an arbitrary executable.
func launcherBootstrapEligible() bool {
	enginePath, launcherPath, activeFile, ok := currentEngineInstallLayout()
	if !ok || !launcherActiveEngineMatches(enginePath, activeFile) {
		return false
	}
	parentPath, err := launcherParentExecutable()
	return err == nil && samePath(parentPath, launcherPath)
}

func launcherActiveEngineMatches(enginePath, activeFile string) bool {
	activePath, ok := launcherActivePointer(activeFile)
	return ok && samePath(activePath, enginePath)
}

// currentEngineInstallLayout derives the only stable launcher that can govern
// this engine. Custom or copied engine paths deliberately fail closed.
func currentEngineInstallLayout() (enginePath, launcherPath, activeFile string, ok bool) {
	enginePath, err := launcherCurrentExecutable()
	if err != nil || filepath.Base(enginePath) != engineFileName() {
		return "", "", "", false
	}
	versionDir := filepath.Dir(enginePath)
	storeDir := filepath.Dir(versionDir)
	launcherName := launcherFileName()
	if filepath.Base(versionDir) == "." || filepath.Base(storeDir) != "mcp-mux.versions" {
		return "", "", "", false
	}
	launcherPath = filepath.Join(filepath.Dir(storeDir), launcherName)
	if !samePath(storeDir, versionStoreDir(launcherPath)) {
		return "", "", "", false
	}
	return enginePath, launcherPath, activeEngineFile(launcherPath), true
}

// bootstrapStableLauncher copies the verified active engine beside the running
// stable launcher and uses upgrade.Swap's two-rename rollback contract. It
// never touches the current launcher process in place, and a concurrent child
// simply leaves the winner's result intact.
func bootstrapStableLauncher() (bool, error) {
	if !launcherBootstrapEligible() {
		return false, fmt.Errorf("launcher bootstrap identity proof failed")
	}
	enginePath, launcherPath, _, ok := currentEngineInstallLayout()
	if !ok {
		return false, fmt.Errorf("resolve installed launcher identity")
	}
	if sameFile(launcherPath, enginePath) {
		return false, nil
	}

	lock, err := os.OpenFile(launcherPath+".bootstrap.lock", os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("open launcher bootstrap lock: %w", err)
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		// Another verified child is already performing the same idempotent update.
		return false, nil
	}
	defer unlockFile(lock)
	if sameFile(launcherPath, enginePath) {
		return false, nil
	}

	staged := fmt.Sprintf("%s.bootstrap.%d", launcherPath, os.Getpid())
	if err := copyFile(enginePath, staged, 0o755); err != nil {
		return false, fmt.Errorf("stage stable launcher: %w", err)
	}
	oldPath, err := upgrade.Swap(launcherPath, staged)
	if err != nil {
		_ = os.Remove(staged)
		return false, fmt.Errorf("swap stable launcher: %w", err)
	}
	// The old file can still be mapped by the direct parent on Windows. Do not
	// delete it here; a future non-live maintenance operation may clean it.
	_ = oldPath
	return true, nil
}
