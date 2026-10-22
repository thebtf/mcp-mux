package daemon

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const modernSharingBindingTTL = 30 * time.Second

var modernSharingNow = time.Now

var errModernSharingAdmission = errors.New("invalid modern sharing admission")

type modernCompatibility struct {
	Command     string
	Args        []string
	Cwd         string
	Env         map[string]string
	Partition   control.PartitionAssertion
	ProtocolEra string
	Mode        string
}

func (c *modernCompatibility) equal(other *modernCompatibility) bool {
	return c != nil && other != nil && c.Command == other.Command &&
		slices.Equal(c.Args, other.Args) && c.Cwd == other.Cwd &&
		maps.Equal(c.Env, other.Env) && c.Partition == other.Partition &&
		c.ProtocolEra == other.ProtocolEra && c.Mode == other.Mode
}

// normalizeModernSpawn fixes the exact launch context before binding or election.
func normalizeModernSpawn(req control.Request) (control.Request, *modernCompatibility, error) {
	protocolEra, err := era.ParseProtocolEra(req.ProtocolEra)
	if err != nil {
		return req, nil, errModernSharingAdmission
	}
	if req.Partition != nil && req.Partition.Known != (req.Partition.Key != "") {
		return req, nil, errModernSharingAdmission
	}
	if protocolEra != era.EraModern20260728 {
		if req.ModernSharing || req.Partition != nil || req.SpawnNonce != "" || req.SharingBinding != "" {
			return req, nil, errModernSharingAdmission
		}
		return req, nil, nil
	}
	req.Cwd = serverid.CanonicalizePath(req.Cwd)
	if strings.ContainsAny(req.Command, `/\`) {
		command := req.Command
		if !filepath.IsAbs(command) {
			command = filepath.Join(req.Cwd, command)
		}
		req.Command = serverid.CanonicalizePath(command)
	} else if runtime.GOOS == "windows" {
		req.Command = strings.ToLower(req.Command)
	}
	if len(req.Args) == 0 {
		req.Args = nil
	}
	req.Args = slices.Clone(req.Args)
	req.Env = mergeEnv(req.Env)
	shared := req.ModernSharing && req.Mode != "isolated" && req.Partition != nil && req.Partition.Known
	if !shared {
		if req.SharingBinding != "" {
			return req, nil, errModernSharingAdmission
		}
		req.Mode = "isolated"
		return req, nil, nil
	}
	if req.Mode != "" && req.Mode != "global" {
		return req, nil, errModernSharingAdmission
	}
	req.Mode = "global"
	partition := *req.Partition
	req.Partition = &partition
	compatibility := &modernCompatibility{
		Command: req.Command, Args: slices.Clone(req.Args), Cwd: req.Cwd, Env: maps.Clone(req.Env),
		Partition: partition, ProtocolEra: req.ProtocolEra, Mode: req.Mode,
	}
	return req, compatibility, nil
}

// HandlePrepareModernSharing returns only expiry and a daemon-private MAC.
func (d *Daemon) HandlePrepareModernSharing(req control.Request) (string, error) {
	if d.shuttingDown.Load() {
		return "", ErrDaemonShuttingDown
	}
	if req.SharingBinding != "" {
		return "", errModernSharingAdmission
	}
	normalized, compatibility, err := normalizeModernSpawn(req)
	if err != nil || compatibility == nil || !validSpawnNonce(normalized.SpawnNonce) {
		return "", errModernSharingAdmission
	}
	if err := d.maintenanceAdmission(d.maintenanceContext(era.EraModern20260728, normalized.Command, normalized.Args, normalized.Cwd, normalized.Env)); err != nil {
		return "", err
	}
	var issued [8]byte
	binary.BigEndian.PutUint64(issued[:], uint64(modernSharingNow().UnixNano()))
	mac, err := d.modernSharingMAC(compatibility, normalized.SpawnNonce, issued[:])
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(append(issued[:], mac...)), nil
}

func validSpawnNonce(nonce string) bool {
	if len(nonce) != 32 {
		return false
	}
	_, err := hex.DecodeString(nonce)
	return err == nil
}

func (d *Daemon) modernSharingMAC(compatibility *modernCompatibility, nonce string, issued []byte) ([]byte, error) {
	if d.modernSharingKey == [32]byte{} {
		return nil, errModernSharingAdmission
	}
	mac := hmac.New(sha256.New, d.modernSharingKey[:])
	mac.Write([]byte("mcp-mux/modern-sharing/v1\x00"))
	mac.Write(issued)
	if err := json.NewEncoder(mac).Encode(struct {
		Context *modernCompatibility
		Nonce   string
	}{compatibility, nonce}); err != nil {
		return nil, errModernSharingAdmission
	}
	return mac.Sum(nil), nil
}

func (d *Daemon) validateModernSharing(req control.Request, compatibility *modernCompatibility) error {
	if !validSpawnNonce(req.SpawnNonce) {
		return errModernSharingAdmission
	}
	binding, err := base64.RawURLEncoding.DecodeString(req.SharingBinding)
	if err != nil || len(binding) != 8+sha256.Size {
		return errModernSharingAdmission
	}
	issued := time.Unix(0, int64(binary.BigEndian.Uint64(binding[:8])))
	age := modernSharingNow().Sub(issued)
	if age < 0 || age > modernSharingBindingTTL {
		return errModernSharingAdmission
	}
	mac, err := d.modernSharingMAC(compatibility, req.SpawnNonce, binding[:8])
	if err != nil || !hmac.Equal(binding[8:], mac) {
		return errModernSharingAdmission
	}
	return nil
}
