#!/usr/bin/env python3
from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SKIP = {".env"}
PATTERNS = {
    "Meta access token": re.compile(r"\bEA[A-Za-z0-9]{80,}\b"),
    "Shopify client secret": re.compile(r"\bshpss_[A-Za-z0-9]{20,}\b"),
    "Shopify access token": re.compile(r"\bshpat_[A-Za-z0-9]{20,}\b"),
    "private key": re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
    "non-placeholder secret assignment": re.compile(
        r"^(?:META_APP_SECRET|META_SYSTEM_USER_TOKEN|SHOPIFY_CLIENT_SECRET|SEEDHIBAAT_[A-Z_]*(?:KEY|TOKEN))=(?!\s*$|your_|replace_).+",
        re.MULTILINE,
    ),
    "local user path": re.compile("/" + r"Users/[^/\s]+/"),
}
HISTORY_PATTERNS = {
    key: pattern
    for key, pattern in PATTERNS.items()
    if key in {"Meta access token", "Shopify client secret", "private key"}
}


def files() -> list[Path]:
    output = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard"],
        cwd=ROOT,
        text=True,
    )
    return [
        ROOT / line
        for line in output.splitlines()
        if Path(line).name not in SKIP and not line.startswith(".git/")
    ]


def scan_git_history() -> list[str]:
    """Report secret-bearing paths in every reachable commit without printing secrets."""
    try:
        revisions = subprocess.check_output(
            ["git", "rev-list", "--all"], cwd=ROOT, text=True
        ).splitlines()
    except subprocess.CalledProcessError:
        return []

    findings: list[str] = []
    for revision in revisions:
        paths = subprocess.check_output(
            ["git", "ls-tree", "-r", "--name-only", revision], cwd=ROOT, text=True
        ).splitlines()
        for relative in paths:
            if Path(relative).name in SKIP:
                continue
            result = subprocess.run(
                ["git", "show", f"{revision}:{relative}"],
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                check=False,
            )
            if result.returncode != 0:
                continue
            try:
                body = result.stdout.decode("utf-8")
            except UnicodeDecodeError:
                continue
            for label, pattern in HISTORY_PATTERNS.items():
                if pattern.search(body):
                    findings.append(f"git:{revision[:12]}:{relative}: {label}")
    return findings


def main() -> int:
    findings: list[str] = []
    for path in files():
        try:
            body = path.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError):
            continue
        relative = path.relative_to(ROOT)
        for label, pattern in PATTERNS.items():
            for match in pattern.finditer(body):
                line = body.count("\n", 0, match.start()) + 1
                findings.append(f"{relative}:{line}: {label}")
    findings.extend(scan_git_history())
    if findings:
        print("privacy scan failed:", file=sys.stderr)
        print("\n".join(findings), file=sys.stderr)
        return 1
    print("privacy scan passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
