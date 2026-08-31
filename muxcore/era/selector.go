package era

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var errDuplicateOpeningKey = errors.New("duplicate modern opening key")

// OpeningSelection is the pre-election result for one client ingress stream.
type OpeningSelection struct {
	Era       ProtocolEra
	Frame     *OpeningFrame
	Remainder io.Reader

	requestID json.RawMessage
}

// AdmissionError returns a redacted request-scoped refusal for a successfully
// selected opening. It retains only the valid request ID needed on the wire.
func (selection *OpeningSelection) AdmissionError(kind AdmissionErrorKind) *AdmissionError {
	if selection == nil {
		return NewAdmissionError(kind)
	}
	return newAdmissionError(kind, admissionErrorCode(kind), selection.requestID, "")
}

// SelectOpening chooses the ingress route before owner election. Legacy
// selection leaves input untouched; modern selection validates one exact
// newline-delimited request and retains its raw frame for a single forward.
func SelectOpening(policy ProtocolPolicy, input io.Reader) (*OpeningSelection, error) {
	switch policy {
	case PolicyLegacyOnly:
		return &OpeningSelection{Era: EraLegacy, Remainder: input}, nil
	case PolicyModern20260728:
		frame, remainder, err := ReadOpeningFrame(input)
		if err != nil || frame == nil {
			return nil, newAdmissionError(AdmissionMalformedFrame, -32700, nil, "")
		}

		requestID, admission := validateModernOpening(frame.raw)
		if admission != nil {
			return nil, admission
		}
		return &OpeningSelection{
			Era:       EraModern20260728,
			Frame:     frame,
			Remainder: remainder,
			requestID: requestID,
		}, nil
	default:
		return nil, fmt.Errorf("protocol selector: unsupported policy %d", policy)
	}
}

func validateModernOpening(raw []byte) (json.RawMessage, *AdmissionError) {
	if err := validateUniqueJSONDocument(raw); err != nil {
		if errors.Is(err, errDuplicateOpeningKey) {
			return nil, newAdmissionError(AdmissionConflictingEraSignals, admissionErrorCode(AdmissionConflictingEraSignals), nil, "")
		}
		return nil, newAdmissionError(AdmissionMalformedFrame, -32700, nil, "")
	}

	fields, ok := openingJSONObject(raw)
	if !ok {
		return nil, newAdmissionError(AdmissionMalformedFrame, -32600, nil, "")
	}
	requestID, ok := openingRequestID(fields["id"])
	if !ok {
		return nil, newAdmissionError(AdmissionMalformedFrame, -32600, nil, "")
	}
	jsonrpc, ok := openingJSONString(fields["jsonrpc"])
	if !ok || jsonrpc != "2.0" {
		return nil, newAdmissionError(AdmissionMalformedFrame, -32600, nil, "")
	}
	method, ok := openingJSONString(fields["method"])
	if !ok || method == "" {
		return nil, newAdmissionError(AdmissionMalformedFrame, -32600, nil, "")
	}
	if _, response := fields["result"]; response {
		return nil, newAdmissionError(AdmissionMalformedFrame, -32600, nil, "")
	}
	if _, response := fields["error"]; response {
		return nil, newAdmissionError(AdmissionMalformedFrame, -32600, nil, "")
	}
	if method == "initialize" || method == "notifications/initialized" {
		return nil, newAdmissionError(AdmissionConflictingEraSignals, admissionErrorCode(AdmissionConflictingEraSignals), requestID, "")
	}

	params, ok := openingJSONObject(fields["params"])
	if !ok {
		return nil, newAdmissionError(AdmissionInvalidModernParams, admissionErrorCode(AdmissionInvalidModernParams), requestID, "")
	}
	metadata, ok := openingJSONObject(params["_meta"])
	if !ok {
		return nil, newAdmissionError(AdmissionInvalidModernParams, admissionErrorCode(AdmissionInvalidModernParams), requestID, "")
	}
	version, ok := openingJSONString(metadata["io.modelcontextprotocol/protocolVersion"])
	if !ok || version == "" {
		return nil, newAdmissionError(AdmissionInvalidModernParams, admissionErrorCode(AdmissionInvalidModernParams), requestID, "")
	}
	if _, ok := openingJSONObject(metadata["io.modelcontextprotocol/clientCapabilities"]); !ok {
		return nil, newAdmissionError(AdmissionInvalidModernParams, admissionErrorCode(AdmissionInvalidModernParams), requestID, "")
	}
	if clientInfoRaw, present := metadata["io.modelcontextprotocol/clientInfo"]; present {
		clientInfo, ok := openingJSONObject(clientInfoRaw)
		if !ok {
			return nil, newAdmissionError(AdmissionInvalidModernParams, admissionErrorCode(AdmissionInvalidModernParams), requestID, "")
		}
		name, nameOK := openingJSONString(clientInfo["name"])
		version, versionOK := openingJSONString(clientInfo["version"])
		if !nameOK || !versionOK || strings.TrimSpace(name) == "" || strings.TrimSpace(version) == "" {
			return nil, newAdmissionError(AdmissionInvalidModernParams, admissionErrorCode(AdmissionInvalidModernParams), requestID, "")
		}
	}
	if version != modern20260728Wire {
		return nil, newAdmissionError(AdmissionUnsupportedModernVersion, admissionErrorCode(AdmissionUnsupportedModernVersion), requestID, version)
	}
	return requestID, nil
}

func openingJSONObject(raw []byte) (map[string]json.RawMessage, bool) {
	trimmed := trimOpeningJSONWhitespace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func openingJSONString(raw json.RawMessage) (string, bool) {
	trimmed := trimOpeningJSONWhitespace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false
	}

	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false
	}
	return value, true
}

func openingRequestID(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := trimOpeningJSONWhitespace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return nil, false
	}
	switch value := value.(type) {
	case string:
		return raw, true
	case json.Number:
		if isOpeningIntegerNumber(value.String()) {
			return raw, true
		}
	}
	return nil, false
}

func isOpeningIntegerNumber(value string) bool {
	if value == "" {
		return false
	}
	start := 0
	if value[0] == '-' {
		start = 1
	}
	if start == len(value) {
		return false
	}
	for index := start; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func validateUniqueJSONDocument(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeOpeningJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("trailing JSON token")
	}
	return nil
}

func consumeOpeningJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := keys[key]; duplicate {
				return errDuplicateOpeningKey
			}
			keys[key] = struct{}{}
			if err := consumeOpeningJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("invalid JSON object close")
		}
	case '[':
		for decoder.More() {
			if err := consumeOpeningJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("invalid JSON array close")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func trimOpeningJSONWhitespace(raw []byte) []byte {
	start := 0
	for start < len(raw) && isOpeningJSONWhitespace(raw[start]) {
		start++
	}
	end := len(raw)
	for end > start && isOpeningJSONWhitespace(raw[end-1]) {
		end--
	}
	return raw[start:end]
}

func isOpeningJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}
