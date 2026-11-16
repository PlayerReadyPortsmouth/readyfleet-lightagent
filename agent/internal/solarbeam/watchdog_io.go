package solarbeam

import (
	"os"
	"path/filepath"
)

const maxLogRead = 1 << 20

// readFrom returns bytes appended since offset (all of it is skipped on the
// first, unprimed call). A shrunken file is treated as rotated and read afresh.
func readFrom(path string, offset int64, primed bool) ([]byte, int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, offset, false
	}
	size := fi.Size()
	if !primed {
		return nil, size, true
	}
	if size < offset {
		offset = 0
	}
	if size-offset > maxLogRead {
		offset = size - maxLogRead
	}
	if size == offset {
		return nil, size, true
	}
	buf := make([]byte, size-offset)
	n, _ := f.ReadAt(buf, offset)
	if n < len(buf) && n >= 0 {
		buf = buf[:n]
	}
	return buf, size, true
}

// engineLogPath is where the engine writes sunshine.log (<install>/config).
func engineLogPath(enginePath string) string {
	return filepath.Join(filepath.Dir(enginePath), "config", "sunshine.log")
}
