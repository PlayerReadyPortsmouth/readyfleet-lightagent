package companionadapter

import (
	"encoding/json"
	"os"
	"path/filepath"

	ui "github.com/playerreadyportsmouth/readyfleet/agent/internal/companionui"
)

// Only shortcut preferences reach disk. Broker credentials never enter this
// format or any config/environment passed to a child process.
func LoadShortcuts(path string) [4]ui.Shortcut {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 4096 {
		return ui.DefaultShortcuts()
	}
	var bindings [4]ui.Shortcut
	if json.Unmarshal(raw, &bindings) != nil || ui.ValidateShortcuts(bindings) != nil {
		return ui.DefaultShortcuts()
	}
	return bindings
}
func SaveShortcuts(path string, bindings [4]ui.Shortcut) error {
	if err := ui.ValidateShortcuts(bindings); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}
