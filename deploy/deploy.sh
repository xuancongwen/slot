#!/usr/bin/env bash
# Builds Slot, checks it, and deploys it to an LXC prepared by setup.sh.
#
#   deploy/deploy.sh root@slot.lan      (or set SLOT_HOST)
#
# The database is backed up before each restart, and if the new build does not come
# up healthy, the previous binary is restored.
set -euo pipefail

target=${1:-${SLOT_HOST:-}}
if [[ -z $target ]]; then
  echo "Usage: $0 root@host (or set SLOT_HOST)" >&2
  exit 2
fi
cd "$(dirname "$0")/.."

# One SSH connection for every step, so a password or key prompt happens once.
control="${TMPDIR:-/tmp}/slot-deploy-$$"
ssh_opts=(-o ControlMaster=auto -o "ControlPath=$control" -o ControlPersist=60)
remote() { ssh "${ssh_opts[@]}" "$target" "$@"; }
trap 'ssh "${ssh_opts[@]}" -O exit "$target" 2>/dev/null || true' EXIT

echo "==> Checking $target"
if ! remote 'test -f /etc/systemd/system/slot.service && test -f /etc/slot/slot.env'; then
  echo "$target is not set up. Copy deploy/setup.sh there and run it first." >&2
  exit 1
fi
if remote 'grep -q "example\.com" /etc/slot/slot.env'; then
  echo "Edit /etc/slot/slot.env on $target: it still has the example.com placeholders." >&2
  exit 1
fi
arch=$(remote 'dpkg --print-architecture')
case $arch in
  amd64 | arm64) ;;
  *)
    echo "Unsupported architecture on $target: $arch" >&2
    exit 1
    ;;
esac

echo "==> Running checks"
make check

echo "==> Building for linux/$arch"
binary="bin/slot-linux-$arch"
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags='-s -w' -o "$binary" ./cmd/slot

echo "==> Uploading"
scp -q "${ssh_opts[@]}" "$binary" "$target:/opt/slot/slot.new"

echo "==> Installing and restarting"
remote bash -s <<'EOF'
set -euo pipefail
chmod 0755 /opt/slot/slot.new

# Slot upgrades its schema at startup, so keep a restorable copy first. The key never
# changes, but the backups are useless without it.
if [[ -f /var/lib/slot/slot.db ]]; then
  backup="/var/backups/slot/slot-$(date -u +%Y%m%dT%H%M%SZ).db"
  sqlite3 /var/lib/slot/slot.db ".backup '$backup'"
  cp -p /var/lib/slot/secret.key /var/backups/slot/secret.key
  ls -1t /var/backups/slot/slot-*.db | tail -n +11 | xargs -r rm --
  echo "Backed up the database to $backup"
fi

# Renaming leaves the running process's binary intact until the restart.
if [[ -f /opt/slot/slot ]]; then
  mv -f /opt/slot/slot /opt/slot/slot.prev
fi
mv -f /opt/slot/slot.new /opt/slot/slot
systemctl restart slot

addr=$(sed -n 's/^PUBLIC_ADDR=//p' /etc/slot/slot.env | tail -n 1)
addr=${addr:-:8080}
host=${addr%:*}
case $host in "" | 0.0.0.0 | "[::]") host=127.0.0.1 ;; esac
health="http://$host:${addr##*:}/healthz"
healthy() {
  for _ in $(seq 20); do
    if systemctl is-active --quiet slot && curl -fsS --max-time 2 "$health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

if healthy; then
  echo "Slot is healthy at $health"
  exit 0
fi
echo "The new build did not become healthy. Recent logs:" >&2
journalctl -u slot -n 30 --no-pager >&2 || true
if [[ -f /opt/slot/slot.prev ]]; then
  mv -f /opt/slot/slot.prev /opt/slot/slot
  systemctl restart slot
  if healthy; then
    echo "Rolled back to the previous binary." >&2
  else
    echo "The previous binary is unhealthy too. If the schema was upgraded, restore the latest backup from /var/backups/slot." >&2
  fi
fi
exit 1
EOF
echo "==> Deployed to $target"
