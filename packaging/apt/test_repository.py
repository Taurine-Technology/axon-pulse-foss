#!/usr/bin/env python3
"""Real signed APT integration tests, using only temporary keys/package state."""

import base64
import getpass
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("repository", Path(__file__).with_name("build_repository.py"))
repository = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(repository)


class RepositoryTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.directory = tempfile.TemporaryDirectory()
        cls.root = Path(cls.directory.name)
        cls.gnupg = cls.root / "gnupg"
        cls.gnupg.mkdir(mode=0o700)
        cls.environment = mock.patch.dict(os.environ, {"GNUPGHOME": str(cls.gnupg), "APT_SIGNING_PASSPHRASE": ""})
        cls.environment.start()
        repository.run("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "",
                       "--quick-generate-key", "Pulse test only <test@example.invalid>", "rsa2048", "sign", "1d")
        listing = repository.run("gpg", "--batch", "--with-colons", "--list-secret-keys").decode()
        cls.fingerprint = next(line.split(":")[9] for line in listing.splitlines() if line.startswith("fpr:"))
        cls.edkey = cls.root / "ed25519.pem"
        repository.run("openssl", "genpkey", "-algorithm", "ED25519", "-out", str(cls.edkey))
        public = repository.run("openssl", "pkey", "-in", str(cls.edkey), "-pubout", "-outform", "DER")
        cls.public = base64.b64encode(public[-32:]).decode()

    @classmethod
    def tearDownClass(cls):
        repository.run("gpgconf", "--kill", "gpg-agent")
        cls.environment.stop()
        cls.directory.cleanup()

    def setUp(self):
        self.work = Path(tempfile.mkdtemp(dir=self.root))
        self.payload = self.work / "payload"
        self.payload.mkdir()
        self.index = {"schema_version": 1, "artifacts": []}
        for arch in ("amd64", "arm64"):
            for package in ("axon-pulse", "axon-pulse-desktop"):
                staging = self.work / f"{package}-{arch}"
                (staging / "DEBIAN").mkdir(parents=True)
                (staging / "DEBIAN/control").write_text(
                    f"Package: {package}\nVersion: 1:0.0.8~alpha.1\nArchitecture: {arch}\n"
                    "Maintainer: Test <test@example.invalid>\nDescription: test package\n")
                target = self.payload / f"{package}_0.0.8-alpha.1_linux_{arch}.deb"
                repository.run("dpkg-deb", "--build", "--root-owner-group", str(staging), str(target))
                self.index["artifacts"].append({"os": "linux", "arch": arch, "version": "0.0.8-alpha.1",
                    "filename": target.name, "sha256": repository.sha256(target), "size_bytes": target.stat().st_size})
        self.sign_index()

    def sign_index(self):
        path = self.payload / "index.json"
        path.write_text(json.dumps(self.index))
        signature = repository.run("openssl", "pkeyutl", "-sign", "-rawin", "-inkey", str(self.edkey), "-in", str(path))
        (self.payload / "index.json.sig").write_text(base64.b64encode(signature).decode())

    def build(self, suite="alpha"):
        output = self.work / "repository"
        repository.build(self.payload, output, suite, self.fingerprint, self.public)
        return output

    def test_apt_accepts_signed_repo_and_upgrades_legacy_preview(self):
        root = self.build()
        requests = []

        class Handler(http.server.SimpleHTTPRequestHandler):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, directory=str(root), **kwargs)

            def log_message(self, *_args):
                pass

            def do_GET(self):
                requests.append(self.path)
                return super().do_GET()

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            apt = self.work / "apt"
            for subdir in ("state/lists/partial", "cache/archives/partial", "log", "etc/preferences.d"):
                (apt / subdir).mkdir(parents=True)
            status = apt / "state/status"
            status.write_text("Package: axon-pulse-desktop\nStatus: install ok installed\n"
                              "Priority: optional\nSection: net\nArchitecture: amd64\nVersion: 0.1.0-preview.10\n"
                              "Maintainer: Test <test@example.invalid>\nDescription: legacy\n\n")
            source = apt / "etc/sources.list"
            source.write_text(f"deb [arch=amd64 signed-by={root}/axon-pulse-archive-keyring.gpg] http://127.0.0.1:{server.server_port} alpha main\n")
            options = ["-o", f"Dir={apt}", "-o", f"Dir::State={apt}/state", "-o", f"Dir::State::status={status}",
                       "-o", f"Dir::Cache={apt}/cache", "-o", f"Dir::Log={apt}/log", "-o", f"Dir::Etc={apt}/etc",
                       "-o", "Dir::Etc::sourceparts=-", "-o", "APT::Architecture=amd64",
                       "-o", f"APT::Sandbox::User={getpass.getuser()}", "-o", "Debug::NoLocking=true"]
            repository.run("apt-get", *options, "update", "--error-on=any")
            policy = repository.run("apt-cache", *options, "policy", "axon-pulse-desktop").decode()
            self.assertIn("Candidate: 1:0.0.8~alpha.1", policy)
            simulation = repository.run("apt-get", *options, "--simulate", "install", "axon-pulse-desktop").decode()
            self.assertIn("Inst axon-pulse-desktop [0.1.0-preview.10] (1:0.0.8~alpha.1", simulation)
            self.assertTrue(any("/by-hash/SHA256/" in path for path in requests))
            # Fetch the actual package through APT without installing on the host.
            repository.run("apt-get", *options, "--download-only", "--yes", "install", "axon-pulse-desktop")
            self.assertTrue(any("/pool/main/axon-pulse-desktop/" in path for path in requests))
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_unauthenticated_index_rejected(self):
        (self.payload / "index.json").write_text("{}")
        with self.assertRaises(subprocess.CalledProcessError):
            self.build()

    def test_tampered_package_rejected(self):
        target = self.payload / self.index["artifacts"][0]["filename"]
        with target.open("ab") as stream:
            stream.write(b"tampered")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            self.build()

    def test_signed_path_traversal_rejected(self):
        self.index["artifacts"][0]["filename"] = "../escape.deb"
        self.sign_index()
        with self.assertRaisesRegex(ValueError, "unsafe package"):
            self.build()

    def test_wrong_package_version_rejected(self):
        for artifact in self.index["artifacts"]:
            artifact["version"] = "0.0.9"
        self.sign_index()
        with self.assertRaisesRegex(ValueError, "metadata differs"):
            self.build()

    def test_debian_version_preserves_build_metadata(self):
        self.assertEqual(repository.deb_version("1.2.3-alpha-one+build-two"), "1:1.2.3~alpha-one+build-two")
        self.assertEqual(repository.deb_version("1.2.3+build-two"), "1:1.2.3+build-two")

    def test_subkey_fingerprint_rejected_before_publication(self):
        repository.run("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "",
                       "--quick-add-key", self.fingerprint, "rsa2048", "sign", "1d")
        listing = repository.run("gpg", "--batch", "--with-colons", "--list-secret-keys", self.fingerprint).decode()
        subkey = [line.split(":")[9] for line in listing.splitlines() if line.startswith("fpr:")][-1]
        with self.assertRaisesRegex(ValueError, "primary key"):
            repository.build(self.payload, self.work / "subkey-repo", "alpha", subkey, self.public)
        self.assertFalse((self.work / "subkey-repo/upload.json").exists())
        # Configuring the primary identity remains valid when GnuPG chooses its
        # signing subkey; the generated signature is verified by build().
        self.assertTrue((self.build() / "upload.json").exists())

    def test_promotion_preserves_packages_and_upload_order(self):
        alpha = self.build()
        beta = self.work / "beta"
        repository.build(self.payload, beta, "beta", self.fingerprint, self.public)
        alpha_pool = {p.relative_to(alpha).as_posix(): repository.sha256(p) for p in (alpha / "pool").rglob("*.deb")}
        beta_pool = {p.relative_to(beta).as_posix(): repository.sha256(p) for p in (beta / "pool").rglob("*.deb")}
        self.assertEqual(alpha_pool, beta_pool)
        manifest = json.loads((beta / "upload.json").read_text())
        self.assertEqual(manifest[-1]["path"], "dists/beta/InRelease")
        self.assertIn("Codename: beta", (beta / "dists/beta/InRelease").read_text())
        self.assertIn("Valid-Until:", (beta / "dists/beta/InRelease").read_text())
        script = beta / "install-apt.sh"
        self.assertIn(self.fingerprint, script.read_text())
        repository.run("sh", "-n", str(script))
        repository.run("sh", str(script), "--help")
        result = subprocess.run(["sh", str(script), "--channel", "alpha", "--enable-auto-updates"], capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"only offered for the main stream", result.stderr)

    def test_fetch_authenticates_before_following_artifact_paths(self):
        commands = []
        original = repository.run

        def fake_curl(*args, **kwargs):
            if args[0] != "curl":
                return original(*args, **kwargs)
            commands.append(args)
            target = Path(args[args.index("--output") + 1])
            shutil.copyfile(self.payload / target.name, target)
            return b""

        destination = self.work / "fetched"
        with mock.patch.object(repository, "run", side_effect=fake_curl):
            repository.fetch_payload("https://dist.taurinetech.com/pulse/alpha", destination, self.public)
        self.assertEqual(len(commands), 6)
        self.assertTrue(all("?fetch=" in cmd[-1] for cmd in commands))
        self.assertEqual(len({cmd[-1].split("?fetch=")[1] for cmd in commands}), 1)
        commands.clear()
        (self.payload / "index.json").write_text("{}")
        with mock.patch.object(repository, "run", side_effect=fake_curl):
            with self.assertRaises(subprocess.CalledProcessError):
                repository.fetch_payload("https://dist.taurinetech.com/pulse/alpha", self.work / "untrusted", self.public)
        self.assertEqual(len(commands), 2, "must verify metadata before downloading any artifact")

    def run_bootstrap(self, fingerprint=None, *arguments, stdin=None, overrides=None):
        # Privileged effects are captured under this temporary directory. Real
        # gpg still verifies the downloaded key; no host APT commands are run.
        root = self.build()
        script = root / "install-apt.sh"
        systemd = self.work / "systemd"
        systemd.mkdir()
        script.write_text(script.read_text().replace("/run/systemd/system", str(systemd)))
        if fingerprint:
            script.write_text(script.read_text().replace(self.fingerprint, fingerprint))
        bindir = self.work / "bin"
        bindir.mkdir()
        stub = bindir / "stub"
        stub.write_text("""#!/usr/bin/env python3
import json, os, pathlib, shutil, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
root = pathlib.Path(os.environ['TEST_ROOT'])
if name == 'id':
    print('1000')
elif name == 'dpkg':
    print('amd64')
elif name == 'curl':
    shutil.copyfile(os.environ['TEST_KEY'], args[args.index('--output') + 1])
elif name == 'sudo':
    with (root / 'calls').open('a') as stream:
        stream.write(json.dumps(args) + '\\n')
    if args[0] == 'install' and '-d' not in args:
        dest = root / 'installed' / args[-1].lstrip('/')
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(args[-2], dest)
    elif args[0] == 'apt-get' and os.environ.get('TEST_APT_FAIL') == '1':
        raise SystemExit(100)
    elif args[0] == 'env' and 'status' in args:
        if os.environ.get('TEST_NOT_READY') == '1':
            raise SystemExit(1)
        print(os.environ.get('TEST_STATUS', '{"connected":false,"update_method":"apt"}'))
    elif args[0] == 'env' and 'connect' in args:
        if '--token-stdin' not in args or '--token' in args:
            raise SystemExit(2)
        (root / 'received-token').write_text(sys.stdin.read().strip())
        if os.environ.get('TEST_ENROLL_FAIL') == '1':
            raise SystemExit(1)
elif name == 'sleep':
    pass
else:
    raise SystemExit('unexpected unprivileged command: ' + name)
""")
        stub.chmod(0o755)
        for name in ("sudo", "id", "dpkg", "curl", "apt-get", "systemctl", "timeout", "sleep"):
            (bindir / name).symlink_to(stub)
        env = dict(os.environ, PATH=str(bindir) + os.pathsep + os.environ["PATH"],
                   TEST_ROOT=str(self.work), TEST_KEY=str(root / "axon-pulse-archive-keyring.gpg"))
        env.update(overrides or {})
        return subprocess.run(["sh", str(script), *arguments], env=env, capture_output=True, input=stdin)

    def test_bootstrap_configures_scoped_source_and_explicit_auto_updates(self):
        result = self.run_bootstrap(None, "--enable-auto-updates")
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        root = self.work / "installed/etc/apt"
        source = (root / "sources.list.d/axon-pulse.sources").read_text()
        self.assertIn("Suites: main", source)
        self.assertIn("Signed-By: /etc/apt/keyrings/axon-pulse-archive-keyring.gpg", source)
        self.assertIn("Pin-Priority: -1", (root / "preferences.d/axon-pulse").read_text())
        self.assertIn("codename=main", (root / "apt.conf.d/52unattended-upgrades-axon-pulse").read_text())
        calls = [json.loads(line) for line in (self.work / "calls").read_text().splitlines()]
        self.assertIn(["apt-get", "install", "--yes", "axon-pulse-desktop"], calls)

    def test_bootstrap_key_mismatch_never_invokes_sudo(self):
        result = self.run_bootstrap("0" * 40)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"fingerprint mismatch", result.stderr)
        self.assertFalse((self.work / "calls").exists())

    def test_headless_apt_install_and_pair_uses_stdin_and_system_state(self):
        token = b"spt_test_not_real"
        result = self.run_bootstrap(None, "--package", "headless", "--channel", "beta",
                                    "--url", "https://controller.example", "--token-stdin", stdin=token + b"\n")
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        calls = (self.work / "calls").read_text()
        self.assertIn('["apt-get", "install", "--yes", "axon-pulse"]', calls)
        self.assertIn('["systemctl", "restart", "axon-pulse.service"]', calls)
        self.assertIn('"--state-dir", "/var/lib/axon-pulse"', calls)
        self.assertNotIn('"channel"', calls)
        self.assertEqual((self.work / "received-token").read_bytes(), token)
        self.assertNotIn(token, result.stdout + result.stderr + calls.encode())

    def test_paired_headless_upgrade_never_consumes_new_invite(self):
        result = self.run_bootstrap(None, "--package", "headless", "--url", "https://controller.example",
                                    "--token", "spt_unused", overrides={"TEST_STATUS": '{"connected":true,"update_method":"apt"}'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"already paired", result.stderr)
        self.assertFalse((self.work / "received-token").exists())
        self.assertNotIn("spt_unused", (self.work / "calls").read_text())

    def test_unmanaged_service_does_not_consume_invitation(self):
        result = self.run_bootstrap(None, "--package", "headless", "--url", "https://controller.example",
                                    "--token", "spt_unused", overrides={"TEST_STATUS": '{"connected":false,"update_method":"self"}'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"not APT-managed", result.stderr)
        self.assertFalse((self.work / "received-token").exists())

    def test_apt_failure_does_not_restart_or_enroll(self):
        result = self.run_bootstrap(None, "--package", "headless", "--url", "https://controller.example",
                                    "--token", "spt_unused", overrides={"TEST_APT_FAIL": "1"})
        self.assertNotEqual(result.returncode, 0)
        calls = (self.work / "calls").read_text()
        self.assertNotIn("restart", calls)
        self.assertFalse((self.work / "received-token").exists())

    def test_enrollment_failure_keeps_apt_install_and_reports_failure(self):
        result = self.run_bootstrap(None, "--package", "headless", "--url", "https://controller.example",
                                    "--token", "spt_unused", overrides={"TEST_ENROLL_FAIL": "1"})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"installed, but enrollment failed", result.stderr)
        self.assertTrue((self.work / "installed/etc/apt/sources.list.d/axon-pulse.sources").exists())

    def test_legacy_mirror_option_configures_apt_sibling(self):
        result = self.run_bootstrap(None, "--download-base-url", "https://mirror.example/pulse/beta")
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        source = (self.work / "installed/etc/apt/sources.list.d/axon-pulse.sources").read_text()
        self.assertIn("URIs: https://mirror.example/pulse/apt", source)
        self.assertIn("Suites: beta", source)


if __name__ == "__main__":
    unittest.main()
