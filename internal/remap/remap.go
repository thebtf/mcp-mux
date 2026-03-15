// Package remap provides JSON-RPC id remapping for session multiplexing.
//
// When multiple downstream clients share a single upstream, their JSON-RPC
// request ids can collide (e.g., both send id:1). The remapper prefixes each
// id with a session identifier to make it unique, and strips the prefix on
// the way back.
//
// Format: "s{sessionID}:{originalID}"
// Examples:
//
//	id: 1         → "s3:1"     (number)
//	id: "abc"     → "s3:abc"   (string — quotes stripped in prefix)
//	id: null      → "s3:null"  (null preserved)
package remap

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Remap takes a session ID and the original JSON-RPC id value (as raw JSON),
// and returns a new id as a JSON string value with session prefix.
//
// The original id type is encoded in the prefix for correct restoration:
//
//	number → "s{sess}:n:{value}"
//	string → "s{sess}:s:{value}"
//	null   → "s{sess}:null"
func Remap(sessionID int, originalID json.RawMessage) json.RawMessage {
	if originalID == nil || string(originalID) == "null" {
		s := fmt.Sprintf(`"s%d:null"`, sessionID)
		return json.RawMessage(s)
	}

	raw := strings.TrimSpace(string(originalID))

	// Determine type and encode accordingly
	if len(raw) > 0 && raw[0] == '"' {
		// String id — unquote, prefix with s:
		var str string
		if err := json.Unmarshal(originalID, &str); err != nil {
			// Fallback: use raw value
			str = raw
		}
		s := fmt.Sprintf(`"s%d:s:%s"`, sessionID, str)
		return json.RawMessage(s)
	}

	// Number id — prefix with n:
	s := fmt.Sprintf(`"s%d:n:%s"`, sessionID, raw)
	return json.RawMessage(s)
}

// DeremapResult holds the result of parsing a remapped id.
type DeremapResult struct {
	SessionID  int
	OriginalID json.RawMessage
}

// Deremap parses a remapped id string and returns the session ID and original
// id value restored to its original JSON type.
func Deremap(remappedID json.RawMessage) (*DeremapResult, error) {
	// The remapped id is always a JSON string
	var s string
	if err := json.Unmarshal(remappedID, &s); err != nil {
		return nil, fmt.Errorf("deremap: expected string id, got %s: %w", string(remappedID), err)
	}

	// Format: "s{sessionID}:{type}:{value}" or "s{sessionID}:null"
	if !strings.HasPrefix(s, "s") {
		return nil, fmt.Errorf("deremap: missing session prefix in %q", s)
	}

	// Find first colon after 's'
	colonIdx := strings.Index(s[1:], ":")
	if colonIdx < 0 {
		return nil, fmt.Errorf("deremap: malformed remapped id %q", s)
	}
	colonIdx++ // adjust for s[1:] offset

	sessionStr := s[1:colonIdx]
	sessionID, err := strconv.Atoi(sessionStr)
	if err != nil {
		return nil, fmt.Errorf("deremap: invalid session id %q: %w", sessionStr, err)
	}

	rest := s[colonIdx+1:] // after "s{N}:"

	// Handle null
	if rest == "null" {
		return &DeremapResult{
			SessionID:  sessionID,
			OriginalID: json.RawMessage("null"),
		}, nil
	}

	// Handle typed values: "n:{number}" or "s:{string}"
	if len(rest) < 2 || rest[1] != ':' {
		return nil, fmt.Errorf("deremap: malformed type prefix in %q", s)
	}

	typeChar := rest[0]
	value := rest[2:]

	switch typeChar {
	case 'n':
		// Number — return as raw number
		return &DeremapResult{
			SessionID:  sessionID,
			OriginalID: json.RawMessage(value),
		}, nil
	case 's':
		// String — re-quote
		quoted, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("deremap: failed to re-quote string %q: %w", value, err)
		}
		return &DeremapResult{
			SessionID:  sessionID,
			OriginalID: json.RawMessage(quoted),
		}, nil
	default:
		return nil, fmt.Errorf("deremap: unknown type prefix %q in %q", typeChar, s)
	}
}

// IsRemapped checks if a JSON-RPC id looks like a remapped id (starts with "s" prefix).
func IsRemapped(id json.RawMessage) bool {
	var s string
	if err := json.Unmarshal(id, &s); err != nil {
		return false
	}
	return strings.HasPrefix(s, "s") && strings.Contains(s, ":")
}
