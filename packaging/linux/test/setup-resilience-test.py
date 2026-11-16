#!/usr/bin/env python3
"""Run the Linux setup transaction against temporary user files and fake sudo.

No root integration, package manager or real user service is invoked.
"""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SOURCE = Path(__file__).resolve().parents[1] / "setup-resilient.sh"


class SetupTransactionTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="readyfleet-setup-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.bundle = self.root / "bundle"
        (self.bundle / "engine").mkdir(parents=True)
        (self.bundle / "engine" / "VERSION").write_text("new-engine")
        (self.bundle / "integration").mkdir()
        self.tool("systemctl", 'exit "${SERVICE_FAIL:-0}"')
        self.tool("df", 'printf "Filesystem 1024-blocks Used Available Capacity Mounted\\nfixture 9999999 0 %s 0%% /\\n" "${FREE_KB:-9999999}"')
        self.tool("sudo", '''
case "$1" in
 -v) exit "${SUDO_FAIL:-0}";;
 test) [[ ${PRIOR_INTEGRATION:-0} == 1 ]]; exit $?;;
 apt-get) exit 0;;
 *) exec "$@";;
esac''')
        self.tool("mv", '''
if [[ ${ACTIVATION_FAIL:-0} == 1 && $2 == */.readyfleet-setup-*/engine ]]; then exit 1; fi
exec /usr/bin/mv "$@"''')
        integration = self.bundle / "integration" / "install-system-integration.sh"
        integration.write_text('''#!/usr/bin/env bash
if [[ $1 == --uninstall ]]; then
  echo uninstall >> "$FIXTURE/events"
  exit "${UNDO_FAIL:-0}"
fi
echo integrate >> "$FIXTURE/events"
exit "${INTEGRATION_FAIL:-0}"
''')
        integration.chmod(0o755)
        # Substitute just the host-release path: no real distro fixture can
        # require changing the developer's /etc/os-release.
        release = self.root / "os-release"
        release.write_text('ID=ubuntu\nVERSION_ID=24.04\n')
        helper = self.root / "helper.sh"
        helper.write_text(SOURCE.read_text().replace(". /etc/os-release", f". '{release}'"))
        self.data = self.root / "data"
        self.engine = self.data / "solarbeam"
        self.helper = helper

    def tool(self, name, body):
        path = self.bin / name
        path.write_text("#!/usr/bin/env bash\n" + body + "\n")
        path.chmod(0o755)

    def prior_engine(self):
        self.engine.mkdir(parents=True)
        (self.engine / "VERSION").write_text("old-engine")

    def run_setup(self, **env):
        script = '''set -euo pipefail
source "$HELPER"
bundle_dir=$BUNDLE
resilient_begin ubuntu
resilient_activate
echo enroll >> "$FIXTURE/events"
if [[ ${ENROLL_FAIL:-0} != 0 ]]; then exit 10; fi
committed=1
'''
        return subprocess.run(["bash", "-c", script], text=True, capture_output=True,
                              env={**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}",
                                   "HELPER": str(self.helper), "BUNDLE": str(self.bundle),
                                   "XDG_DATA_HOME": str(self.data), "FIXTURE": str(self.root), **env})

    def events(self):
        path = self.root / "events"
        return path.read_text().splitlines() if path.exists() else []

    def clean_transactions(self):
        return not list(self.data.glob(".readyfleet-setup*"))

    def test_success_keeps_new_engine_and_cleans_backup(self):
        self.prior_engine()
        result = self.run_setup()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.engine / "VERSION").read_text(), "new-engine")
        self.assertEqual(self.events(), ["integrate", "enroll"])
        self.assertTrue(self.clean_transactions())

    def test_failed_enrollment_restores_previous_and_uninstalls_new_integration(self):
        self.prior_engine()
        result = self.run_setup(ENROLL_FAIL="1")
        self.assertEqual(result.returncode, 10, result.stderr)
        self.assertEqual((self.engine / "VERSION").read_text(), "old-engine")
        self.assertEqual(self.events(), ["integrate", "enroll", "uninstall"])
        self.assertTrue(self.clean_transactions())

    def test_failed_fresh_install_removes_new_engine(self):
        result = self.run_setup(ENROLL_FAIL="1")
        self.assertEqual(result.returncode, 10, result.stderr)
        self.assertFalse(self.engine.exists())
        self.assertTrue(self.clean_transactions())

    def test_failed_integration_restores_previous_before_enrollment(self):
        self.prior_engine()
        result = self.run_setup(INTEGRATION_FAIL="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.engine / "VERSION").read_text(), "old-engine")
        self.assertEqual(self.events(), ["integrate", "uninstall"])

    def test_preserves_preexisting_integration_on_failure(self):
        self.prior_engine()
        result = self.run_setup(PRIOR_INTEGRATION="1", ENROLL_FAIL="1")
        self.assertEqual(result.returncode, 10, result.stderr)
        self.assertEqual(self.events(), ["integrate", "enroll"])
        self.assertEqual((self.engine / "VERSION").read_text(), "old-engine")

    def test_failed_activation_restores_previous_without_running_integration(self):
        self.prior_engine()
        result = self.run_setup(ACTIVATION_FAIL="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.engine / "VERSION").read_text(), "old-engine")
        self.assertEqual(self.events(), [])
        self.assertTrue(self.clean_transactions())

    def test_cleanup_failure_is_explicit_and_preserves_transaction(self):
        self.prior_engine()
        result = self.run_setup(ENROLL_FAIL="1", UNDO_FAIL="1")
        self.assertEqual(result.returncode, 11)
        self.assertIn("Automatic cleanup needs help", result.stderr)
        self.assertEqual((self.engine / "VERSION").read_text(), "old-engine")
        self.assertTrue(list(self.data.glob(".readyfleet-setup-*/")))

    def test_preflight_does_not_mutate_engine(self):
        for config in ({"FREE_KB": "1"}, {"SUDO_FAIL": "1"}, {"SERVICE_FAIL": "1"}):
            with self.subTest(config=config):
                result = self.run_setup(**config)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertFalse(self.engine.exists())
                self.assertEqual(self.events(), [])

    def test_distro_entrypoints_run_integration_before_agent_and_commit_success(self):
        for distro in ("ubuntu", "cachyos"):
            with self.subTest(distro=distro):
                self.prior_engine()
                release = self.root / "os-release"
                release.write_text("ID=ubuntu\nVERSION_ID=24.04\n" if distro == "ubuntu" else "ID=cachyos\nVERSION_ID=2026\n")
                (self.bundle / "setup-resilient.sh").write_text(self.helper.read_text())
                setup_source = SOURCE.parent / f"setup-{distro}.sh"
                setup = self.bundle / f"setup-{distro}.sh"
                setup.write_text(setup_source.read_text().replace(". /etc/os-release", f". '{release}'"))
                for path in (self.bundle / "engine" / "sunshine", self.bundle / "engine" / "solarbeam-bridge", self.bundle / "readyfleet-light", self.bundle / "uninstall-linux.sh"):
                    path.write_text("#!/bin/sh\nexit 0\n")
                    path.chmod(0o755)
                agent = self.bundle / "readyfleet-light-installer"
                agent.write_text('#!/bin/sh\necho enroll >> "$FIXTURE/events"\n')
                agent.chmod(0o755)
                self.tool("pacman", "exit 0")
                payload = self.root / "enrollment.json"
                payload.write_text('{"enrollment_token":"fixture","resilient_install":true}')
                result = subprocess.run(["bash", str(setup), str(payload)], text=True, capture_output=True,
                                        env={**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}", "FIXTURE": str(self.root),
                                             "XDG_DATA_HOME": str(self.data), "XDG_CONFIG_HOME": str(self.root / "config")})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.events(), ["integrate", "enroll"])
                self.assertEqual((self.engine / "VERSION").read_text(), "new-engine")
                self.assertTrue(self.clean_transactions())
                (self.root / "events").unlink()
                import shutil
                shutil.rmtree(self.engine)

    def test_concurrent_setup_is_refused(self):
        self.data.mkdir()
        (self.data / ".readyfleet-setup.lock").mkdir()
        result = self.run_setup()
        self.assertEqual(result.returncode, 2)
        self.assertIn("Another ReadyFleet setup", result.stderr)
        self.assertTrue((self.data / ".readyfleet-setup.lock").exists())
        self.assertFalse(self.engine.exists())


if __name__ == "__main__":
    unittest.main()
