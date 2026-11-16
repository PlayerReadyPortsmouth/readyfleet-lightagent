//go:build darwin

package solarbeam

// Link authentication on a Mac uses the direct-environment credential, the
// same shape as ordinary Windows BYOD: both children are direct children of
// the agent, SOLARBEAM_LINK_SECRET is in their initial environment, and both
// runtimes consume and clear it as their first act (engine main(),
// bridge startup) before spawning anything. Nothing is written to disk.
//
// Why not the protected file (SOLARBEAM_LINK_SECRET_FILE) Linux uses: the
// bridge's darwin file backend (internal/linkauth/file_darwin.go) is being
// added in a separate change; until it lands the bridge returns errCredential
// for a file on darwin, and a launcher that is the parent of both processes
// does not need the file anyway. When the backend exists this function can
// switch to a 0600 file under the solarbeam directory without touching the
// manager; the contract with the two children is unchanged either way. The
// bridge builder's recommended file location for that day is
// ~/Library/Application Support/ReadyFleet/solarbeam/link-auth/secret (dir
// 0700, file 0600, atomic rename, resolved path: the bridge's O_NOFOLLOW walk
// refuses /var, /tmp and /etc, which are symlinks into /private).
func (m *Manager) provisionPlatformLinkAuth() error {
	return m.provisionLinkAuth(func(secret []byte) (*linkCredential, error) {
		return &linkCredential{env: []string{"SOLARBEAM_LINK_SECRET=" + string(secret)}}, nil
	})
}

// removeRecoveredLinkAuth: nothing is persisted on a Mac, so there is nothing
// to remove after an agent restart.
func (m *Manager) removeRecoveredLinkAuth() error { return nil }
