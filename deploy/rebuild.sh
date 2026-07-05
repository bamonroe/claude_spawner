#!/usr/bin/env bash
# Rebuild + relaunch the whole claude_spawner stack, in order:
#   1. resident whisper.cpp servers (transcription backend, docker-compose.yml)
#   2. host-side broker binary + its systemd *user* service
#   3. the containerized server (docker-compose.broker.yml — rebuilds the image)
#
# It needs NO root: the target user is in the `docker` group and the broker is a
# user service. It is written to ALSO work when invoked via `sudo` (see
# deploy/spawner-rebuild.sudoers) — when run as root it re-execs everything as
# $TARGET_USER so docker (group), `go build` (file ownership) and `systemctl
# --user` all behave. Safe to run repeatedly.
set -euo pipefail

REPO=/data/claude_spawner
TARGET_USER=bam

# If invoked as root (via sudo), hand the whole thing back to the ordinary user —
# docker/group, go build, and the systemd *user* service must run as that user.
if [ "$(id -u)" -eq 0 ]; then
  exec runuser -l "$TARGET_USER" -c "$(printf '%q ' "$0" "$@")"
fi

cd "$REPO"
# systemctl --user needs the user bus; set it in case we came through a login shell.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

echo "==> [1/3] resident whisper servers (accurate :8571 + fast :8572)"
docker compose up -d whisper whisper-fast

echo "==> [2/3] broker binary + user service"
go build -o "$HOME/.local/bin/spawner-broker" ./server/cmd/broker
systemctl --user restart spawner-broker

echo "==> [3/3] server container (rebuild image + recreate)"
docker compose -f docker-compose.broker.yml up -d --build spawner

echo "==> done. current state:"
docker compose -f docker-compose.broker.yml ps
docker compose ps whisper whisper-fast
