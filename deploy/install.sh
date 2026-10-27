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
if [ ! -f /etc/tmpfiles.d/ollama-turn-lock.conf ]; then
  printf 'd /run/lock/ollama 0755 root root -\n' | sudo tee /etc/tmpfiles.d/ollama-turn-lock.conf >/dev/null
  sudo systemd-tmpfiles --create /etc/tmpfiles.d/ollama-turn-lock.conf
  echo "created /run/lock/ollama; set \"lock\": \"/run/lock/ollama\" in the config to take turns on the GPU with other Ollama users"
fi
sudo install -m 0644 deploy/reviewdo.service /etc/systemd/system/reviewdo.service
sudo systemctl daemon-reload
echo "then: sudo systemctl enable --now reviewdo && journalctl -fu reviewdo"
