package supervisor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
)

// ErrFrameTooLarge reports a line that exceeded the configured frame limit.
var ErrFrameTooLarge = errors.New("supervisor: frame too large")

type frameKind uint8

const (
	frameRequest frameKind = iota + 1
	frameNotification
	frameResult
	frameError
)

type cancellationMeta struct {
	requestID scalarValue
}

type progressMeta struct {
	token    scalarValue
	progress string
}

type taskMeta struct {
	id     string
	status string
}

type parsedFrame struct {
	raw       []byte
	kind      frameKind
	id        *scalarValue
	method    string
	params    json.RawMessage
	result    json.RawMessage
	errorData json.RawMessage
	reserved  bool

	progressToken *scalarValue
	taskAugmented bool
	cancellation  *cancellationMeta
	progress      *progressMeta
	taskResult    *taskMeta
	taskStatus    *taskMeta
	taskOperation string
	taskID        string
	utilityErr    error
}

func parseFrame(raw []byte, maxBytes int) (*parsedFrame, error) {
	if maxBytes > 0 && len(raw) > maxBytes {
		return nil, ErrFrameTooLarge
	}
	if bytes.ContainsAny(raw, "\r\n") {
		return nil, fmt.Errorf("supervisor: frame contains a line break")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("supervisor: empty frame")
	}

	fields, err := decodeObject(trimmed)
	if err != nil {
		return nil, fmt.Errorf("supervisor: JSON-RPC object: %w", err)
	}
	version, ok := fields["jsonrpc"]
	if !ok {
		return nil, fmt.Errorf("supervisor: missing jsonrpc")
	}
	if got, err := parseString(version, "jsonrpc"); err != nil || got != "2.0" {
		return nil, fmt.Errorf("supervisor: jsonrpc must be \"2.0\"")
	}

	methodRaw, hasMethod := fields["method"]
	idRaw, hasID := fields["id"]
	resultRaw, hasResult := fields["result"]
	errorRaw, hasError := fields["error"]
	paramsRaw, hasParams := fields["params"]

	if hasParams && !isStructured(paramsRaw) {
		return nil, fmt.Errorf("supervisor: params must be an object or array")
	}
	if hasResult && hasError {
		return nil, fmt.Errorf("supervisor: response contains both result and error")
	}

	frame := &parsedFrame{raw: append([]byte(nil), trimmed...)}
	if hasID {
		id, err := parseScalar(idRaw)
		if err != nil {
			return nil, fmt.Errorf("supervisor: invalid id: %w", err)
		}
		frame.id = &id
	}

	switch {
	case hasMethod:
		if hasResult || hasError {
			return nil, fmt.Errorf("supervisor: method frame contains result or error")
		}
		method, err := parseString(methodRaw, "method")
		if err != nil {
			return nil, fmt.Errorf("supervisor: %w", err)
		}
		frame.method = method
		frame.reserved = strings.HasPrefix(method, reservedMethodPrefix)
		if hasID {
			frame.kind = frameRequest
		} else {
			frame.kind = frameNotification
		}
		if hasParams {
			frame.params = cloneRaw(paramsRaw)
		}
	case hasResult || hasError:
		if !hasID {
			return nil, fmt.Errorf("supervisor: response is missing id")
		}
		if hasParams {
			return nil, fmt.Errorf("supervisor: response contains params")
		}
		if hasResult {
			frame.kind = frameResult
			frame.result = cloneRaw(resultRaw)
		} else {
			if err := validateErrorObject(errorRaw); err != nil {
				return nil, err
			}
			frame.kind = frameError
			frame.errorData = cloneRaw(errorRaw)
		}
	default:
		return nil, fmt.Errorf("supervisor: frame has neither method nor result/error")
	}

	frame.extractUtilities()
	return frame, nil
}

func (frame *parsedFrame) extractUtilities() {
	if frame.kind == frameRequest && len(frame.params) > 0 {
		params, err := decodeObject(frame.params)
		if err == nil {
			if metaRaw, ok := params["_meta"]; ok {
				meta, metaErr := decodeObject(metaRaw)
				if metaErr != nil {
					frame.setUtilityErr(fmt.Errorf("_meta: %w", metaErr))
				} else if tokenRaw, ok := meta["progressToken"]; ok {
					token, tokenErr := parseScalar(tokenRaw)
					if tokenErr != nil {
						frame.setUtilityErr(fmt.Errorf("progressToken: %w", tokenErr))
					} else {
						frame.progressToken = &token
					}
				}
			}
			if taskRaw, ok := params["task"]; ok {
				if _, taskErr := decodeObject(taskRaw); taskErr != nil {
					frame.setUtilityErr(fmt.Errorf("task: %w", taskErr))
				} else {
					frame.taskAugmented = true
				}
			}
		}
	}

	switch frame.method {
	case "notifications/cancelled":
		if frame.kind != frameNotification {
			frame.setUtilityErr(fmt.Errorf("cancellation must be a notification"))
			return
		}
		params, err := decodeRequiredParams(frame.params)
		if err != nil {
			frame.setUtilityErr(err)
			return
		}
		raw, ok := params["requestId"]
		if !ok {
			frame.setUtilityErr(fmt.Errorf("cancellation requestId is missing"))
			return
		}
		id, err := parseScalar(raw)
		if err != nil {
			frame.setUtilityErr(fmt.Errorf("cancellation requestId: %w", err))
			return
		}
		frame.cancellation = &cancellationMeta{requestID: id}

	case "notifications/progress":
		if frame.kind != frameNotification {
			frame.setUtilityErr(fmt.Errorf("progress must be a notification"))
			return
		}
		params, err := decodeRequiredParams(frame.params)
		if err != nil {
			frame.setUtilityErr(err)
			return
		}
		tokenRaw, tokenOK := params["progressToken"]
		progressRaw, progressOK := params["progress"]
		if !tokenOK || !progressOK {
			frame.setUtilityErr(fmt.Errorf("progress token or value is missing"))
			return
		}
		token, err := parseScalar(tokenRaw)
		if err != nil {
			frame.setUtilityErr(fmt.Errorf("progressToken: %w", err))
			return
		}
		if len(bytes.TrimSpace(progressRaw)) > maxCorrelationValueBytes {
			frame.setUtilityErr(fmt.Errorf("progress value exceeds correlation limit"))
			return
		}
		progress, err := canonicalJSONNumber(string(bytes.TrimSpace(progressRaw)))
		if err != nil {
			frame.setUtilityErr(fmt.Errorf("progress value: %w", err))
			return
		}
		frame.progress = &progressMeta{token: token, progress: progress}

	case "notifications/tasks/status":
		if frame.kind != frameNotification {
			frame.setUtilityErr(fmt.Errorf("task status must be a notification"))
			return
		}
		meta, err := parseTaskMeta(frame.params)
		if err != nil {
			frame.setUtilityErr(err)
			return
		}
		frame.taskStatus = meta

	case "tasks/get", "tasks/result", "tasks/cancel":
		if frame.kind != frameRequest {
			frame.setUtilityErr(fmt.Errorf("task operation must be a request"))
			return
		}
		params, err := decodeRequiredParams(frame.params)
		if err != nil {
			frame.setUtilityErr(err)
			return
		}
		taskRaw, ok := params["taskId"]
		if !ok {
			frame.setUtilityErr(fmt.Errorf("taskId is missing"))
			return
		}
		taskID, err := parseString(taskRaw, "taskId")
		if err != nil || taskID == "" || len(taskID) > maxCorrelationValueBytes {
			frame.setUtilityErr(fmt.Errorf("taskId must be a non-empty bounded string"))
			return
		}
		frame.taskOperation = frame.method
		frame.taskID = taskID
	}

	if frame.kind == frameResult && len(frame.result) > 0 {
		result, err := decodeObject(frame.result)
		if err != nil {
			return
		}
		if taskRaw, ok := result["task"]; ok {
			meta, taskErr := parseTaskMeta(taskRaw)
			if taskErr != nil {
				frame.setUtilityErr(taskErr)
				return
			}
			frame.taskResult = meta
		}
	}
}

func (frame *parsedFrame) setUtilityErr(err error) {
	if frame.utilityErr == nil {
		frame.utilityErr = err
	}
}

func decodeRequiredParams(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("params are missing")
	}
	params, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	return params, nil
}

func parseTaskMeta(raw json.RawMessage) (*taskMeta, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("task: %w", err)
	}
	idRaw, idOK := fields["taskId"]
	statusRaw, statusOK := fields["status"]
	if !idOK || !statusOK {
		return nil, fmt.Errorf("taskId or task status is missing")
	}
	id, err := parseString(idRaw, "taskId")
	if err != nil || id == "" || len(id) > maxCorrelationValueBytes {
		return nil, fmt.Errorf("taskId must be a non-empty bounded string")
	}
	status, err := parseString(statusRaw, "task status")
	if err != nil || !validTaskStatus(status) {
		return nil, fmt.Errorf("invalid task status")
	}
	return &taskMeta{id: id, status: status}, nil
}

func validTaskStatus(status string) bool {
	switch status {
	case "working", "input_required", "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func validateErrorObject(raw json.RawMessage) error {
	fields, err := decodeObject(raw)
	if err != nil {
		return fmt.Errorf("supervisor: error response: %w", err)
	}
	codeRaw, codeOK := fields["code"]
	messageRaw, messageOK := fields["message"]
	if !codeOK || !messageOK {
		return fmt.Errorf("supervisor: error response requires code and message")
	}
	code, err := parseScalar(codeRaw)
	if err != nil || code.key.kind != scalarNumber || !canonicalNumberIsInteger(code.key.value) {
		return fmt.Errorf("supervisor: error code must be an integer")
	}
	if _, err := parseString(messageRaw, "error message"); err != nil {
		return fmt.Errorf("supervisor: %w", err)
	}
	return nil
}

func canonicalNumberIsInteger(value string) bool {
	if value == "0" {
		return true
	}
	index := strings.LastIndexByte(value, 'e')
	if index < 0 {
		return false
	}
	exponent := new(big.Int)
	if _, ok := exponent.SetString(value[index+1:], 10); !ok {
		return false
	}
	return exponent.Sign() >= 0
}

func isStructured(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if opening != json.Delim('{') {
		return nil, fmt.Errorf("expected object")
	}

	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected object key")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if closing != json.Delim('}') {
		return nil, fmt.Errorf("expected object close")
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("trailing JSON token %v", token)
	}
	return fields, nil
}

func readBoundedLine(reader *bufio.Reader, maxBytes int) ([]byte, error) {
	if maxBytes < 1 {
		return nil, fmt.Errorf("supervisor: frame limit must be positive")
	}
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxBytes+2 {
			return nil, ErrFrameTooLarge
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			line = line[:len(line)-1]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
		default:
			return nil, err
		}
		if len(line) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		if len(line) > maxBytes {
			return nil, ErrFrameTooLarge
		}
		return line, nil
	}
}
