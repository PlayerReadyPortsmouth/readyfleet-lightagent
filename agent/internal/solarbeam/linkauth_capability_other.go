//go:build !windows

package solarbeam

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
)

func linkCapabilityCommand(ctx context.Context, path string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path)
	configureLinkCapabilityCommand(cmd, linkCapabilityEnvironment())
	if runtime.GOOS == "linux" {
		cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+filepath.Join(filepath.Dir(path), "lib"))
	}
	return cmd
}

func linkEngineMatches(Paths) bool { return true }
