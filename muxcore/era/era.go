// Package era defines the protocol-era vocabulary used before owner election.
package era

import (
	"bytes"
	"encoding/json"
	"errors"
)

const modern20260728Wire = "2026-07-28"

var errUnsupportedProtocolEra = errors.New("protocol era: unsupported")

// ProtocolEra identifies the MCP protocol behavior selected before owner election.
type ProtocolEra uint8

const (
	// EraLegacy preserves the released initialization-based behavior.
	EraLegacy ProtocolEra = iota
	// EraModern20260728 selects native MCP 2026-07-28 behavior.
	EraModern20260728
)

// ParseProtocolEra accepts only the legacy-compatible empty wire value or the
// exact supported modern wire value.
func ParseProtocolEra(wire string) (ProtocolEra, error) {
	switch wire {
	case "":
		return EraLegacy, nil
	case modern20260728Wire:
		return EraModern20260728, nil
	default:
		return ProtocolEra(^uint8(0)), errUnsupportedProtocolEra
	}
}

// Wire returns the exact control-wire representation for era.
func (era ProtocolEra) Wire() (string, error) {
	switch era {
	case EraLegacy:
		return "", nil
	case EraModern20260728:
		return modern20260728Wire, nil
	default:
		return "", errUnsupportedProtocolEra
	}
}

// ProtocolPolicy describes the caller's allowed protocol-era behavior.
type ProtocolPolicy uint8

const (
	// PolicyLegacyOnly preserves the released legacy path.
	PolicyLegacyOnly ProtocolPolicy = iota
	// PolicyModern20260728 requires the pinned native modern path.
	PolicyModern20260728
)

// OpeningFrame owns one raw opening frame until it is transferred upstream.
type OpeningFrame struct {
	raw       []byte
	available bool
}

// NewOpeningFrame defensively copies raw so callers retain no ownership of the
// buffered opening frame.
func NewOpeningFrame(raw []byte) *OpeningFrame {
	return &OpeningFrame{raw: bytes.Clone(raw), available: true}
}

// Take transfers the owned frame without copying. It succeeds only once.
func (frame *OpeningFrame) Take() ([]byte, bool) {
	if frame == nil || !frame.available {
		return nil, false
	}

	frame.available = false
	raw := frame.raw
	frame.raw = nil
	return raw, true
}

// AdmissionErrorKind categorizes a safe local admission refusal.
type AdmissionErrorKind uint8

const (
	AdmissionMalformedFrame AdmissionErrorKind = iota
	AdmissionInvalidModernParams
	AdmissionUnsupportedModernVersion
	AdmissionConflictingEraSignals
	AdmissionControlEraMismatch
	AdmissionUnsafeLifecycleBoundary
	AdmissionContainedUpstreamRequest
)

// String returns the stable redacted category name.
func (kind AdmissionErrorKind) String() string {
	switch kind {
	case AdmissionMalformedFrame:
		return "malformed frame"
	case AdmissionInvalidModernParams:
		return "invalid modern parameters"
	case AdmissionUnsupportedModernVersion:
		return "unsupported modern version"
	case AdmissionConflictingEraSignals:
		return "conflicting era signals"
	case AdmissionControlEraMismatch:
		return "control era mismatch"
	case AdmissionUnsafeLifecycleBoundary:
		return "unsafe lifecycle boundary"
	case AdmissionContainedUpstreamRequest:
		return "contained upstream request"
	default:
		return "unknown"
	}
}

// Error makes an admission category usable as an errors.Is target.
func (kind AdmissionErrorKind) Error() string {
	return "admission: " + kind.String()
}

// AdmissionError exposes only a redacted error category while retaining the
// safe JSON-RPC response fields needed at the ingress boundary.
type AdmissionError struct {
	kind      AdmissionErrorKind
	id        json.RawMessage
	code      int
	requested string
}

// NewAdmissionError constructs a category-only admission error.
func NewAdmissionError(kind AdmissionErrorKind) *AdmissionError {
	return newAdmissionError(kind, admissionErrorCode(kind), nil, "")
}

func newAdmissionError(kind AdmissionErrorKind, code int, id json.RawMessage, requested string) *AdmissionError {
	return &AdmissionError{
		kind:      kind,
		id:        id,
		code:      code,
		requested: requested,
	}
}

func admissionErrorCode(kind AdmissionErrorKind) int {
	switch kind {
	case AdmissionMalformedFrame:
		return -32600
	case AdmissionInvalidModernParams:
		return -32602
	case AdmissionUnsupportedModernVersion:
		return -32022
	default:
		return -32000
	}
}

// Kind returns the redacted admission-error category.
func (err *AdmissionError) Kind() AdmissionErrorKind {
	if err == nil {
		return AdmissionMalformedFrame
	}
	return err.kind
}

// Error returns only the redacted category.
func (err *AdmissionError) Error() string {
	if err == nil {
		return ""
	}
	return err.kind.Error()
}

// JSONRPCResponse returns the local JSON-RPC error response without a framing
// newline. It never includes the opening payload or client metadata.
func (err *AdmissionError) JSONRPCResponse() []byte {
	if err == nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`)
	}

	code := err.code
	if code == 0 {
		code = admissionErrorCode(err.kind)
	}
	message := "Admission refused"
	switch code {
	case -32700:
		message = "Parse error"
	case -32600:
		message = "Invalid Request"
	case -32602:
		message = "Invalid params"
	case -32022:
		message = "Unsupported protocol version"
	}

	response := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data,omitempty"`
		} `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      err.id,
	}
	response.Error.Code = code
	response.Error.Message = message
	if err.kind == AdmissionUnsupportedModernVersion {
		response.Error.Data = struct {
			Supported []string `json:"supported"`
			Requested string   `json:"requested"`
		}{
			Supported: []string{modern20260728Wire},
			Requested: err.requested,
		}
	}

	payload, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`)
	}
	return payload
}

// Is compares admission errors by their redacted category.
func (err *AdmissionError) Is(target error) bool {
	if err == nil {
		return false
	}

	switch target := target.(type) {
	case AdmissionErrorKind:
		return err.kind == target
	case *AdmissionError:
		return target != nil && err.kind == target.kind
	default:
		return false
	}
}
