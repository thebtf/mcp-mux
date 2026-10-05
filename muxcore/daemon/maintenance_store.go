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
	maintenanceSchema     = 2
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
	Version     int                 `json:"version"`
	Scope       string              `json:"scope"`
	Transaction string              `json:"transaction"`
	Leases      []maintenanceRecord `json:"leases"`
}

// transaction.json is a mandatory member of the same private ledger authority,
// not an advisory sidecar. A prepared transaction always refuses recovery.
type maintenanceTransaction struct {
	Version     int                 `json:"version"`
	Scope       string              `json:"scope"`
	Transaction string              `json:"transaction"`
	Target      string              `json:"target"`
	State       string              `json:"state"`
	Predecessor []maintenanceRecord `json:"predecessor,omitempty"`
}

func maintenanceTransactionPath(path string) string {
	return filepath.Join(filepath.Dir(path), "transaction.json")
}

func maintenanceLedgerDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return maintenanceKeyVersion + hex.EncodeToString(digest[:])
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

// The endpoint leaf is identity, not a symlink-resolution input: a socket can
// appear or disappear while its namespace authority must remain unchanged.
func canonicalMaintenancePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil || path == "" {
		return ""
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return ""
	}
	canonical := filepath.Join(parent, filepath.Base(absolute))
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	return canonical
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
	fields = append(fields, serverid.CanonicalizePath(cwd), envidentity.Build(normalized).Fingerprint)
	return maintenanceDigest(fields...)
}

func validMaintenanceDigest(key string) bool {
	if len(key) != 67 || !strings.HasPrefix(key, maintenanceKeyVersion) {
		return false
	}
	for _, char := range key[3:] {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func (d *Daemon) loadMaintenance(endpoint string) error {
	path, scope, ledger, err := readMaintenanceAuthority(d.namespace, endpoint)
	if err != nil {
		return err
	}
	d.maintenanceLockPath = serverid.DaemonLockPath(filepath.Dir(endpoint), d.namespace)
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
	return nil
}

func readMaintenanceAuthority(namespace, endpoint string) (string, string, *maintenanceLedger, error) {
	if endpoint == "" {
		return "", "", nil, control.ErrMaintenancePersistenceFailed
	}
	absolute := endpoint
	if !filepath.IsAbs(absolute) {
		// Do not clean away original components before authenticating them.
		// Drive-relative/root-relative Windows paths lack a bound directory.
		if filepath.VolumeName(absolute) != "" || os.IsPathSeparator(absolute[0]) {
			return "", "", nil, control.ErrMaintenancePersistenceFailed
		}
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", nil, control.ErrMaintenancePersistenceFailed
		}
		absolute = cwd + string(os.PathSeparator) + absolute
	}
	if validateMaintenanceParent(maintenanceOriginalParent(absolute)) != nil {
		return "", "", nil, control.ErrMaintenancePersistenceFailed
	}
	canonical := canonicalMaintenancePath(absolute)
	if canonical == "" {
		return "", "", nil, control.ErrMaintenancePersistenceFailed
	}
	scope := maintenanceDigest("mcp-mux-maintenance-scope-v1", namespace, canonical)
	// Keep durable authority beside the namespace lock, independent of the
	// caller's mutable user-config environment and the endpoint leaf lifetime.
	path := filepath.Join(serverid.DaemonLockPath(filepath.Dir(canonical), namespace)+".maintenance", scope[3:], "ledger.json")
	if err := checkMaintenanceStorePaths(path, false); err != nil {
		return path, scope, nil, control.ErrMaintenancePersistenceFailed
	}
	data, ledgerErr := os.ReadFile(path)
	transactionData, transactionErr := os.ReadFile(maintenanceTransactionPath(path))
	if errors.Is(ledgerErr, os.ErrNotExist) && errors.Is(transactionErr, os.ErrNotExist) {
		// An existing namespace directory without its complete authority is not
		// a cold start (including a crash during the first preparation).
		if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
			return path, scope, nil, nil
		}
		return path, scope, nil, control.ErrMaintenancePersistenceFailed
	}
	if ledgerErr != nil || transactionErr != nil {
		return path, scope, nil, control.ErrMaintenancePersistenceFailed
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return path, scope, nil, control.ErrMaintenancePersistenceFailed
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() != "ledger.json" && entry.Name() != "transaction.json" && !strings.HasPrefix(entry.Name(), ".ledger-") {
			return path, scope, nil, control.ErrMaintenancePersistenceFailed
		}
		if err := validateMaintenancePath(filepath.Join(filepath.Dir(path), entry.Name()), false); err != nil {
			return path, scope, nil, control.ErrMaintenancePersistenceFailed
		}
	}
	var ledger maintenanceLedger
	if !decodeMaintenanceJSON(data, &ledger) || ledger.Version != maintenanceSchema || ledger.Scope != scope || !validMaintenanceDigest(ledger.Transaction) || ledger.Leases == nil {
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
	var transaction maintenanceTransaction
	if !decodeMaintenanceJSON(transactionData, &transaction) || transaction.Version != maintenanceSchema || transaction.Scope != scope || transaction.Transaction != ledger.Transaction || transaction.Target != maintenanceLedgerDigest(data) || transaction.State != "committed" || transaction.Predecessor != nil {
		return path, scope, nil, control.ErrMaintenancePersistenceFailed
	}
	return path, scope, &ledger, nil
}

func decodeMaintenanceJSON(data []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return unambiguousMaintenanceJSON(data) && decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
}

// encoding/json accepts duplicate and case-equivalent struct members. Authority
// cannot use that last-member-wins rule. The ledger schema has at most four
// container levels and six members per object; unknown fields are checked by
// the typed decoder after this bounded token pass.
func unambiguousMaintenanceJSON(data []byte) bool {
	type frame struct {
		object bool
		key    bool
		names  [6]string
		count  int
	}
	var stack [4]frame
	depth := 0
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return depth == 0
		}
		if err != nil {
			return false
		}
		if depth > 0 && stack[depth-1].object && stack[depth-1].key {
			if name, ok := token.(string); ok {
				current := &stack[depth-1]
				if current.count == len(current.names) {
					return false
				}
				for _, previous := range current.names[:current.count] {
					if strings.EqualFold(previous, name) {
						return false
					}
				}
				current.names[current.count] = name
				current.count++
				current.key = false
				continue
			}
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				if depth == len(stack) {
					return false
				}
				stack[depth] = frame{object: delimiter == '{', key: delimiter == '{'}
				depth++
				continue
			case '}', ']':
				if depth == 0 {
					return false
				}
				depth--
			}
		}
		if depth > 0 && stack[depth-1].object {
			stack[depth-1].key = true
		}
	}
}

func maintenanceRecords(leases map[string]*maintenanceLease) []maintenanceRecord {
	records := make([]maintenanceRecord, 0, len(leases))
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
		records = append(records, record)
	}
	return records
}

func (d *Daemon) persistMaintenanceLocked(leases map[string]*maintenanceLease) error {
	token, err := generateToken()
	if err != nil {
		return control.ErrMaintenancePersistenceFailed
	}
	ledger := maintenanceLedger{Version: maintenanceSchema, Scope: d.maintenanceScope, Transaction: maintenanceDigest("mcp-mux-maintenance-transaction-v2", token), Leases: maintenanceRecords(leases)}
	return commitMaintenanceStore(d.maintenancePath, ledger, maintenanceRecords(d.maintenanceLeases), func(path string, data []byte) error {
		if path == d.maintenancePath && d.maintenanceCommit != nil {
			return d.maintenanceCommit(data)
		}
		return writeMaintenanceLedger(path, data)
	})
}

// All mutations run under the existing namespace file lock, before admission
// and registry locks. Preparation must be acknowledged durable before the main
// ledger can change. A publish error leaves the prepared predecessor in place,
// even when rename succeeded. Recovery reads both members and refuses pending,
// missing, mismatched, or unknown authority without performing repair writes.
//
// Finalization is a certificate that the target publication was ACKNOWLEDGED
// durable, and is attempted only after that acknowledgment. Its own write may
// fail after publication: recovery then either fences on prepared/invalid bytes,
// or verifies the certificate and proves the earlier target commit completed.
// The error is still returned and live admission retains its predecessor. No
// authority file is removed; acknowledged successful finalization cannot later
// resurrect a predecessor. This does not claim atomic replacement of two files.
func commitMaintenanceStore(path string, ledger maintenanceLedger, predecessor []maintenanceRecord, writer func(string, []byte) error) error {
	data, err := json.Marshal(ledger)
	if err != nil {
		return control.ErrMaintenancePersistenceFailed
	}
	transaction := maintenanceTransaction{Version: maintenanceSchema, Scope: ledger.Scope, Transaction: ledger.Transaction, Target: maintenanceLedgerDigest(data), State: "prepared", Predecessor: predecessor}
	prepared, err := json.Marshal(transaction)
	if err != nil {
		return control.ErrMaintenancePersistenceFailed
	}
	transactionPath := maintenanceTransactionPath(path)
	if err := writer(transactionPath, prepared); err != nil {
		return control.ErrMaintenancePersistenceFailed
	}
	if err := writer(path, data); err != nil {
		return control.ErrMaintenancePersistenceFailed
	}
	transaction.State = "committed"
	transaction.Predecessor = nil
	committed, err := json.Marshal(transaction)
	if err != nil || writer(transactionPath, committed) != nil {
		return control.ErrMaintenancePersistenceFailed
	}
	return nil
}

// Authenticate link objects and their original parents before resolving them.
// Canonicalization alone would erase a foreign-owned, retargetable alias.
func validateMaintenanceTraversal(path string, links int, validate func(string, os.FileInfo, bool) error) error {
	_, err := resolveMaintenanceTraversal(path, &links, validate)
	return err
}

func maintenanceOriginalParent(path string) string {
	volume := len(filepath.VolumeName(path))
	for index := len(path) - 1; index >= volume; index-- {
		if os.IsPathSeparator(path[index]) {
			if index == volume {
				return path[:index+1]
			}
			return path[:index]
		}
	}
	return ""
}

func resolveMaintenanceTraversal(path string, links *int, validate func(string, os.FileInfo, bool) error) (string, error) {
	if !filepath.IsAbs(path) {
		return "", control.ErrMaintenancePersistenceFailed
	}
	start := len(filepath.VolumeName(path)) + 1
	if start > len(path) {
		return "", control.ErrMaintenancePersistenceFailed
	}
	resolved := path[:start]
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if err := validate(resolved, info, false); err != nil {
		return "", err
	}
	for start < len(path) {
		if os.IsPathSeparator(path[start]) {
			start++
			continue
		}
		end := start
		for end < len(path) && !os.IsPathSeparator(path[end]) {
			end++
		}
		component := path[start:end]
		start = end
		switch component {
		case ".":
			continue
		case "..":
			// Prior components, including link objects/targets, were already
			// authenticated. Only now may kernel-style parent traversal occur.
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, component)
		info, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		symbolic := info.Mode()&os.ModeSymlink != 0
		if err := validate(next, info, symbolic); err != nil {
			return "", err
		}
		if !symbolic {
			resolved = next
			continue
		}
		if *links == 0 {
			return "", control.ErrMaintenancePersistenceFailed
		}
		*links = *links - 1
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			if filepath.VolumeName(target) != "" || target == "" || os.IsPathSeparator(target[0]) {
				return "", control.ErrMaintenancePersistenceFailed
			}
			target = resolved + string(os.PathSeparator) + target
		}
		resolved, err = resolveMaintenanceTraversal(target, links, validate)
		if err != nil {
			return "", err
		}
	}
	return resolved, nil
}

// The two private directories are authority boundaries, not directories to
// adopt or repair. Readers validate without creating or changing permissions.
func checkMaintenanceStorePaths(path string, create bool) error {
	dir := filepath.Dir(path)
	anchor := filepath.Dir(dir)
	if err := validateMaintenanceParent(filepath.Dir(anchor)); err != nil {
		return err
	}
	for _, directory := range [2]string{anchor, dir} {
		if create {
			if err := createMaintenanceDirectory(directory); err != nil {
				return err
			}
		}
		if err := validateMaintenancePath(directory, true); err != nil {
			if !create && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
	}
	for _, member := range [3]string{path, filepath.Join(dir, "ledger.json"), maintenanceTransactionPath(path)} {
		if err := validateMaintenancePath(member, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func writeMaintenanceLedger(path string, data []byte) error {
	if path == "" {
		return control.ErrMaintenancePersistenceFailed
	}
	dir := filepath.Dir(path)
	if err := checkMaintenanceStorePaths(path, true); err != nil {
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
