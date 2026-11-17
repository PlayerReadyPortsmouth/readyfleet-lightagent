//go:build !windows

package ownerfiles

import (
	"os"
	"syscall"
)

// Nonblocking open prevents a FIFO planted in the chosen folder from hanging
// before the regular-file check. It has no effect on ordinary disk files.
func openRead(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
