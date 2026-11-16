package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCommandHandlersNeverDecodeTheEnvelopeDirectly is the tripwire from the
// 6 Sep 2026 incident: a command handler decoded the command ENVELOPE
// (env.DecodeData(&args)) instead of unwrapping CommandData.Args, so every
// args field came out zero — and an empty software-control mode failed open
// into blocking processes on venue machines.
//
// The contract: a command handler decodes proto.CommandData from the
// envelope first (exec's decodeArgs helper does this), then
// CommandData.DecodeArgs into its per-kind args.
//
// Mechanics: any `env.DecodeData(&x)` in the handler packages is flagged
// unless (a) x is declared as proto.CommandData in the same file, or (b) the
// (file, variable) pair is in the allowlist of NON-command message handlers
// (terminal/screen/override-decision envelopes carry their payload directly,
// with no CommandData wrapper). A new handler file starts flagged by
// default: either decode the wrapper or consciously extend the allowlist
// with a comment saying which non-command message type it decodes.
func TestCommandHandlersNeverDecodeTheEnvelopeDirectly(t *testing.T) {
	handlerDirs := []string{
		"../exec", "../security", "../appcontrol", "../installer", "../solarbeam",
		"../endofday", "../usbstorage", "../operatingmode",
	}
	// (file base name, decoded variable) pairs that decode NON-command
	// message payloads and are therefore correct as-is.
	allowed := map[string]bool{
		"terminal.go|d":  true, // MsgTerminalOpen/Input/Resize/Close payloads
		"screencap.go|d": true, // MsgScreenStart/Stop payloads
		"manager.go|d":   true, // MsgOverrideDecision payload (appcontrol)
	}
	call := regexp.MustCompile(`\.DecodeData\(&(\w+)`)

	for _, dir := range handlerDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			src := string(raw)
			for _, m := range call.FindAllStringSubmatch(src, -1) {
				v := m[1]
				declared := regexp.MustCompile(fmt.Sprintf(`var\s+%s\s+proto\.CommandData`, regexp.QuoteMeta(v)))
				if declared.MatchString(src) {
					continue // decoding the wrapper — the sanctioned pattern
				}
				if allowed[e.Name()+"|"+v] {
					continue
				}
				t.Errorf("%s: env.DecodeData(&%s) without a proto.CommandData declaration — command handlers must decode the CommandData wrapper then cmd.DecodeArgs(&%s); if this decodes a NON-command message payload, add it to this test's allowlist with a comment (see the 6 Sep software-control incident)", path, v, v)
			}
		}
	}
}
