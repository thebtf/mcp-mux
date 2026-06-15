// Package registry provides an opt-in descriptor registry for native muxcore
// daemons. Descriptors are advisory: callers must verify the advertised daemon
// over its control socket before trusting owner or lifecycle state.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/thebtf/mcp-mux/muxcore/control"
)

const (
	SchemaVersion = 1

	StateHealthy   = "healthy"
	StateStale     = "stale"
	StateInvalid   = "invalid"
	StateDuplicate = "duplicate"

	registryDirName = "muxcore-daemon-registry"
)

var (
	ErrInvalidDescriptor = errors.New("registry: invalid descriptor")
	ErrEngineNotFound    = errors.New("registry: engine not found")
	ErrDuplicateEngine   = errors.New("registry: duplicate engine name")
)

// Capabilities advertises read-only and future management capabilities exposed
// by a daemon. CR-001 uses ListOwners only; mutating fields are reserved for
// later opt-in CRs.
type Capabilities struct {
	ListOwners bool `json:"list_owners"`
	Stop       bool `json:"stop,omitempty"`
	Restart    bool `json:"restart,omitempty"`
	Update     bool `json:"update,omitempty"`
}

// Config enables daemon advertisement when passed through engine.Config or
// daemon.Config. A nil *Config is the opt-out zero value. Readers discover
// descriptors only in their muxcore base dir, so native products and operator
// tools must share BaseDir (or both use the default os.TempDir()).
type Config struct {
	ProductName    string
	MuxcoreVersion string
	// Capabilities defaults to ListOwners when left empty so an advertised
	// daemon is useful for CR-001 read-only discovery.
	Capabilities Capabilities
}

// Descriptor is a single daemon advertisement. It is intentionally small and
// stable JSON so agent-facing tools can render it without product-specific code.
type Descriptor struct {
	SchemaVersion     int          `json:"schema_version"`
	EngineName        string       `json:"engine_name"`
	ProductName       string       `json:"product_name,omitempty"`
	PID               int          `json:"pid"`
	BaseDir           string       `json:"base_dir,omitempty"`
	DaemonControlPath string       `json:"daemon_control_path"`
	StartedAt         time.Time    `json:"started_at"`
	MuxcoreVersion    string       `json:"muxcore_version,omitempty"`
	Capabilities      Capabilities `json:"capabilities"`
}

// BuildDescriptor builds the runtime descriptor for one daemon process.
func (cfg Config) BuildDescriptor(engineName, baseDir, controlPath string, pid int, startedAt time.Time) Descriptor {
	caps := cfg.Capabilities
	if !caps.ListOwners && !caps.Stop && !caps.Restart && !caps.Update {
		caps.ListOwners = true
	}
	return Descriptor{
		SchemaVersion:     SchemaVersion,
		EngineName:        engineName,
		ProductName:       cfg.ProductName,
		PID:               pid,
		BaseDir:           baseDir,
		DaemonControlPath: controlPath,
		StartedAt:         startedAt.UTC(),
		MuxcoreVersion:    cfg.MuxcoreVersion,
		Capabilities:      caps,
	}
}

// Record is a registry file plus the parsed descriptor when parsing succeeded.
// Invalid records are returned so operator tools can label stale/malformed
// entries instead of silently pretending the registry is clean.
type Record struct {
	Path       string
	Descriptor Descriptor
	Err        error
}

// VerifiedDescriptor is the result of checking a descriptor through the
// daemon's status control RPC.
type VerifiedDescriptor struct {
	Record           Record
	State            string
	Reachable        bool
	Reason           string
	StatusEngineName string
	PID              int
	DaemonGeneration string
	OwnerCount       int
}

type SendFunc func(socketPath string, req control.Request) (*control.Response, error)

// Dir returns the registry directory under the muxcore base dir. Empty baseDir
// follows the same convention as sockets: os.TempDir().
func Dir(baseDir string) string {
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	return filepath.Join(baseDir, registryDirName)
}

// DescriptorPath returns the deterministic file path for a descriptor. The
// engine name is human-readable in the file name; the hash keeps entries unique
// when two descriptors claim the same engine name but point at different
// control sockets.
func DescriptorPath(baseDir string, d Descriptor) (string, error) {
	if d.SchemaVersion == 0 {
		d.SchemaVersion = SchemaVersion
	}
	if err := validateDescriptor(d); err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(d.EngineName + "\x00" + d.DaemonControlPath))
	suffix := hex.EncodeToString(hash[:])[:12]
	return filepath.Join(Dir(baseDir), fmt.Sprintf("%s-%s.json", safeName(d.EngineName), suffix)), nil
}

// WriteDescriptor writes or replaces this daemon's descriptor. The replacement
// is best-effort atomic through a temp file and rename; descriptors are
// advisory and are re-verified by readers.
func WriteDescriptor(baseDir string, d Descriptor) (string, error) {
	if d.SchemaVersion == 0 {
		d.SchemaVersion = SchemaVersion
	}
	if err := validateDescriptor(d); err != nil {
		return "", err
	}
	path, err := DescriptorPath(baseDir, d)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("registry: create dir: %w", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", fmt.Errorf("registry: marshal descriptor: %w", err)
	}
	data = append(data, '\n')

	tmpFile, err := os.CreateTemp(filepath.Dir(path), safeName(d.EngineName)+"-*.tmp")
	if err != nil {
		return "", fmt.Errorf("registry: create temp descriptor: %w", err)
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("registry: write temp descriptor: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("registry: close temp descriptor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Some platforms are stricter about replacing an existing file. This is
		// this daemon's own deterministic descriptor path, not a foreign cleanup.
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", fmt.Errorf("registry: replace descriptor: remove existing: %w (original rename: %v)", removeErr, err)
		}
		if retryErr := os.Rename(tmp, path); retryErr != nil {
			return "", fmt.Errorf("registry: replace descriptor: %w", retryErr)
		}
	}
	return path, nil
}

// RemoveDescriptorIfOwned removes path only when the descriptor currently on
// disk still belongs to the expected daemon process. It intentionally refuses
// to remove descriptors with a different PID so a predecessor cannot erase a
// successor advertisement that reused the same deterministic descriptor path.
func RemoveDescriptorIfOwned(path string, expected Descriptor) (bool, error) {
	current, err := ReadDescriptor(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if current.EngineName != expected.EngineName ||
		current.DaemonControlPath != expected.DaemonControlPath ||
		current.PID == 0 ||
		current.PID != expected.PID {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReadDescriptor reads and validates a descriptor file.
func ReadDescriptor(path string) (Descriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, fmt.Errorf("registry: read descriptor: %w", err)
	}
	var d Descriptor
	if err := json.Unmarshal(data, &d); err != nil {
		return Descriptor{}, fmt.Errorf("%w: parse %s: %v", ErrInvalidDescriptor, path, err)
	}
	if err := validateDescriptor(d); err != nil {
		return Descriptor{}, err
	}
	return d, nil
}

// ListDescriptors reads every registry descriptor file. A missing registry dir
// is an empty registry, not an error.
func ListDescriptors(baseDir string) ([]Record, error) {
	dir := Dir(baseDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("registry: list descriptors: %w", err)
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		d, err := ReadDescriptor(path)
		records = append(records, Record{Path: path, Descriptor: d, Err: err})
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Path < records[j].Path
	})
	return records, nil
}

// DuplicateEngineNames returns exact engine names claimed by more than one
// valid descriptor record.
func DuplicateEngineNames(records []Record) map[string]int {
	counts := make(map[string]int)
	for _, rec := range records {
		if rec.Err != nil {
			continue
		}
		name := rec.Descriptor.EngineName
		if name != "" {
			counts[name]++
		}
	}
	dups := make(map[string]int)
	for name, count := range counts {
		if count > 1 {
			dups[name] = count
		}
	}
	return dups
}

// ResolveEngine returns exactly one valid descriptor for engineName.
func ResolveEngine(records []Record, engineName string) (Record, error) {
	var matches []Record
	for _, rec := range records {
		if rec.Err != nil {
			continue
		}
		if rec.Descriptor.EngineName == engineName {
			matches = append(matches, rec)
		}
	}
	switch len(matches) {
	case 0:
		return Record{}, ErrEngineNotFound
	case 1:
		return matches[0], nil
	default:
		return Record{}, ErrDuplicateEngine
	}
}

// VerifyDescriptor verifies a descriptor with the default control client.
func VerifyDescriptor(rec Record) VerifiedDescriptor {
	return VerifyDescriptorWithSender(rec, control.Send)
}

// VerifyDescriptorWithSender verifies a descriptor by calling status on its
// advertised control path and checking that status.engine_name matches.
func VerifyDescriptorWithSender(rec Record, send SendFunc) VerifiedDescriptor {
	out := VerifiedDescriptor{Record: rec}
	if rec.Err != nil {
		out.State = StateInvalid
		out.Reason = rec.Err.Error()
		return out
	}
	if err := validateDescriptor(rec.Descriptor); err != nil {
		out.State = StateInvalid
		out.Reason = err.Error()
		return out
	}
	resp, err := send(rec.Descriptor.DaemonControlPath, control.Request{Cmd: "status"})
	if err != nil {
		out.State = StateStale
		out.Reason = fmt.Sprintf("control_unreachable: %v", err)
		return out
	}
	if resp == nil {
		out.State = StateStale
		out.Reason = "status_nil_response"
		return out
	}
	if !resp.OK {
		out.State = StateStale
		if resp.Message != "" {
			out.Reason = "status_not_ok: " + resp.Message
		} else {
			out.Reason = "status_not_ok"
		}
		return out
	}
	if len(resp.Data) == 0 {
		out.State = StateStale
		out.Reason = "status_empty_response_data"
		return out
	}
	var status map[string]any
	if err := json.Unmarshal(resp.Data, &status); err != nil {
		out.State = StateStale
		out.Reason = fmt.Sprintf("status_parse_error: %v", err)
		return out
	}
	engineName, _ := status["engine_name"].(string)
	out.StatusEngineName = engineName
	out.PID = intFromStatus(status["pid"])
	out.OwnerCount = intFromStatus(status["owner_count"])
	if generation, _ := status["daemon_generation"].(string); generation != "" {
		out.DaemonGeneration = generation
	}
	if engineName != rec.Descriptor.EngineName {
		out.State = StateStale
		out.Reason = fmt.Sprintf("engine_name_mismatch: descriptor=%q status=%q", rec.Descriptor.EngineName, engineName)
		return out
	}
	if rec.Descriptor.PID != 0 && out.PID != rec.Descriptor.PID {
		out.State = StateStale
		out.Reason = fmt.Sprintf("pid_mismatch: descriptor=%d status=%d", rec.Descriptor.PID, out.PID)
		return out
	}
	out.State = StateHealthy
	out.Reachable = true
	return out
}

func validateDescriptor(d Descriptor) error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrInvalidDescriptor, d.SchemaVersion)
	}
	if strings.TrimSpace(d.EngineName) == "" {
		return fmt.Errorf("%w: engine_name is required", ErrInvalidDescriptor)
	}
	if strings.TrimSpace(d.DaemonControlPath) == "" {
		return fmt.Errorf("%w: daemon_control_path is required", ErrInvalidDescriptor)
	}
	return nil
}

func safeName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "engine"
	}
	return out
}

func intFromStatus(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}
