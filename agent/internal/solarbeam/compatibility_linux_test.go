//go:build linux

package solarbeam

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testLinuxELF(t *testing.T, path, interpreter string, machine elf.Machine) {
	t.Helper()
	var content bytes.Buffer
	header := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(machine), Version: 1, Ehsize: 64}
	copy(header.Ident[:], []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), 1})
	if interpreter != "" {
		header.Phoff, header.Phentsize, header.Phnum = 64, 56, 1
	}
	if err := binary.Write(&content, binary.LittleEndian, header); err != nil {
		t.Fatal(err)
	}
	if interpreter != "" {
		program := elf.Prog64{Type: uint32(elf.PT_INTERP), Off: 120, Filesz: uint64(len(interpreter) + 1)}
		if err := binary.Write(&content, binary.LittleEndian, program); err != nil {
			t.Fatal(err)
		}
		content.WriteString(interpreter + "\x00")
	}
	if err := os.WriteFile(path, content.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxELFInterpreterRefusesNonELFArchitectureAndUnexpectedLoader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sunshine")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxELFInterpreter(path, "amd64"); err == nil {
		t.Fatal("accepted a script as an ELF executable")
	}
	testLinuxELF(t, path, "/lib64/ld-linux-x86-64.so.2", elf.EM_AARCH64)
	if _, err := linuxELFInterpreter(path, "amd64"); err == nil {
		t.Fatal("accepted another architecture")
	}
	testLinuxELF(t, path, "/tmp/untrusted-loader", elf.EM_X86_64)
	if _, err := linuxELFInterpreter(path, "amd64"); err == nil {
		t.Fatal("would execute an interpreter outside the system loader paths")
	}
	testLinuxELF(t, path, "", elf.EM_X86_64)
	if loader, err := linuxELFInterpreter(path, "amd64"); err != nil || loader != "" {
		t.Fatalf("static ELF: loader %q, error %v", loader, err)
	}
}

func TestLinuxEngineCompatibilityUsesStagedEngineLibrariesAndNeverStartsBinaries(t *testing.T) {
	machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]
	loader := map[string]string{"amd64": "/lib64/ld-linux-x86-64.so.2", "arm64": "/lib/ld-linux-aarch64.so.1"}[runtime.GOARCH]
	if machine == elf.EM_NONE {
		t.Skip("fixture supports the packaged amd64/arm64 architectures")
	}
	dir := filepath.Join(t.TempDir(), "stage with spaces")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sunshine", "solarbeam-bridge"} {
		testLinuxELF(t, filepath.Join(dir, name), loader, machine)
	}
	t.Setenv("LD_LIBRARY_PATH", "/existing/libraries")
	previous := linuxLoaderListFn
	t.Cleanup(func() { linuxLoaderListFn = previous })
	calls := 0
	linuxLoaderListFn = func(_ context.Context, interpreter, executable string, env []string) error {
		calls++
		if interpreter != loader {
			t.Fatalf("loader %q", interpreter)
		}
		libraryPath := ""
		for _, item := range env {
			if strings.HasPrefix(item, "LD_LIBRARY_PATH=") {
				libraryPath = strings.TrimPrefix(item, "LD_LIBRARY_PATH=")
			}
		}
		want := "/existing/libraries"
		if filepath.Base(executable) == "sunshine" {
			want = filepath.Join(dir, "lib") + ":" + want
		}
		if libraryPath != want {
			t.Fatalf("%s library path %q, want %q", executable, libraryPath, want)
		}
		return nil
	}
	if err := checkLinuxEngineCompatibility(context.Background(), dir); err != nil || calls != 2 {
		t.Fatalf("compatibility: %v, %d loader probes", err, calls)
	}
	for _, incompatible := range []string{"sunshine", "solarbeam-bridge"} {
		linuxLoaderListFn = func(_ context.Context, _, executable string, _ []string) error {
			if filepath.Base(executable) == incompatible {
				return errors.New("version GLIBC_2.44 not found / libfoo.so.3 not found")
			}
			return nil
		}
		if err := checkLinuxEngineCompatibility(context.Background(), dir); err == nil || !strings.Contains(err.Error(), incompatible+" is incompatible") {
			t.Fatalf("incompatible %s accepted: %v", incompatible, err)
		}
	}
}
