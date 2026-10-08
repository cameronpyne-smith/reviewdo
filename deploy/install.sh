#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

go build -trimpath -ldflags='-s -w' -o reviewdo .
sudo install -m 0755 reviewdo /usr/local/bin/reviewdo
sudo install -d -m 0755 /etc/reviewdo
if [ ! -f /etc/reviewdo/config.json ]; then
  sudo install -m 0644 deploy/config.example.json /etc/reviewdo/config.json
  echo "installed default config at /etc/reviewdo/config.json, edit it before starting"
fi
if [ ! -f /etc/reviewdo/private-key.pem ]; then
  echo "copy the GitHub App private key to /etc/reviewdo/private-key.pem:"
  echo "  sudo install -m 0600 -o root -g root ~/.config/reviewdo/private-key.pem /etc/reviewdo/private-key.pem"
fi
sudo install -m 0644 deploy/reviewdo.service /etc/systemd/system/reviewdo.service
sudo systemctl daemon-reload
echo "then: sudo systemctl enable --now reviewdo && journalctl -fu reviewdo"
