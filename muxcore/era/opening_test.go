package era

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testOpeningFrameLimit = 1 << 20

func TestReadOpeningFramePreservesSC001CorpusAndPrefetchedTail(t *testing.T) {
	frames := readSC001OpeningCorpus(t)
	var direct, discovery, absentClientInfo, presentClientInfo int

	for index, raw := range frames {
		method, hasClientInfo := classifySC001Opening(t, index, raw)
		if method == "server/discover" {
			discovery++
		} else {
			direct++
		}
		if hasClientInfo {
			presentClientInfo++
		} else {
			absentClientInfo++
		}

		t.Run(fmt.Sprintf("%03d/%s", index+1, method), func(t *testing.T) {
			wantOpening := []byte(raw + "\n")
			wantTail := []byte("{\"jsonrpc\":\"2.0\",\"id\":991,\"method\":\"notifications/cancelled\",\"params\":{}}\n")
			input := append(append([]byte(nil), wantOpening...), wantTail...)

			frame, remainder, err := ReadOpeningFrame(bytes.NewReader(input))
			if err != nil {
				t.Fatalf("ReadOpeningFrame() error = %v", err)
			}
			if frame == nil {
				t.Fatal("ReadOpeningFrame() returned nil frame")
			}
			if remainder == nil {
				t.Fatal("ReadOpeningFrame() returned nil remainder")
			}

			gotOpening, available := frame.Take()
			if !available {
				t.Fatal("first OpeningFrame.Take() = unavailable, want opening frame")
			}
			if !bytes.Equal(gotOpening, wantOpening) {
				t.Fatalf("opening bytes = %q, want %q", gotOpening, wantOpening)
			}
			if gotAgain, available := frame.Take(); available || len(gotAgain) != 0 {
				t.Fatalf("second OpeningFrame.Take() = (%q, %v), want (empty, false)", gotAgain, available)
			}

			gotTail, err := io.ReadAll(remainder)
			if err != nil {
				t.Fatalf("read remainder: %v", err)
			}
			if !bytes.Equal(gotTail, wantTail) {
				t.Fatalf("remainder bytes = %q, want preserved prefetched tail %q", gotTail, wantTail)
			}
		})
	}

	if direct == 0 || discovery == 0 {
		t.Fatalf("SC-001 opening coverage direct=%d discovery=%d, want both", direct, discovery)
	}
	if absentClientInfo == 0 || presentClientInfo == 0 {
		t.Fatalf("SC-001 clientInfo coverage absent=%d present=%d, want both", absentClientInfo, presentClientInfo)
	}
}

func TestReadOpeningFrameRejectsFrameLargerThanOneMiB(t *testing.T) {
	var input bytes.Buffer
	input.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"padding":"`)
	input.Write(bytes.Repeat([]byte{'x'}, testOpeningFrameLimit+1))
	input.WriteString(`"}}` + "\n")

	frame, _, err := ReadOpeningFrame(&input)
	if err == nil {
		t.Fatal("ReadOpeningFrame() accepted a frame larger than 1 MiB")
	}
	if frame != nil {
		t.Fatal("ReadOpeningFrame() returned a frame after rejecting an oversized opener")
	}
}

func readSC001OpeningCorpus(t *testing.T) []string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate opening test source")
	}

	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "testdata", "modern_opening_corpus.ndjson"))
	if err != nil {
		t.Fatalf("read SC-001 opening corpus: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) < 100 {
		t.Fatalf("SC-001 opening corpus has %d frame(s), want at least 100", len(lines))
	}
	return lines
}

func classifySC001Opening(t *testing.T, index int, raw string) (string, bool) {
	t.Helper()
	var envelope struct {
		Method string `json:"method"`
		Params struct {
			Meta json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("SC-001 frame %d is not valid JSON: %v", index, err)
	}
	if envelope.Method == "" {
		t.Fatalf("SC-001 frame %d has no method", index)
	}

	var meta map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Params.Meta, &meta); err != nil || meta == nil {
		t.Fatalf("SC-001 frame %d has invalid params._meta: %v", index, err)
	}
	if _, ok := meta["io.modelcontextprotocol/protocolVersion"]; !ok {
		t.Fatalf("SC-001 frame %d has no protocol version", index)
	}
	if _, ok := meta["io.modelcontextprotocol/clientCapabilities"]; !ok {
		t.Fatalf("SC-001 frame %d has no client capabilities", index)
	}
	_, hasClientInfo := meta["io.modelcontextprotocol/clientInfo"]
	return envelope.Method, hasClientInfo
}
