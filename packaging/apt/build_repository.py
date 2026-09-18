#!/usr/bin/env python3
"""Build a signed, by-hash APT suite from an authenticated Pulse release.

Only standard-library Python, openssl, dpkg-deb and GnuPG are required. The
output is a fresh upload tree, not a mirror: publishers retain old pool and
by-hash objects so concurrent APT readers can finish an older transaction.
"""

import argparse
import base64
import datetime as dt
import email.utils
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time


def run(*args, **kwargs):
    return subprocess.run(args, check=True, stdout=subprocess.PIPE, **kwargs).stdout


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def deb_version(version):
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?", version):
        raise ValueError("invalid release version")
    return "1:" + re.sub(r"^(\d+\.\d+\.\d+)-", r"\1~", version)


def verify_index(payload, public_key):
    key = base64.b64decode(public_key, validate=True)
    signature = base64.b64decode((payload / "index.json.sig").read_text().strip(), validate=True)
    if len(key) != 32 or len(signature) != 64:
        raise ValueError("invalid Ed25519 key or signature size")
    with tempfile.TemporaryDirectory() as directory:
        work = Path(directory)
        # RFC 8410 SubjectPublicKeyInfo for an Ed25519 public key.
        (work / "key.der").write_bytes(bytes.fromhex("302a300506032b6570032100") + key)
        (work / "signature").write_bytes(signature)
        run("openssl", "pkeyutl", "-verify", "-pubin", "-keyform", "DER",
            "-inkey", str(work / "key.der"), "-rawin", "-in", str(payload / "index.json"),
            "-sigfile", str(work / "signature"))
    index = json.loads((payload / "index.json").read_bytes())
    if index.get("schema_version") != 1:
        raise ValueError("unsupported release index schema")
    return index


def fetch_payload(base, payload, public_key):
    if not re.fullmatch(r"https://dist\.taurinetech\.com/pulse/(alpha|beta|main)", base):
        raise ValueError("expected an official Pulse channel URL")
    payload.mkdir(parents=True, exist_ok=False)
    cache_buster = str(time.time_ns())

    def fetch(name, limit):
        run("curl", "--proto", "=https", "--proto-redir", "=https", "--fail",
            "--silent", "--show-error", "--location", "--connect-timeout", "15",
            "--max-time", "300", "--max-filesize", str(limit),
            "--output", str(payload / name), base + "/" + name + "?fetch=" + cache_buster)

    fetch("index.json", 2 * 1024 * 1024)
    fetch("index.json.sig", 1024)
    index = verify_index(payload, public_key)
    # Authenticate metadata before interpreting filenames or following downloads.
    # Only fetch the Debian packages; weekly metadata refreshes need no GUI
    # installers or headless archives from other platforms.
    for artifact in index["artifacts"]:
        name = artifact["filename"]
        if artifact["os"] == "linux" and name.endswith(".deb"):
            if not re.fullmatch(r"[A-Za-z0-9_.+-]+\.deb", name):
                raise ValueError("unsafe package filename")
            if not isinstance(artifact["size_bytes"], int) or artifact["size_bytes"] <= 0:
                raise ValueError("invalid package size")
            fetch(name, artifact["size_bytes"])


def build(payload, output, suite, fingerprint, public_key):
    if suite not in ("alpha", "beta", "main"):
        raise ValueError("invalid suite")
    if not re.fullmatch(r"[A-F0-9]{40}|[A-F0-9]{64}", fingerprint):
        raise ValueError("use the full uppercase APT signing-key fingerprint")
    index = verify_index(payload, public_key)
    artifacts = [a for a in index["artifacts"] if a["os"] == "linux" and a["filename"].endswith(".deb")]
    if len(artifacts) != 4 or len({a["version"] for a in artifacts}) != 1:
        raise ValueError("expected desktop and headless packages for both architectures at one version")
    version = deb_version(artifacts[0]["version"])
    if output.exists():
        raise ValueError("output must not exist; use a fresh staging directory")
    output.mkdir(parents=True)
    packages = {"amd64": [], "arm64": []}
    seen = set()
    for artifact in artifacts:
        name = artifact["filename"]
        if not re.fullmatch(r"[A-Za-z0-9_.+-]+\.deb", name):
            raise ValueError("unsafe package filename")
        source = payload / name
        digest = sha256(source)
        if source.stat().st_size != artifact["size_bytes"] or digest != artifact["sha256"]:
            raise ValueError(f"package size/checksum mismatch: {name}")
        control = run("dpkg-deb", "--field", str(source)).decode()
        fields = dict(line.split(": ", 1) for line in control.splitlines() if line and not line[0].isspace() and ": " in line)
        package, arch = fields["Package"], fields["Architecture"]
        if package not in ("axon-pulse", "axon-pulse-desktop") or arch not in packages:
            raise ValueError("unexpected package name/architecture")
        if fields["Version"] != version or arch != artifact["arch"] or (package, arch) in seen:
            raise ValueError("package metadata differs from the release or is duplicated")
        seen.add((package, arch))
        relative = f"pool/main/{package}/{digest}/{name}"
        target = output / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
        packages[arch].append(control.rstrip() + f"\nFilename: {relative}\nSize: {source.stat().st_size}\nSHA256: {digest}\n\n")

    release_dir = output / "dists" / suite
    indexed = []
    for arch, entries in packages.items():
        directory = release_dir / "main" / f"binary-{arch}"
        directory.mkdir(parents=True)
        data = "".join(sorted(entries)).encode()
        for name, content in (("Packages", data), ("Packages.gz", gzip.compress(data, mtime=0))):
            path = directory / name
            path.write_bytes(content)
            indexed.append(path)
            by_hash = directory / "by-hash" / "SHA256" / sha256(path)
            by_hash.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, by_hash)

    now = dt.datetime.now(dt.timezone.utc)
    release = (f"Origin: Taurine Technology\nLabel: Axon Pulse\nSuite: {suite}\nCodename: {suite}\n"
               f"Version: {version}\nDate: {email.utils.format_datetime(now, usegmt=True)}\n"
               f"Valid-Until: {email.utils.format_datetime(now + dt.timedelta(days=30), usegmt=True)}\n"
               "Architectures: amd64 arm64\nComponents: main\nAcquire-By-Hash: yes\n"
               "Description: Axon Pulse for Ubuntu and Debian\nSHA256:\n")
    for path in indexed:
        release += f" {sha256(path)} {path.stat().st_size} {path.relative_to(release_dir)}\n"
    with tempfile.TemporaryDirectory() as directory:
        unsigned = Path(directory) / "Release"
        unsigned.write_text(release)
        # Pin the primary key identity; GnuPG may sign with its valid signing
        # subkey. APT verifies that signature against the exported primary key.
        run("gpg", "--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase-fd", "0",
            "--local-user", fingerprint, "--digest-algo", "SHA256", "--clearsign",
            "--output", str(release_dir / "InRelease"), str(unsigned),
            input=os.environ.get("APT_SIGNING_PASSPHRASE", "").encode())
    keyring = output / "axon-pulse-archive-keyring.gpg"
    keyring.write_bytes(run("gpg", "--batch", "--export-options", "export-minimal", "--export", fingerprint))
    if not keyring.stat().st_size:
        raise ValueError("APT public key export failed")
    listing = run("gpg", "--batch", "--show-keys", "--with-colons", str(keyring)).decode()
    primary, fingerprints = False, []
    for line in listing.splitlines():
        fields = line.split(":")
        if fields[0] == "pub":
            primary = True
        elif fields[0] == "fpr" and primary:
            fingerprints.append(fields[9])
            primary = False
    if fingerprints != [fingerprint]:
        raise ValueError("APT signing fingerprint must identify exactly one primary key")
    run("gpgv", "--keyring", str(keyring.resolve()), str(release_dir / "InRelease"))
    template = Path(__file__).with_name("install-apt.sh.in").read_text()
    (output / "install-apt.sh").write_text(template.replace("@FINGERPRINT@", fingerprint))
    # Upload order is explicit: only the final, single InRelease object changes
    # the authenticated view. All files it references have already been uploaded.
    paths = sorted(p for p in output.rglob("*") if p.is_file() and p.name != "InRelease")
    paths.append(release_dir / "InRelease")
    manifest = [{"path": p.relative_to(output).as_posix(), "size": p.stat().st_size, "sha256": sha256(p)} for p in paths]
    (output / "upload.json").write_text(json.dumps(manifest, indent=2) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--payload", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--suite", choices=("alpha", "beta", "main"), required=True)
    parser.add_argument("--fingerprint", required=True)
    parser.add_argument("--update-public-key", required=True)
    parser.add_argument("--fetch-url", help="authenticate and download Debian payload from this channel first")
    args = parser.parse_args()
    if args.fetch_url:
        fetch_payload(args.fetch_url, args.payload, args.update_public_key)
    build(args.payload, args.output, args.suite, args.fingerprint, args.update_public_key)
