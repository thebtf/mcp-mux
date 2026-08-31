package era

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSelectOpeningModernAdmissionTable(t *testing.T) {
	const tail = "{\"jsonrpc\":\"2.0\",\"id\":\"after-opening\",\"method\":\"notifications/cancelled\",\"params\":{}}\n"
	const validParams = `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`
	const validClientInfoParams = `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"roots":{}},"io.modelcontextprotocol/clientInfo":{"name":"selector-client","version":"1.2.3"}}}`

	tests := []struct {
		name          string
		opening       string
		valid         bool
		wantKind      AdmissionErrorKind
		wantCode      int
		wantIDs       []string
		wantRequested string
		redacted      string
	}{
		{
			name:    "direct request omits optional client info",
			opening: selectorTestRequest(`"direct-no-client-info"`, "tools/list", validParams),
			valid:   true,
		},
		{
			name:    "direct request includes valid client info",
			opening: selectorTestRequest(`"direct-client-info"`, "tools/call", validClientInfoParams),
			valid:   true,
		},
		{
			name:    "discovery request preserves whitespace and client info",
			opening: ` { "method" : "server/discover", "params" : { "_meta" : { "io.modelcontextprotocol/clientInfo" : { "version" : "1.2.3", "name" : "selector-client" }, "io.modelcontextprotocol/clientCapabilities" : { "roots" : {} }, "io.modelcontextprotocol/protocolVersion" : "2026-07-28" } }, "id" : 42, "jsonrpc" : "2.0" } `,
			valid:   true,
		},
		{
			name:     "missing params",
			opening:  `{"jsonrpc":"2.0","id":"missing-params","method":"tools/list"}`,
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"missing-params"`},
		},
		{
			name:     "null params",
			opening:  selectorTestRequest(`"null-params"`, "tools/list", "null"),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"null-params"`},
		},
		{
			name:     "non object params",
			opening:  selectorTestRequest(`"array-params"`, "tools/list", `[]`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"array-params"`},
		},
		{
			name:     "missing metadata",
			opening:  selectorTestRequest(`"missing-meta"`, "tools/list", `{}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"missing-meta"`},
		},
		{
			name:     "null metadata",
			opening:  selectorTestRequest(`"null-meta"`, "tools/list", `{"_meta":null}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"null-meta"`},
		},
		{
			name:     "non object metadata",
			opening:  selectorTestRequest(`"array-meta"`, "tools/list", `{"_meta":[]}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"array-meta"`},
		},
		{
			name:     "missing protocol version",
			opening:  selectorTestRequest(`"missing-version"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"missing-version"`},
		},
		{
			name:     "null protocol version",
			opening:  selectorTestRequest(`"null-version"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":null,"io.modelcontextprotocol/clientCapabilities":{}}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"null-version"`},
		},
		{
			name:     "non string protocol version",
			opening:  selectorTestRequest(`"numeric-version"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":20260728,"io.modelcontextprotocol/clientCapabilities":{}}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"numeric-version"`},
		},
		{
			name:     "empty protocol version",
			opening:  selectorTestRequest(`"empty-version"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"","io.modelcontextprotocol/clientCapabilities":{}}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"empty-version"`},
		},
		{
			name:     "missing client capabilities",
			opening:  selectorTestRequest(`"missing-capabilities"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"missing-capabilities"`},
		},
		{
			name:     "null client capabilities",
			opening:  selectorTestRequest(`"null-capabilities"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":null}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"null-capabilities"`},
		},
		{
			name:     "non object client capabilities",
			opening:  selectorTestRequest(`"array-capabilities"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":[]}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"array-capabilities"`},
		},
		{
			name:     "null client info",
			opening:  selectorTestRequest(`"null-client-info"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":null}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"null-client-info"`},
		},
		{
			name:     "non object client info",
			opening:  selectorTestRequest(`"array-client-info"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":[]}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"array-client-info"`},
		},
		{
			name:     "client info missing name",
			opening:  selectorTestRequest(`"client-info-missing-name"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"version":"1.2.3"}}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"client-info-missing-name"`},
		},
		{
			name:     "client info empty version is redacted",
			opening:  selectorTestRequest(`"client-info-empty-version"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"selector-secret","version":""}}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"client-info-empty-version"`},
			redacted: "selector-secret",
		},
		{
			name:     "client info non string name",
			opening:  selectorTestRequest(`"client-info-numeric-name"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":1,"version":"1.2.3"}}}`),
			wantKind: AdmissionInvalidModernParams,
			wantCode: -32602,
			wantIDs:  []string{`"client-info-numeric-name"`},
		},
		{
			name:     "syntax error",
			opening:  `{"jsonrpc":"2.0","id":`,
			wantKind: AdmissionMalformedFrame,
			wantCode: -32700,
			wantIDs:  []string{"null"},
		},
		{
			name:     "top level array is not a request object",
			opening:  `[]`,
			wantKind: AdmissionMalformedFrame,
			wantCode: -32600,
			wantIDs:  []string{"null"},
		},
		{
			name:     "response is not an opening request",
			opening:  `{"jsonrpc":"2.0","id":7,"result":{}}`,
			wantKind: AdmissionMalformedFrame,
			wantCode: -32600,
			wantIDs:  []string{"null", "7"},
		},
		{
			name:     "notification is not an opening request",
			opening:  `{"jsonrpc":"2.0","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			wantKind: AdmissionMalformedFrame,
			wantCode: -32600,
			wantIDs:  []string{"null"},
		},
		{
			name:     "null id",
			opening:  selectorTestRequest("null", "tools/list", validParams),
			wantKind: AdmissionMalformedFrame,
			wantCode: -32600,
			wantIDs:  []string{"null"},
		},
		{
			name:     "non string non integer id",
			opening:  selectorTestRequest("false", "tools/list", validParams),
			wantKind: AdmissionMalformedFrame,
			wantCode: -32600,
			wantIDs:  []string{"null"},
		},
		{
			name:     "fractional id is not an integer",
			opening:  selectorTestRequest("1.5", "tools/list", validParams),
			wantKind: AdmissionMalformedFrame,
			wantCode: -32600,
			wantIDs:  []string{"null"},
		},
		{
			name:     "wrong jsonrpc version",
			opening:  `{"jsonrpc":"1.0","id":"wrong-jsonrpc","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			wantKind: AdmissionMalformedFrame,
			wantCode: -32600,
			wantIDs:  []string{"null", `"wrong-jsonrpc"`},
		},
		{
			name:     "empty method",
			opening:  selectorTestRequest(`"empty-method"`, "", validParams),
			wantKind: AdmissionMalformedFrame,
			wantCode: -32600,
			wantIDs:  []string{"null", `"empty-method"`},
		},
		{
			name:          "unsupported otherwise valid version",
			opening:       selectorTestRequest(`"unsupported-version"`, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-03-26","io.modelcontextprotocol/clientCapabilities":{}}}`),
			wantKind:      AdmissionUnsupportedModernVersion,
			wantCode:      -32022,
			wantIDs:       []string{`"unsupported-version"`},
			wantRequested: "2025-03-26",
		},
		{
			name:     "duplicate supported protocol version is ambiguous",
			opening:  `{"jsonrpc":"2.0","id":"duplicate-version","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			wantKind: AdmissionConflictingEraSignals,
			wantIDs:  []string{"null", `"duplicate-version"`},
		},
		{
			name:     "conflicting protocol versions are a local era refusal",
			opening:  `{"jsonrpc":"2.0","id":"conflicting-version","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/protocolVersion":"2025-03-26","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			wantKind: AdmissionConflictingEraSignals,
			wantIDs:  []string{"null", `"conflicting-version"`},
		},
		{
			name:     "legacy initialize with modern metadata",
			opening:  selectorTestRequest(`"legacy-initialize"`, "initialize", validParams),
			wantKind: AdmissionConflictingEraSignals,
			wantIDs:  []string{"null", `"legacy-initialize"`},
		},
		{
			name:     "legacy initialized signal with modern metadata",
			opening:  selectorTestRequest(`"legacy-initialized"`, "notifications/initialized", validParams),
			wantKind: AdmissionConflictingEraSignals,
			wantIDs:  []string{"null", `"legacy-initialized"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selection, err := SelectOpening(PolicyModern20260728, bytes.NewReader([]byte(tt.opening+"\n"+tail)))
			if tt.valid {
				if err != nil {
					t.Fatalf("SelectOpening() error = %v", err)
				}
				assertSelectorOpening(t, selection, []byte(tt.opening+"\n"), []byte(tail))
				return
			}

			if err == nil {
				t.Fatal("SelectOpening() error = nil, want local admission refusal")
			}
			if selection != nil {
				t.Fatalf("SelectOpening() selection = %#v, want nil after refusal", selection)
			}
			assertSelectorAdmissionError(t, err, tt.wantKind, tt.wantCode, tt.wantIDs, tt.wantRequested, tt.redacted)
		})
	}
}

func TestSelectOpeningLegacyPolicyLeavesInputUnread(t *testing.T) {
	input := &selectorUnreadReader{}

	selection, err := SelectOpening(PolicyLegacyOnly, input)
	if err != nil {
		t.Fatalf("SelectOpening() error = %v", err)
	}
	if input.reads != 0 {
		t.Fatalf("legacy input reads = %d, want 0", input.reads)
	}
	if selection == nil {
		t.Fatal("SelectOpening() selection = nil, want legacy selection")
	}
	if selection.Era != EraLegacy {
		t.Fatalf("legacy selection era = %v, want %v", selection.Era, EraLegacy)
	}
	if selection.Frame != nil {
		t.Fatalf("legacy selection frame = %#v, want nil", selection.Frame)
	}
	if selection.Remainder != input {
		t.Fatal("legacy selection did not return the untouched input reader")
	}
}

func selectorTestRequest(id, method, params string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":` + params + `}`
}

func assertSelectorOpening(t *testing.T, selection *OpeningSelection, wantOpening, wantTail []byte) {
	t.Helper()

	if selection == nil {
		t.Fatal("SelectOpening() selection = nil, want modern selection")
	}
	if selection.Era != EraModern20260728 {
		t.Fatalf("selection era = %v, want %v", selection.Era, EraModern20260728)
	}
	if selection.Frame == nil {
		t.Fatal("selection frame = nil, want retained opening frame")
	}

	gotOpening, available := selection.Frame.Take()
	if !available {
		t.Fatal("selection frame is unavailable, want exact opening bytes")
	}
	if !bytes.Equal(gotOpening, wantOpening) {
		t.Fatalf("selection opening bytes = %q, want %q", gotOpening, wantOpening)
	}
	if again, available := selection.Frame.Take(); available || len(again) != 0 {
		t.Fatalf("second selection frame Take() = (%q, %v), want (empty, false)", again, available)
	}
	if selection.Remainder == nil {
		t.Fatal("selection remainder = nil, want exact tail reader")
	}

	gotTail, err := io.ReadAll(selection.Remainder)
	if err != nil {
		t.Fatalf("read selection remainder: %v", err)
	}
	if !bytes.Equal(gotTail, wantTail) {
		t.Fatalf("selection remainder = %q, want %q", gotTail, wantTail)
	}
}

func assertSelectorAdmissionError(t *testing.T, err error, wantKind AdmissionErrorKind, wantCode int, wantIDs []string, wantRequested, redacted string) {
	t.Helper()

	var admission *AdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("SelectOpening() error = %T %v, want wrapped *AdmissionError", err, err)
	}
	if got := admission.Kind(); got != wantKind {
		t.Fatalf("AdmissionError.Kind() = %v, want %v", got, wantKind)
	}
	if !errors.Is(err, wantKind) {
		t.Fatalf("errors.Is(%v, %v) = false, want true", err, wantKind)
	}
	if redacted != "" && strings.Contains(err.Error(), redacted) {
		t.Fatalf("AdmissionError.Error() leaked raw opening content %q: %q", redacted, err.Error())
	}

	responseBytes := admission.JSONRPCResponse()
	if len(responseBytes) == 0 {
		t.Fatal("AdmissionError.JSONRPCResponse() = empty, want JSON-RPC error response")
	}

	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseBytes, &response); err != nil {
		t.Fatalf("AdmissionError.JSONRPCResponse() = %q, want valid JSON-RPC: %v", responseBytes, err)
	}
	if response.JSONRPC != "2.0" {
		t.Fatalf("error response jsonrpc = %q, want 2.0", response.JSONRPC)
	}
	if response.Error.Message == "" {
		t.Fatal("error response message = empty, want a JSON-RPC error message")
	}
	if !selectorAllowsID(response.ID, wantIDs) {
		t.Fatalf("error response id = %s, want one of %v", response.ID, wantIDs)
	}
	if wantCode != 0 {
		if response.Error.Code != wantCode {
			t.Fatalf("error response code = %d, want %d", response.Error.Code, wantCode)
		}
	} else if response.Error.Code == -32022 {
		t.Fatal("local era refusal used -32022, which is reserved for a valid unsupported version")
	}

	if wantRequested == "" {
		return
	}

	var data struct {
		Supported []string `json:"supported"`
		Requested string   `json:"requested"`
	}
	if err := json.Unmarshal(response.Error.Data, &data); err != nil {
		t.Fatalf("unsupported-version response data = %q, want supported/requested object: %v", response.Error.Data, err)
	}
	if len(data.Supported) != 1 || data.Supported[0] != "2026-07-28" {
		t.Fatalf("unsupported-version response supported = %v, want [2026-07-28]", data.Supported)
	}
	if data.Requested != wantRequested {
		t.Fatalf("unsupported-version response requested = %q, want %q", data.Requested, wantRequested)
	}
}

func selectorAllowsID(got json.RawMessage, wantIDs []string) bool {
	for _, want := range wantIDs {
		if string(got) == want {
			return true
		}
	}
	return false
}

type selectorUnreadReader struct {
	reads int
}

func (r *selectorUnreadReader) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("legacy selector input was read")
}
