"""Check the legacy URL's embedded APT wrapper without host changes.

The shared APT suite owns package, service, and enrollment behavior tests.
These tests cover argument forwarding, complete embedding, shell syntax and
truncation at the old bootstrap entry point.
"""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
FINGERPRINT = "A" * 40


def render(shared=None):
    wrapper = (ROOT / "packaging/install.sh.in").read_text()
    if shared is None:
        shared = (ROOT / "packaging/apt/install-apt.sh.in").read_text().strip()
        assert shared.endswith('\nmain "$@"')
        shared = shared.removesuffix('main "$@"')
        shared = shared.replace("@FINGERPRINT@", FINGERPRINT)
    return wrapper.replace("@APT_INSTALLER@", shared)


class InstallerWrapperTests(unittest.TestCase):
    def test_embeds_shared_installer_once_and_selects_headless(self):
        script = render()
        self.assertEqual(script.count("\nmain "), 1)
        self.assertTrue(script.endswith('main --package headless "$@"\n'))
        self.assertIn("fingerprint='" + FINGERPRINT + "'", script)
        self.assertIn("apt-get install", script)
        for obsolete in ("@APT_INSTALLER@", "@FINGERPRINT@", "@AMD64_SHA256@", "axon-pulse.installing"):
            self.assertNotIn(obsolete, script)

    def test_complete_embedded_installer_has_valid_shell_syntax(self):
        for shell in ("bash", "sh"):
            result = subprocess.run([shell, "-n"], input=render(), text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_help_works_from_legacy_entrypoint_without_network(self):
        result = subprocess.run(["bash", "-s", "--", "--help"], input=render(), text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Usage:", result.stdout)
        self.assertIn("--package", result.stdout)

    def test_forwards_claim_arguments_without_reinterpretation(self):
        args = ["--url", "https://controller.example", "--token", "spt_test_not_real",
                "--channel", "beta", "--download-base-url", "https://mirror.example/pulse/beta"]
        shared = 'main() { printf "%s\\0" "$@" > "$TEST_ARGUMENTS"; }\n'
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "arguments"
            result = subprocess.run(["bash", "-s", "--", *args], input=render(shared),
                                    env=dict(os.environ, TEST_ARGUMENTS=str(target)), text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(target.read_bytes().decode().split("\0")[:-1], ["--package", "headless", *args])
            self.assertEqual(result.stdout + result.stderr, "")

    def test_token_stdin_is_available_to_embedded_main(self):
        shared = 'main() { IFS= read -r token; printf "%s" "$token" > "$TEST_ARGUMENTS"; }\n'
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / "install.sh"
            script.write_text(render(shared))
            target = Path(directory) / "arguments"
            result = subprocess.run(["bash", str(script), "--token-stdin"], input="spt_test_not_real\n",
                                    env=dict(os.environ, TEST_ARGUMENTS=str(target)), text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(target.read_text(), "spt_test_not_real")
            self.assertEqual(result.stdout + result.stderr, "")

    def test_truncated_pipe_does_not_execute_main(self):
        # Before the closing function brace, bash must reject the download
        # without running any of its setup/install/enrollment commands.
        truncated = render().rsplit("}\n", 1)[0]
        result = subprocess.run(["bash", "-s"], input=truncated, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertIn("syntax error", result.stderr)


if __name__ == "__main__":
    unittest.main()
