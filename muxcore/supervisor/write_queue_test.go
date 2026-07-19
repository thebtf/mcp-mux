package supervisor

import (
	"errors"
	"io"
	"testing"
	"time"
)

type resultWriter struct {
	n   int
	err error
}

func (writer resultWriter) Write(buffer []byte) (int, error) {
	if writer.n < 0 {
		return len(buffer), writer.err
	}
	return writer.n, writer.err
}

func TestWriteFrameOutcomes(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	tests := []struct {
		name    string
		writer  io.Writer
		outcome writeOutcome
		err     error
	}{
		{name: "delivered", writer: resultWriter{n: -1}, outcome: writeDelivered},
		{name: "delivered with error", writer: resultWriter{n: -1, err: boom}, outcome: writeDelivered, err: boom},
		{name: "not written", writer: resultWriter{n: 0, err: boom}, outcome: writeNotWritten, err: boom},
		{name: "partial error", writer: resultWriter{n: 1, err: boom}, outcome: writeAmbiguous, err: boom},
		{name: "partial nil", writer: resultWriter{n: 1}, outcome: writeAmbiguous, err: io.ErrShortWrite},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			outcome, err := writeFrame(test.writer, []byte("{}"))
			if outcome != test.outcome || !errors.Is(err, test.err) {
				t.Fatalf("writeFrame = (%v, %v), want (%v, %v)", outcome, err, test.outcome, test.err)
			}
		})
	}
}

func TestPendingQueueCountBytesCancellationAndOrder(t *testing.T) {
	t.Parallel()

	first, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"a"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"b"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	queue := pendingQueue{}
	one := &pendingFrame{sequence: 1, arrived: time.Unix(1, 0), frame: first}
	two := &pendingFrame{sequence: 2, arrived: time.Unix(2, 0), frame: second}
	maxBytes := len(first.raw) + len(second.raw) + 2
	if !queue.enqueue(one, 2, maxBytes) || !queue.enqueue(two, 2, maxBytes) {
		t.Fatal("bounded queue rejected exact capacity")
	}
	if queue.enqueue(two, 3, maxBytes) {
		t.Fatal("bounded queue exceeded byte capacity")
	}
	if !queue.removeRequest(first.id.key) {
		t.Fatal("pending cancellation did not remove request")
	}
	if got := queue.pop(); got != two {
		t.Fatalf("remaining FIFO item = %#v", got)
	}
	if queue.len() != 0 || queue.bytes != 0 {
		t.Fatalf("queue retained count=%d bytes=%d", queue.len(), queue.bytes)
	}
}

func TestProgressComparisonIsExactAndMonotonic(t *testing.T) {
	t.Parallel()

	registry := newDirectionRegistry()
	request, err := parseFrame([]byte(`{"jsonrpc":"2.0","id":"r","method":"x","params":{"_meta":{"progressToken":"p"}}}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	registry.register(request, 1)
	progress := func(value string) *progressMeta {
		frame, parseErr := parseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"p","progress":`+value+`}}`), 1024)
		if parseErr != nil || frame.utilityErr != nil {
			t.Fatalf("progress %s: parse=%v utility=%v", value, parseErr, frame.utilityErr)
		}
		return frame.progress
	}
	if !registry.acceptProgress(progress("0.10"), 1) {
		t.Fatal("initial progress rejected")
	}
	if registry.acceptProgress(progress("1e-1"), 1) {
		t.Fatal("equivalent progress was not rejected")
	}
	if !registry.acceptProgress(progress("0.1000000000000000000001"), 1) {
		t.Fatal("exact larger progress rejected")
	}
	if registry.acceptProgress(progress("0.09"), 1) {
		t.Fatal("decreasing progress accepted")
	}
}
