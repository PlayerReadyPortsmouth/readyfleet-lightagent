//go:build linux

package solarbeam

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The signed Linux target is shared by distributions. Before swapping a working
// install, ask the host loader to resolve the staged binaries (including symbol
// versions) without invoking their entry points or opening a capture session.
var linuxEngineCompatibilityFn = checkLinuxEngineCompatibility
var linuxLoaderListFn = listLinuxBinaryDependencies

func checkLinuxEngineCompatibility(ctx context.Context, stage string) error {
	for _, name := range []string{"sunshine", "solarbeam-bridge"} {
		binary := filepath.Join(stage, name)
		interpreter, err := linuxELFInterpreter(binary, runtime.GOARCH)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if interpreter == "" { // A static ELF has no host shared-library requirements.
			continue
		}
		env := os.Environ()
		if name == "sunshine" {
			env = append(env, "LD_LIBRARY_PATH="+engineLibraryPath(binary))
		}
		if err := linuxLoaderListFn(ctx, interpreter, binary, env); err != nil {
			return fmt.Errorf("%s is incompatible with this host: %w", name, err)
		}
	}
	return nil
}

func linuxELFInterpreter(path, arch string) (string, error) {
	file, err := elf.Open(path)
	if err != nil {
		return "", fmt.Errorf("read ELF: %w", err)
	}
	defer file.Close()
	machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if machine == elf.EM_NONE || file.Machine != machine || file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB || (file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN) {
		return "", fmt.Errorf("ELF executable does not match host architecture %s", arch)
	}
	for _, program := range file.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(program.Open(), 4097))
		if err != nil || len(raw) < 2 || len(raw) > 4096 || raw[len(raw)-1] != 0 {
			return "", errors.New("invalid ELF interpreter")
		}
		interpreter := string(raw[:len(raw)-1])
		allowed := map[string][]string{
			"amd64": {"/lib64/ld-linux-x86-64.so.2", "/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2"},
			"arm64": {"/lib/ld-linux-aarch64.so.1", "/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1"},
		}
		for _, candidate := range allowed[arch] {
			if interpreter == candidate {
				return interpreter, nil
			}
		}
		return "", fmt.Errorf("unsupported host ELF interpreter %q", interpreter)
	}
	libraries, err := file.ImportedLibraries()
	if err != nil {
		return "", fmt.Errorf("read ELF dependencies: %w", err)
	}
	if len(libraries) != 0 {
		return "", errors.New("dynamic ELF has no host interpreter")
	}
	return "", nil
}

func listLinuxBinaryDependencies(ctx context.Context, interpreter, binary string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, interpreter, "--list", binary)
	command.Dir = filepath.Dir(binary)
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil || strings.Contains(string(output), "not found") {
		if err == nil {
			err = errors.New("unresolved shared library or symbol version")
		}
		detail := strings.TrimSpace(string(output))
		if len(detail) > 4096 {
			detail = detail[:4096]
		}
		return fmt.Errorf("host loader dependency check failed: %w: %s", err, detail)
	}
	return nil
}
