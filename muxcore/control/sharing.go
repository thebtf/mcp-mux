package control

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/era"
)

// SendSpawn obtains a fresh daemon binding for explicit known-partition sharing.
// Preparation failure never falls back to an unbound or legacy spawn.
func SendSpawn(socketPath string, req Request, timeout time.Duration) (*Response, error) {
	if req.Cmd != "spawn" || timeout <= 0 {
		return nil, fmt.Errorf("control: invalid spawn exchange")
	}
	if req.Partition != nil && (req.Partition.Known != (req.Partition.Key != "")) {
		return nil, fmt.Errorf("control: invalid partition assertion")
	}
	if req.ModernSharing && req.ProtocolEra != "2026-07-28" {
		return nil, fmt.Errorf("control: modern sharing requires modern protocol")
	}
	if req.ModernSharing && req.Mode != "isolated" && req.Partition != nil && req.Partition.Known {
		deadline := time.Now().Add(timeout)
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, fmt.Errorf("control: sharing nonce: %w", err)
		}
		req.SpawnNonce = hex.EncodeToString(nonce[:])
		req.SharingBinding = ""
		prepare := req
		prepare.Cmd = "prepare_modern_sharing"
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("control: modern sharing admission deadline exceeded")
		}
		prepared, err := SendWithTimeout(socketPath, prepare, remaining)
		if err != nil {
			return nil, fmt.Errorf("control: modern sharing preparation: %w", err)
		}
		if err := prepared.Err(); err != nil {
			return nil, fmt.Errorf("control: modern sharing preparation: %w", err)
		}
		if prepared.ProtocolEra != req.ProtocolEra || prepared.SharingBinding == "" || prepared.Token != "" || prepared.ServerID != "" || prepared.IPCPath != "" {
			return nil, fmt.Errorf("control: invalid modern sharing preparation response")
		}
		req.SharingBinding = prepared.SharingBinding
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return nil, fmt.Errorf("control: modern sharing admission deadline exceeded")
		}
	}
	resp, err := SendWithTimeout(socketPath, req, timeout)
	if err != nil {
		return nil, err
	}
	if resp.OK && req.ProtocolEra != "" {
		if resp.ProtocolEra != req.ProtocolEra {
			return nil, era.NewAdmissionError(era.AdmissionControlEraMismatch)
		}
		if resp.Token == "" || resp.ServerID == "" || resp.IPCPath == "" {
			return nil, fmt.Errorf("control: incomplete modern spawn response")
		}
	}
	return resp, nil
}
