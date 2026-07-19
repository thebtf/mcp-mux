package supervisor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestParseFrameKindsAndRawID(t *testing.T) {
	t.Parallel()

	request, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":1e0,"method":"tools/list","params":{"cursor":"x"}}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if request.kind != frameRequest || request.method != "tools/list" || request.id == nil {
		t.Fatalf("request = %#v", request)
	}
	if string(request.id.raw) != "1e0" || request.id.key.value != "1e0" {
		t.Fatalf("request id raw=%s key=%q", request.id.raw, request.id.key.value)
	}

	notification, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), 1024)
	if err != nil || notification.kind != frameNotification {
		t.Fatalf("notification = %#v, %v", notification, err)
	}

	result, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":"a","result":{"ok":true}}`), 1024)
	if err != nil || result.kind != frameResult || string(result.result) != `{"ok":true}` {
		t.Fatalf("result = %#v, %v", result, err)
	}

	errorFrame, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"boom"}}`), 1024)
	if err != nil || errorFrame.kind != frameError {
		t.Fatalf("error = %#v, %v", errorFrame, err)
	}
	zeroError, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":3,"error":{"code":0,"message":"zero"}}`), 1024)
	if err != nil || zeroError.kind != frameError {
		t.Fatalf("zero-code error = %#v, %v", zeroError, err)
	}
}

func TestParseFrameRejectsInvalidJSONRPC(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`[]`,
		`{"jsonrpc":"1.0","method":"x"}`,
		`{"jsonrpc":"2.0","id":null,"method":"x"}`,
		`{"jsonrpc":"2.0","id":1,"id":2,"method":"x"}`,
		`{"jsonrpc":"2.0","method":"x","params":1}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"x"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":1.5,"message":"x"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"params":{}}`,
		`{"jsonrpc":"2.0"}`,
		`{"jsonrpc":"2.0","method":"x"} {}`,
	} {
		if _, err := parseFrame([]byte(raw), 1024); err == nil {
			t.Errorf("parseFrame(%s) unexpectedly succeeded", raw)
		}
	}
	if _, err := parseFrame([]byte("{\n}"), 1024); err == nil {
		t.Error("parseFrame accepted a line break")
	}
	if _, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"long"}`), 8); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestParseFrameRejectsNonJSONWhitespaceAndInvalidUTF8(t *testing.T) {
	t.Parallel()

	frame, err := parseFrame([]byte("\t\r\n {\"jsonrpc\":\"2.0\",\"method\":\"ping\"} \r\n"), 1024)
	if err != nil || frame.kind != frameNotification {
		t.Fatalf("JSON whitespace frame = %#v, %v", frame, err)
	}

	for name, raw := range map[string][]byte{
		"vertical-tab wrapper": []byte("\v{\"jsonrpc\":\"2.0\",\"method\":\"ping\"}\v"),
		"form-feed wrapper":    []byte("\f{\"jsonrpc\":\"2.0\",\"method\":\"ping\"}\f"),
		"non-JSON whitespace":  append([]byte{0xc2, 0xa0}, []byte(`{"jsonrpc":"2.0","method":"ping"}`)...),
		"invalid UTF-8 in ID":  []byte("{\"jsonrpc\":\"2.0\",\"id\":\"a\xffb\",\"method\":\"ping\"}"),
		"malformed JSON":       []byte(`{"jsonrpc":"2.0","method":`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFrame(raw, 1024); !errors.Is(err, ErrMalformedJSON) {
				t.Fatalf("parse error = %v, want ErrMalformedJSON", err)
			}
		})
	}
	if _, err := parseFrame([]byte(`[]`), 1024); err == nil || errors.Is(err, ErrMalformedJSON) {
		t.Fatalf("well-formed non-object error = %v, want invalid request classification", err)
	}
}

func TestParseFrameRequiresMCPObjectParamsAndResults(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[]}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":null}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`,
		`{"jsonrpc":"2.0","id":1,"result":[]}`,
		`{"jsonrpc":"2.0","id":1,"result":null}`,
		`{"jsonrpc":"2.0","id":1,"result":true}`,
	} {
		if _, err := parseFrame([]byte(raw), 1024); err == nil || errors.Is(err, ErrMalformedJSON) {
			t.Errorf("parseFrame(%s) error = %v, want structural rejection", raw, err)
		}
	}

	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
	} {
		if _, err := parseFrame([]byte(raw), 1024); err != nil {
			t.Errorf("parseFrame(%s) error = %v", raw, err)
		}
	}
}

func TestInvalidFrameResponseUsesJSONRPCErrorClasses(t *testing.T) {
	t.Parallel()

	if code, message := invalidFrameResponse(fmt.Errorf("wrapped: %w", ErrMalformedJSON)); code != -32700 || message != "parse error" {
		t.Fatalf("malformed response = (%d, %q)", code, message)
	}
	if code, message := invalidFrameResponse(errors.New("invalid envelope")); code != codeInvalidRequest || message != "invalid JSON-RPC frame" {
		t.Fatalf("invalid request response = (%d, %q)", code, message)
	}
}

func TestParseFrameExtractsCorrelationMetadata(t *testing.T) {
	t.Parallel()

	request, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":"r","method":"tools/call","params":{"_meta":{"progressToken":1.0},"task":{}}}`), 2048)
	if err != nil {
		t.Fatal(err)
	}
	if request.utilityErr != nil || !request.taskAugmented || request.progressToken == nil || request.progressToken.key.value != "1e0" {
		t.Fatalf("request metadata = %#v, utility error=%v", request, request.utilityErr)
	}

	cancel, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"\u0072"}}`), 2048)
	if err != nil || cancel.utilityErr != nil || cancel.cancellation == nil || cancel.cancellation.requestID.key.value != "r" {
		t.Fatalf("cancellation = %#v, parse=%v utility=%v", cancel, err, cancel.utilityErr)
	}

	progress, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":1e0,"progress":0.50}}`), 2048)
	if err != nil || progress.utilityErr != nil || progress.progress == nil {
		t.Fatalf("progress = %#v, parse=%v utility=%v", progress, err, progress.utilityErr)
	}
	if progress.progress.token.key.value != "1e0" || progress.progress.progress != "5e-1" {
		t.Fatalf("progress metadata = %#v", progress.progress)
	}

	taskResult, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":"r","result":{"task":{"taskId":"t1","status":"working"}}}`), 2048)
	if err != nil || taskResult.utilityErr != nil || taskResult.taskResult == nil || taskResult.taskResult.id != "t1" {
		t.Fatalf("task result = %#v, parse=%v utility=%v", taskResult, err, taskResult.utilityErr)
	}

	taskStatus, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"t1","status":"completed"}}`), 2048)
	if err != nil || taskStatus.utilityErr != nil || taskStatus.taskStatus == nil || taskStatus.taskStatus.status != "completed" {
		t.Fatalf("task status = %#v, parse=%v utility=%v", taskStatus, err, taskStatus.utilityErr)
	}

	taskOperation, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"tasks/get","params":{"taskId":"t1"}}`), 2048)
	if err != nil || taskOperation.utilityErr != nil || taskOperation.taskOperation != "tasks/get" || taskOperation.taskID != "t1" {
		t.Fatalf("task operation = %#v, parse=%v utility=%v", taskOperation, err, taskOperation.utilityErr)
	}
}

func TestCorrelationMetadataRejectsOversizedRetainedValues(t *testing.T) {
	oversized := strings.Repeat("1", maxCorrelationValueBytes+1)
	progressRaw := `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"p","progress":` + oversized + `}}`
	progress, err := parseFrame([]byte(progressRaw), len(progressRaw)+1)
	if err != nil || progress.utilityErr == nil {
		t.Fatalf("oversized progress parse=%v utility=%v", err, progress.utilityErr)
	}

	taskID := strings.Repeat("t", maxCorrelationValueBytes+1)
	taskRaw := `{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{"taskId":"` + taskID + `","status":"working"}}`
	task, err := parseFrame([]byte(taskRaw), len(taskRaw)+1)
	if err != nil || task.utilityErr == nil {
		t.Fatalf("oversized taskId parse=%v utility=%v", err, task.utilityErr)
	}
}

func TestMalformedUtilityNotificationIsNotAFrameError(t *testing.T) {
	t.Parallel()

	frame, err := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`), 1024)
	if err != nil {
		t.Fatalf("parseFrame returned envelope error: %v", err)
	}
	if frame.utilityErr == nil || frame.cancellation != nil {
		t.Fatalf("malformed cancellation utilityErr=%v metadata=%#v", frame.utilityErr, frame.cancellation)
	}
}

func TestReadBoundedLine(t *testing.T) {
	t.Parallel()

	reader := bufio.NewReaderSize(strings.NewReader("\r\n12345678\r\nlast"), 4)
	line, err := readBoundedLine(reader, 8)
	if err != nil || string(line) != "12345678" {
		t.Fatalf("first line = %q, %v", line, err)
	}
	line, err = readBoundedLine(reader, 8)
	if err != nil || string(line) != "last" {
		t.Fatalf("EOF line = %q, %v", line, err)
	}
	if _, err = readBoundedLine(reader, 8); !errors.Is(err, io.EOF) {
		t.Fatalf("final read error = %v", err)
	}

	oversized := bufio.NewReaderSize(strings.NewReader("123456789\n"), 3)
	if _, err := readBoundedLine(oversized, 8); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized line error = %v", err)
	}
}
