#!/usr/bin/env bash
# Prepares a fresh Debian 13 LXC to run Slot. Copy it to the container and run it as root:
#
#   scp deploy/setup.sh root@slot.lan: && ssh root@slot.lan ./setup.sh
#
# Safe to rerun: it never overwrites /etc/slot/slot.env or anything under /var/lib/slot.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "Run setup.sh as root." >&2
  exit 1
fi
. /etc/os-release
if [[ ${ID:-} != debian || ${VERSION_ID:-} != 13 ]]; then
  echo "Expected Debian 13, found ${PRETTY_NAME:-an unknown system}." >&2
  exit 1
fi

echo "==> Installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update
# Keep existing config files rather than stopping to ask about them.
apt-get -y -o Dpkg::Options::=--force-confold upgrade
# ca-certificates: TLS to Google. curl: the deploy health check. sqlite3: pre-deploy backups.
apt-get -y install --no-install-recommends ca-certificates curl sqlite3

echo "==> Creating directories"
install -d -m 0755 /opt/slot /etc/slot
install -d -m 0700 /var/lib/slot /var/backups/slot

if [[ -e /etc/slot/slot.env ]]; then
  echo "==> Keeping existing /etc/slot/slot.env"
else
  echo "==> Writing /etc/slot/slot.env"
  install -m 0600 /dev/null /etc/slot/slot.env
  cat >/etc/slot/slot.env <<'EOF'
# Slot configuration. After editing: systemctl restart slot
# deploy.sh refuses to run while example.com is still in this file.

# Origins people will use in their browser. They must differ and have no path.
PUBLIC_URL=https://book.example.com
ADMIN_URL=https://slot-admin.example.com

# Listen on every interface so a reverse proxy on another machine can reach Slot.
# Keep the admin port (8081) reachable only from your LAN or VPN.
PUBLIC_ADDR=:8080
ADMIN_ADDR=:8081

# Google OAuth "Web application" client. Its redirect URI is ADMIN_URL/oauth/callback.
GOOGLE_CLIENT_ID=
GOOGLE_CLIENT_SECRET=

# Host registration on the admin port.
REGISTRATION_OPEN=true
REGISTRATION_CODE=

# URL name of the only host, e.g. sam: their page is then the public root, and
# registration accepts that name once. Requires REGISTRATION_CODE.
SINGLE_HOST=
EOF
fi

echo "==> Installing slot.service"
cat >/etc/systemd/system/slot.service <<'EOF'
[Unit]
Description=Slot booking server
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/opt/slot/slot
EnvironmentFile=/etc/slot/slot.env
Environment=DATA_DIR=/var/lib/slot
Restart=on-failure
RestartSec=5
# Slot drains requests and the sync worker for up to 20 seconds on shutdown.
TimeoutStopSec=30
UMask=0077

# Slot runs as root, so the sandbox limits what root can reach: the data directory
# is the only writable path and no capabilities are kept. These need the LXC
# "nesting" feature; without it systemd fails the start with status 226/NAMESPACE.
CapabilityBoundingSet=
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/slot
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable slot.service

cat <<'EOF'

Setup complete. Next:
  1. Edit /etc/slot/slot.env: your origins and Google OAuth client.
  2. From your checkout, run: deploy/deploy.sh root@<this container>
EOF
