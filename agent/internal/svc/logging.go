package svc

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

// OpenServiceLogger returns an slog.Logger that writes JSON lines to a
// lumberjack-rotated file at path. The returned io.Closer flushes any
// buffered writes on Close — main() should defer it.
//
// Rotation: 100 MB per file, 7 backups, 30 days, gzip-compressed.
func OpenServiceLogger(path string) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("svc/logging: mkdir %s: %w", filepath.Dir(path), err)
	}
	lj := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    100, // MB
		MaxBackups: 7,
		MaxAge:     30, // days
		Compress:   true,
	}
	h := slog.NewJSONHandler(lj, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h), lj, nil
}
