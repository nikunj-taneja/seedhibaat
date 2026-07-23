#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

PYTHONPATH=src python3 -m unittest discover -s tests -v
go test -race -timeout 90s ./...
go vet ./...
python3 tools/privacy_scan.py
git diff --check
