package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thebtf/mcp-mux/muxcore/control"
	"github.com/thebtf/mcp-mux/muxcore/registry"
)

var errProcessSnapshotUnsupported = errors.New("process snapshot unsupported on this platform")

type processSnapshotFunc func() ([]runtimeProcess, error)

type runtimeProcess struct {
	PID           int    `json:"pid"`
	ParentPID     int    `json:"parent_pid,omitempty"`
	Name          string `json:"name"`
	ExePath       string `json:"exe_path,omitempty"`
	Role          string `json:"role"`
	VersionDir    string `json:"version_dir,omitempty"`
	ActiveVersion bool   `json:"active_version,omitempty"`
}

func (s *Server) toolMuxTopology(id json.RawMessage, args json.RawMessage) {
	var params struct {
		IncludeProcesses *bool `json:"include_processes"`
	}
	if args != nil {
		_ = json.Unmarshal(args, &params)
	}
	includeProcesses := true
	if params.IncludeProcesses != nil {
		includeProcesses = *params.IncludeProcesses
	}

	owners, ownerNote := s.collectLocalOwners()
	ownerSummary := summarizeOwners(owners)
	engineRows, descriptorCandidates, registrySummary := s.collectRegistryTopology()

	processes := []runtimeProcess{}
	processSummary := map[string]any{"available": false}
	var processNote string
	if includeProcesses {
		var err error
		processes, err = s.collectProcessSnapshot()
		if err != nil {
			processNote = err.Error()
		}
		processSummary = summarizeProcesses(processes, err)
	}

	cleanupPlan, warnings := buildCleanupPlan(owners, descriptorCandidates, processes)
	if ownerNote != "" {
		warnings = append(warnings, ownerNote)
	}
	if processNote != "" {
		warnings = append(warnings, fmt.Sprintf("process_snapshot: %s", processNote))
	}

	s.sendJSONToolResult(id, map[string]any{
		"schema_version": 1,
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"scope": map[string]any{
			"default_engine":      s.engineName(),
			"base_dir":            s.socketDir(),
			"read_only":           true,
			"cleanup_plan_action": "none; this tool reports candidates only",
		},
		"mcp_mux": map[string]any{
			"owners":  owners,
			"summary": ownerSummary,
			"note":    ownerNote,
		},
		"registry": map[string]any{
			"engines": engineRows,
			"summary": registrySummary,
		},
		"processes": map[string]any{
			"included": includeProcesses,
			"items":    processes,
			"summary":  processSummary,
			"note":     processNote,
		},
		"cleanup_plan": cleanupPlan,
		"warnings":     warnings,
	})
}

func (s *Server) collectLocalOwners() ([]control.OwnerInfo, string) {
	resp, err := control.Send(s.daemonCtlPath(), control.Request{Cmd: "list_owners"})
	if err != nil {
		return []control.OwnerInfo{}, fmt.Sprintf("local mcp-mux daemon not reachable: %v", err)
	}
	if !resp.OK {
		return []control.OwnerInfo{}, fmt.Sprintf("local mcp-mux daemon returned error: %s", resp.Message)
	}
	if resp.Data == nil {
		return []control.OwnerInfo{}, "local mcp-mux daemon returned empty list_owners response"
	}
	var listResp control.ListOwnersResponse
	if err := json.Unmarshal(resp.Data, &listResp); err != nil {
		return []control.OwnerInfo{}, fmt.Sprintf("local mcp-mux daemon returned invalid list_owners response: %v", err)
	}
	return listResp.Owners, ""
}

func (s *Server) collectRegistryTopology() ([]map[string]any, []map[string]any, map[string]any) {
	records, err := registry.ListDescriptors(s.socketDir())
	if err != nil {
		return []map[string]any{}, []map[string]any{}, map[string]any{
			"descriptor_count": 0,
			"error":            err.Error(),
		}
	}

	verifiedRecords := make([]registry.VerifiedDescriptor, 0, len(records))
	for _, rec := range records {
		verifiedRecords = append(verifiedRecords, registry.VerifyDescriptor(rec))
	}
	duplicates := registry.DuplicateHealthyEngineNames(verifiedRecords)

	rows := make([]map[string]any, 0, len(verifiedRecords))
	candidates := make([]map[string]any, 0)
	byState := map[string]int{}
	for _, verified := range verifiedRecords {
		rec := verified.Record
		state := verified.State
		reason := verified.Reason
		if rec.Err == nil && state == registry.StateHealthy {
			if count, ok := duplicates[rec.Descriptor.EngineName]; ok {
				state = registry.StateDuplicate
				if reason == "" {
					reason = fmt.Sprintf("duplicate_engine_name: %d descriptors", count)
				}
			}
		}
		byState[state]++

		row := map[string]any{
			"descriptor_path": rec.Path,
			"state":           state,
			"reachable":       verified.Reachable,
		}
		if reason != "" {
			row["reason"] = reason
		}
		if rec.Err == nil {
			row["engine_name"] = rec.Descriptor.EngineName
			row["product_name"] = rec.Descriptor.ProductName
			row["pid"] = rec.Descriptor.PID
			if verified.PID != 0 {
				row["status_pid"] = verified.PID
			}
			row["base_dir"] = rec.Descriptor.BaseDir
			row["daemon_control_path"] = rec.Descriptor.DaemonControlPath
			row["started_at"] = rec.Descriptor.StartedAt.Format(time.RFC3339)
			row["muxcore_version"] = rec.Descriptor.MuxcoreVersion
			row["capabilities"] = rec.Descriptor.Capabilities
			row["owner_count"] = verified.OwnerCount
			if verified.DaemonGeneration != "" {
				row["daemon_generation"] = verified.DaemonGeneration
			}
		}
		rows = append(rows, row)
		if state != registry.StateHealthy {
			candidates = append(candidates, row)
		}
	}

	return rows, candidates, map[string]any{
		"descriptor_count": len(verifiedRecords),
		"by_state":         byState,
		"duplicates":       duplicates,
	}
}

func (s *Server) collectProcessSnapshot() ([]runtimeProcess, error) {
	if s.ProcessSnapshot != nil {
		return s.ProcessSnapshot()
	}
	return platformProcessSnapshot()
}

func summarizeOwners(owners []control.OwnerInfo) map[string]any {
	byClassification := map[string]int{}
	byMuxVersion := map[string]int{}
	zeroSession := 0
	totalSessions := 0
	for _, owner := range owners {
		classification := owner.Classification
		if classification == "" {
			classification = "unknown"
		}
		byClassification[classification]++
		version := owner.MuxVersion
		if version == "" {
			version = "unknown"
		}
		byMuxVersion[version]++
		if owner.Sessions == 0 {
			zeroSession++
		}
		totalSessions += owner.Sessions
	}
	return map[string]any{
		"total":             len(owners),
		"zero_session":      zeroSession,
		"total_sessions":    totalSessions,
		"by_classification": byClassification,
		"by_mux_version":    byMuxVersion,
	}
}

func summarizeProcesses(processes []runtimeProcess, err error) map[string]any {
	summary := map[string]any{
		"available": err == nil,
		"total":     len(processes),
	}
	if err != nil {
		summary["error"] = err.Error()
		return summary
	}
	byRole := map[string]int{}
	engineVersions := map[string]int{}
	oldEngineProcessCount := 0
	for _, proc := range processes {
		byRole[proc.Role]++
		if proc.Role == "engine" {
			version := proc.VersionDir
			if version == "" {
				version = "unknown"
			}
			engineVersions[version]++
			if !proc.ActiveVersion {
				oldEngineProcessCount++
			}
		}
	}
	summary["by_role"] = byRole
	summary["engine_versions"] = engineVersions
	summary["old_engine_process_count"] = oldEngineProcessCount
	return summary
}

func buildCleanupPlan(owners []control.OwnerInfo, descriptorCandidates []map[string]any, processes []runtimeProcess) ([]map[string]any, []string) {
	plan := make([]map[string]any, 0)
	warnings := make([]string, 0)

	if len(descriptorCandidates) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d stale/invalid native registry descriptors are prune candidates", len(descriptorCandidates)))
		for _, candidate := range descriptorCandidates {
			row := map[string]any{
				"kind":        "stale_registry_descriptor",
				"destructive": false,
				"action":      "call mux_prune_engines with dry_run=false only after reviewing candidates",
			}
			copyMapKeys(row, candidate, "engine_name", "product_name", "descriptor_path", "state", "reason", "pid", "started_at")
			plan = append(plan, row)
		}
	}

	isolated := 0
	shared := 0
	for _, owner := range owners {
		switch owner.Classification {
		case "isolated":
			isolated++
		case "shared":
			shared++
		}
		if owner.Sessions == 0 && owner.Pending == 0 && !owner.Persistent {
			plan = append(plan, map[string]any{
				"kind":        "zero_session_owner",
				"destructive": false,
				"action":      "candidate for mux_stop after confirming it stayed idle across a second snapshot",
				"server_id":   owner.ServerID,
				"engine_name": owner.EngineName,
				"command":     owner.Command,
				"args":        owner.Args,
				"class":       owner.Classification,
				"mux_version": owner.MuxVersion,
			})
		}
	}
	if isolated > shared && isolated > 0 {
		warnings = append(warnings, fmt.Sprintf("isolated owners dominate this snapshot: isolated=%d shared=%d", isolated, shared))
	}

	oldEngineProcesses := 0
	for _, proc := range processes {
		if proc.Role != "engine" || proc.ActiveVersion {
			continue
		}
		oldEngineProcesses++
		plan = append(plan, map[string]any{
			"kind":         "old_version_engine_process",
			"destructive":  false,
			"action":       "operator review required; do not kill from mux_topology",
			"pid":          proc.PID,
			"parent_pid":   proc.ParentPID,
			"exe_path":     proc.ExePath,
			"version_dir":  proc.VersionDir,
			"process_name": proc.Name,
		})
	}
	if oldEngineProcesses > 0 {
		warnings = append(warnings, fmt.Sprintf("%d mcp-mux engine processes are not running the active version", oldEngineProcesses))
	}

	if plan == nil {
		return []map[string]any{}, warnings
	}
	return plan, warnings
}

func copyMapKeys(dst map[string]any, src map[string]any, keys ...string) {
	for _, key := range keys {
		if value, ok := src[key]; ok {
			dst[key] = value
		}
	}
}

func newRuntimeProcess(pid, parentPID int, name, exePath string) (runtimeProcess, bool) {
	role, versionDir, ok := classifyMuxProcess(name, exePath)
	if !ok {
		return runtimeProcess{}, false
	}
	activeVersion := false
	if role == "engine" {
		activeVersion = activeEngineVersionDir() != "" && versionDir == activeEngineVersionDir()
	}
	return runtimeProcess{
		PID:           pid,
		ParentPID:     parentPID,
		Name:          name,
		ExePath:       exePath,
		Role:          role,
		VersionDir:    versionDir,
		ActiveVersion: activeVersion,
	}, true
}

func classifyMuxProcess(name, exePath string) (role, versionDir string, ok bool) {
	baseName := strings.ToLower(filepath.Base(name))
	basePath := strings.ToLower(filepath.Base(exePath))
	if baseName == "mcp-mux.exe" || baseName == "mcp-mux" || basePath == "mcp-mux.exe" || basePath == "mcp-mux" {
		return "launcher", "", true
	}
	if baseName == "mcp-mux-engine.exe" || baseName == "mcp-mux-engine" || basePath == "mcp-mux-engine.exe" || basePath == "mcp-mux-engine" {
		return "engine", versionDirFromPath(exePath), true
	}
	return "", "", false
}

func versionDirFromPath(path string) string {
	if path == "" {
		return ""
	}
	parts := splitCleanPath(path)
	for i := 0; i+2 < len(parts); i++ {
		if strings.EqualFold(parts[i], "mcp-mux.versions") {
			return parts[i+1]
		}
	}
	return ""
}

func activeEngineVersionDir() string {
	activeFile := os.Getenv("MCPMUX_ACTIVE_ENGINE_FILE")
	if activeFile == "" {
		exe, err := os.Executable()
		if err != nil {
			return ""
		}
		versionDir := filepath.Dir(filepath.Dir(exe))
		if strings.EqualFold(filepath.Base(versionDir), "mcp-mux.versions") {
			activeFile = filepath.Join(versionDir, "active.txt")
		}
	}
	if activeFile == "" {
		if cwd, err := os.Getwd(); err == nil {
			candidate := filepath.Join(cwd, "mcp-mux.versions", "active.txt")
			if _, statErr := os.Stat(candidate); statErr == nil {
				activeFile = candidate
			}
		}
	}
	if activeFile == "" {
		return ""
	}
	data, err := os.ReadFile(activeFile)
	if err != nil {
		return ""
	}
	target := strings.TrimSpace(string(data))
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(activeFile), target)
	}
	return versionDirFromPath(target)
}

func splitCleanPath(path string) []string {
	cleaned := filepath.Clean(path)
	return strings.FieldsFunc(cleaned, func(r rune) bool {
		return r == '/' || r == '\\'
	})
}
