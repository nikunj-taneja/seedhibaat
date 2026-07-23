#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
go_bin="${GO_BIN:-go}"
target="${SEEDHIBAAT_SSH_TARGET:-vps}"
workflow_source="${SEEDHIBAAT_WORKFLOW_SOURCE:-config/workflows}"
media_source="${SEEDHIBAAT_MEDIA_SOURCE:-}"
release_dir="$(mktemp -d /tmp/seedhibaat-release.XXXXXX)"
trap 'rm -rf "$release_dir"' EXIT

cd "$repo_root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$go_bin" build \
  -trimpath -ldflags="-s -w" -o "$release_dir/seedhibaatd" ./cmd/seedhibaatd

ssh "$target" "id seedhibaat >/dev/null 2>&1 || useradd --system --home /var/lib/seedhibaat --shell /usr/sbin/nologin seedhibaat; install -d -o root -g root -m 0755 /opt/seedhibaat /opt/seedhibaat/bin /opt/seedhibaat/config /opt/seedhibaat/config/workflows /opt/seedhibaat/media; install -d -o seedhibaat -g seedhibaat -m 0700 /var/lib/seedhibaat /var/backups/seedhibaat; install -d -o root -g root -m 0700 /etc/seedhibaat"
scp "$release_dir/seedhibaatd" "$target:/tmp/seedhibaatd.new"
scp deploy/seedhibaat.service deploy/seedhibaat-backup.service deploy/seedhibaat-backup.timer "$target:/tmp/"
scp "$workflow_source"/*.yaml "$target:/tmp/"
ssh "$target" "install -o root -g root -m 0755 /tmp/seedhibaatd.new /opt/seedhibaat/bin/seedhibaatd; install -o root -g root -m 0644 /tmp/seedhibaat.service /etc/systemd/system/seedhibaat.service; install -o root -g root -m 0644 /tmp/seedhibaat-backup.service /etc/systemd/system/seedhibaat-backup.service; install -o root -g root -m 0644 /tmp/seedhibaat-backup.timer /etc/systemd/system/seedhibaat-backup.timer; install -o root -g root -m 0644 /tmp/*.yaml /opt/seedhibaat/config/workflows/; systemctl daemon-reload; cd /opt/seedhibaat && /opt/seedhibaat/bin/seedhibaatd validate-workflows"

if [[ -n "$media_source" ]]; then
  install -d "$release_dir/media"
  find "$media_source" -maxdepth 1 -type f \
    \( -name '*.jpg' -o -name '*.jpeg' -o -name '*.png' \) \
    -exec cp {} "$release_dir/media/" \;
  if compgen -G "$release_dir/media/*" >/dev/null; then
    remote_media_dir="/tmp/seedhibaat-media-upload-$$"
    ssh "$target" "install -d -o root -g root -m 0700 '$remote_media_dir'"
    scp "$release_dir"/media/* "$target:$remote_media_dir/"
    ssh "$target" "for media in '$remote_media_dir'/*; do install -o root -g root -m 0644 \"\$media\" /opt/seedhibaat/media/; done; rm -rf '$remote_media_dir'"
  fi
fi

echo "Release installed but not started. Provision /etc/seedhibaat/seedhibaat.env, then start explicitly."
