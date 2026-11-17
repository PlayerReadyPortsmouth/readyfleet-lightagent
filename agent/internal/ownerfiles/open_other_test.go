//go:build !windows

package ownerfiles

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func TestSpecialFileRefusedWithoutBlocking(t *testing.T) {
	m, dir := fixture(t)
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := m.download(context.Background(), proto.DownloadFileArgs{Path: "fifo", UploadURL: "https://controller.test/file"})
		finished <- err
	}()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		// Unblock a broken implementation so the test cannot leave a goroutine.
		go func() {
			f, _ := os.OpenFile(filepath.Join(dir, "fifo"), os.O_WRONLY, 0)
			if f != nil {
				f.Close()
			}
		}()
		t.Fatal("FIFO blocked before file-type validation")
	}
}
