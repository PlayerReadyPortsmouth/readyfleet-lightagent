package svc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenServiceLogger_WritesJSONLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	log, closer, err := OpenServiceLogger(logPath)
	if err != nil {
		t.Fatalf("OpenServiceLogger: %v", err)
	}

	log.Info("hello", "k", "v", "n", 42)
	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d: %q", len(lines), string(raw))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log line: %v\nline: %q", err, lines[0])
	}
	if entry["msg"] != "hello" {
		t.Errorf("msg: got %v", entry["msg"])
	}
	if entry["k"] != "v" {
		t.Errorf("k: got %v", entry["k"])
	}
	// n was passed as int 42; JSON unmarshals to float64.
	if entry["n"] != float64(42) {
		t.Errorf("n: got %v", entry["n"])
	}
}

func TestOpenServiceLogger_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "nested", "deeper", "agent.log")

	_, closer, err := OpenServiceLogger(logPath)
	if err != nil {
		t.Fatalf("OpenServiceLogger: %v", err)
	}
	defer closer.Close()

	if _, err := os.Stat(filepath.Dir(logPath)); err != nil {
		t.Errorf("parent dir not created: %v", err)
	}
}
