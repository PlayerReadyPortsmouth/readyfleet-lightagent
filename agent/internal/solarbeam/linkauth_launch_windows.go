//go:build windows

package solarbeam

func (m *Manager) provisionPlatformLinkAuth() error {
	return m.provisionLinkAuth(func(secret []byte) (*linkCredential, error) {
		if m.paths.Mode == ModeInteractive {
			// An ordinary BYOD user cannot create or read a SYSTEM/admin-only file.
			// Both direct children consume and clear this variable before spawning helpers.
			return &linkCredential{env: []string{"SOLARBEAM_LINK_SECRET=" + string(secret)}}, nil
		}
		// The SYSTEM engine always reads the protected file (via bridge.env).
		path, cleanup, err := writeWindowsLinkSecret(secret)
		if err != nil {
			return nil, err
		}
		if m.restrictedBridge() {
			// The restricted bridge cannot (and must not) open the SYSTEM/
			// Administrators-only file. It gets the same key in its direct
			// environment, readable only by SYSTEM and its own logon SID
			// (bridge_restricted_windows.go), and clears it at startup.
			return &linkCredential{env: []string{"SOLARBEAM_LINK_SECRET=" + string(secret)}, cleanup: cleanup}, nil
		}
		return &linkCredential{env: []string{"SOLARBEAM_LINK_SECRET_FILE=" + path}, cleanup: cleanup}, nil
	})
}

// restrictedBridge reports whether this launch runs the bridge under the
// restricted LocalService token. Managed only: BYOD already runs it as the user.
func (m *Manager) restrictedBridge() bool {
	return m.paths.Mode == ModeManaged && m.paths.RestrictedBridge
}

func (m *Manager) removeRecoveredLinkAuth() error {
	if m.paths.Mode == ModeManaged {
		return removeWindowsLinkSecret()
	}
	return nil
}

// Do not rotate/remove a key while an old authenticated bridge can still run.
func stopBridgeForLinkRotation(name string) error {
	return stopBridgeForLinkRotationWith(name, terminateByName, func(name string) (bool, error) {
		found := false
		err := forEachProcessNamed(name, func(uint32) bool {
			found = true
			return false
		})
		return found, err
	})
}
