package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/era"
	"github.com/thebtf/mcp-mux/muxcore/internal/envidentity"
	"github.com/thebtf/mcp-mux/muxcore/serverid"
)

const (
	maintenanceSchema     = 1
	maintenanceKeyVersion = "v1:"
)

type maintenanceRecord struct {
	HoldID        string                   `json:"hold_id"`
	Keys          []string                 `json:"keys"`
	State         control.MaintenanceState `json:"state"`
	ExpiresAt     time.Time                `json:"expires_at"`
	DrainDeadline time.Time                `json:"drain_deadline"`
}
type maintenanceLedger struct {
	Version int                 `json:"version"`
	Scope   string              `json:"scope"`
	Leases  []maintenanceRecord `json:"leases"`
}

func maintenanceDigest(fields ...string) string {
	h := sha256.New()
	for _, field := range fields {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = h.Write(size[:])
		_, _ = io.WriteString(h, field)
	}
	return maintenanceKeyVersion + hex.EncodeToString(h.Sum(nil))
}

func canonicalMaintenancePath(path string) string {
	if path == "" {
		path, _ = os.Getwd()
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return serverid.CanonicalizePath(path)
}

func (d *Daemon) maintenanceContext(protocolEra era.ProtocolEra, command string, args []string, cwd string, env map[string]string) string {
	normalized := env
	if runtime.GOOS == "windows" {
		normalized = make(map[string]string, len(env))
		keys := make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			normalized[strings.ToUpper(key)] = env[key]
		}
	}
	wire, _ := protocolEra.Wire()
	fields := []string{"mcp-mux-maintenance-context-v1", d.maintenanceScope, wire, command}
	// Include argv count as well as field lengths, so trailing CWD cannot alias argv.
	fields = append(fields, string(binary.BigEndian.AppendUint64(nil, uint64(len(args)))))
	fields = append(fields, args...)
	fields = append(fields, canonicalMaintenancePath(cwd), envidentity.Build(normalized).Fingerprint)
	return maintenanceDigest(fields...)
}

func validMaintenanceDigest(key string) bool {
	if len(key) != 67 || !strings.HasPrefix(key, maintenanceKeyVersion) {
		return false
	}
	_, err := hex.DecodeString(key[3:])
	return err == nil
}

func (d *Daemon) loadMaintenance(endpoint string) error {
	d.maintenanceLockPath = serverid.DaemonLockPath(filepath.Dir(endpoint), d.namespace)
	path, scope, ledger, err := readMaintenanceAuthority(d.namespace, endpoint)
	if err != nil {
		return err
	}
	d.maintenancePath, d.maintenanceScope = path, scope
	d.maintenanceLeases = make(map[string]*maintenanceLease)
	if ledger == nil {
		return nil
	}
	for _, record := range ledger.Leases {
		result := control.MaintenanceResult{HoldID: record.HoldID, State: record.State, ExpiresAt: record.ExpiresAt, DrainDeadline: record.DrainDeadline, TreesRetired: record.State == control.MaintenanceHeld}
		if result.State != control.MaintenanceHeld {
			result.State = control.MaintenanceRetirementBlocked
		}
		d.maintenanceLeases[result.HoldID] = &maintenanceLease{record: record, result: result, recovered: true}
	}
	// The starter may own the namespace lock through readiness: never mutate
	// authority here. Expired HELD remains fenced until lock-first release.
	for _, lease := range d.maintenanceLeases {
		d.scheduleMaintenanceExpiryLocked(lease)
	}
	return nil
}

func readMaintenanceAuthority(namespace, endpoint string) (string, string, *maintenanceLedger, error) {
	scope := maintenanceDigest("mcp-mux-maintenance-scope-v1", namespace, canonicalMaintenancePath(endpoint))
	root, err := os.UserConfigDir()
	if err != nil {
		return "", scope, nil, control.ErrMaintenancePersistenceFailed
	}
	path := filepath.Join(root, "mcp-mux", "maintenance", scope[3:], "ledger.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, scope, nil, nil
	}
	if err != nil {
		return path, scope, nil, control.ErrMaintenancePersistenceFailed
	}
	var ledger maintenanceLedger
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ledger) != nil || decoder.Decode(new(any)) != io.EOF || ledger.Version != maintenanceSchema || ledger.Scope != scope || ledger.Leases == nil {
		return path, scope, nil, control.ErrMaintenancePersistenceFailed
	}
	used := make(map[string]bool)
	ids := make(map[string]bool)
	for _, record := range ledger.Leases {
		if record.HoldID == "" || strings.TrimSpace(record.HoldID) != record.HoldID || ids[record.HoldID] || len(record.Keys) == 0 || record.ExpiresAt.IsZero() || record.DrainDeadline.IsZero() {
			return path, scope, nil, control.ErrMaintenancePersistenceFailed
		}
		ids[record.HoldID] = true
		switch record.State {
		case control.MaintenanceHeld, control.MaintenanceHolding, control.MaintenanceRetirementBlocked:
		default:
			return path, scope, nil, control.ErrMaintenancePersistenceFailed
		}
		for _, key := range record.Keys {
			if !validMaintenanceDigest(key) || used[key] {
				return path, scope, nil, control.ErrMaintenancePersistenceFailed
			}
			used[key] = true
		}
	}
	return path, scope, &ledger, nil
}

func (d *Daemon) persistMaintenanceLocked(leases map[string]*maintenanceLease) error {
	ledger := maintenanceLedger{Version: maintenanceSchema, Scope: d.maintenanceScope, Leases: make([]maintenanceRecord, 0, len(leases))}
	ids := make([]string, 0, len(leases))
	for id := range leases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		lease := leases[id]
		record := lease.record
		record.State = lease.result.State
		record.ExpiresAt = lease.result.ExpiresAt
		record.DrainDeadline = lease.result.DrainDeadline
		ledger.Leases = append(ledger.Leases, record)
	}
	data, err := json.Marshal(ledger)
	if err != nil {
		return control.ErrMaintenancePersistenceFailed
	}
	if d.maintenanceCommit != nil {
		err = d.maintenanceCommit(data)
	} else {
		err = writeMaintenanceLedger(d.maintenancePath, data)
	}
	if err != nil {
		return control.ErrMaintenancePersistenceFailed
	}
	return nil
}

func writeMaintenanceLedger(path string, data []byte) error {
	if path == "" {
		return control.ErrMaintenancePersistenceFailed
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := secureMaintenancePath(dir, true); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".ledger-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	// Uncommitted temporary bytes are not authority. Never delete the old ledger.
	defer os.Remove(temp)
	if err = secureMaintenancePath(temp, false); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return replaceMaintenanceLedger(temp, path)
}
