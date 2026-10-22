package envidentity

import (
	"runtime"
	"strings"
)

// IsLauncherAdvertisement identifies controller-only values, not upstream env.
func IsLauncherAdvertisement(key string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(key, "MCPMUX_LAUNCHER_ATTESTATION") || strings.EqualFold(key, "MCPMUX_LAUNCHER_PROTOCOL")
	}
	return key == "MCPMUX_LAUNCHER_ATTESTATION" || key == "MCPMUX_LAUNCHER_PROTOCOL"
}
