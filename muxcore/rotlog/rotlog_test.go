package rotlog

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// TestRotatesAtThreshold: MaxSize=100, MaxFiles=3.
// Write 60 bytes → no rotation. Write another 60 bytes → rotation fires (120 total > 100).
func TestRotatesAtThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := New(Config{Path: path, MaxSize: 100, MaxFiles: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	first := make([]byte, 60)
	for i := range first {
		first[i] = 'A'
	}
	if _, err := w.Write(first); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	// No rotation yet: .1 should not exist
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("expected .1 not to exist after first write, got stat err=%v", err)
	}

	second := make([]byte, 60)
	for i := range second {
		second[i] = 'B'
	}
	if _, err := w.Write(second); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	// Rotation should have happened: .1 exists with the first 60 bytes
	info, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("expected .1 to exist after rotation: %v", err)
	}
	if info.Size() != 60 {
		t.Errorf("expected .1 size=60, got %d", info.Size())
	}

	// Active file should have only the second write (60 bytes)
	activeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("active file stat: %v", err)
	}
	if activeInfo.Size() != 60 {
		t.Errorf("expected active size=60, got %d", activeInfo.Size())
	}
}

// TestKeepsFileCountCap: MaxSize=50, MaxFiles=3.
// Write enough to trigger 4+ rotations. .3 must NOT exist (only active + .1 + .2).
func TestKeepsFileCountCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := New(Config{Path: path, MaxSize: 50, MaxFiles: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	// 6 writes of 30 bytes each → triggers 4+ rotations at MaxSize=50
	chunk := make([]byte, 30)
	for i := range chunk {
		chunk[i] = 'X'
	}
	for i := 0; i < 6; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// .3 must not exist (MaxFiles=3 → active + .1 + .2 only)
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("expected .3 to not exist, got stat err=%v", err)
	}

	// Active, .1, .2 should exist
	for _, suffix := range []string{"", ".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("expected %s%s to exist: %v", path, suffix, err)
		}
	}
}

// TestRotatesOnOpenIfAlreadyTooBig: pre-create a 200-byte file (exceeds MaxSize=100).
// New opens it. First Write triggers rotation; active is fresh, .1 holds original.
func TestRotatesOnOpenIfAlreadyTooBig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Pre-create file with 200 bytes content (exceeds MaxSize=100)
	content := make([]byte, 200)
	for i := range content {
		content[i] = 'Z'
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	w, err := New(Config{Path: path, MaxSize: 100, MaxFiles: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	// Write a small payload — triggers rotation (existing file is already too big)
	payload := []byte("hello\n")
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// .1 should contain the original 200-byte content
	info, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf(".1 should exist after rotation of too-big file: %v", err)
	}
	if info.Size() != 200 {
		t.Errorf("expected .1 size=200, got %d", info.Size())
	}

	// Active file should only contain our small write
	activeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("active file stat: %v", err)
	}
	if activeInfo.Size() != int64(len(payload)) {
		t.Errorf("expected active size=%d, got %d", len(payload), activeInfo.Size())
	}
}

// TestConcurrentWrites: 10 goroutines each writing 100 small lines.
// Total byte count across active+rotated files must equal total written.
// Run with -race to catch data races.
func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// MaxFiles=100 prevents eviction during this stress test so the
	// "no bytes lost under concurrent rotation" invariant is observable.
	// MaxSize=1024 still forces ~17 rotations over 18000 bytes of writes —
	// the concurrency+rotation race is still exercised.
	w, err := New(Config{Path: path, MaxSize: 1024, MaxFiles: 100})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	const goroutines = 10
	const writesPerGoroutine = 100
	line := []byte("hello world 12345\n") // 18 bytes
	expectedTotal := int64(goroutines * writesPerGoroutine * len(line))

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < writesPerGoroutine; j++ {
				if _, err := w.Write(line); err != nil {
					t.Errorf("concurrent Write: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// Close to flush
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Count total bytes across all files: active + .1 ... .4
	var totalBytes int64
	// Scan all rotated files up to MaxFiles-1. With MaxFiles=100 and ~17
	// actual rotations, we stop counting when a file doesn't exist.
	totalBytes = 0
	for i := 0; i < 100; i++ {
		var suffix string
		if i > 0 {
			suffix = "." + strconv.Itoa(i)
		}
		info, err := os.Stat(path + suffix)
		if err != nil {
			break
		}
		totalBytes += info.Size()
	}

	if totalBytes != expectedTotal {
		t.Errorf("byte count mismatch: expected %d, got %d", expectedTotal, totalBytes)
	}
}

// TestInvalidConfig: bad configs return errors.
func TestInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	if _, err := New(Config{Path: path, MaxSize: 0, MaxFiles: 3}); err == nil {
		t.Error("expected error for MaxSize=0")
	}
	if _, err := New(Config{Path: path, MaxSize: 100, MaxFiles: 0}); err == nil {
		t.Error("expected error for MaxFiles=0")
	}
	if _, err := New(Config{Path: "", MaxSize: 100, MaxFiles: 3}); err == nil {
		t.Error("expected error for empty Path")
	}
}

// TestClose: after Close, Write returns ErrClosed.
func TestClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := New(Config{Path: path, MaxSize: 1024, MaxFiles: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = w.Write([]byte("after close"))
	if err == nil {
		t.Fatal("expected error after Close, got nil")
	}
	if err != ErrClosed {
		t.Errorf("expected ErrClosed, got: %v", err)
	}
}
