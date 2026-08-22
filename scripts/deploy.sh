#!/bin/sh
set -eu
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/bare ./cmd/bare
scp /tmp/bare xmatic:/tmp/bare
ssh xmatic 'sudo install -m 0755 -o root -g root /tmp/bare /opt/bare/bare && sudo systemctl restart bare && sleep 1 && curl -fsS http://127.0.0.1:8411/healthz'
