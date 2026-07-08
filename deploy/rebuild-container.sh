#!/usr/bin/env bash
# Rebuild + recreate the CONTAINERIZED claude_spawner server
# (deploy/spawner-container.yml). This is what the app's restart button runs for the
# container deployment: unlike the bare-metal button (a pure `systemctl` bounce), it
# rebuilds the image from current source and recreates the container, so one tap ships
# new server code.
#
# It MUST run on the HOST, not inside the container — `compose up --build` recreates
# the container and would kill anything running inside it (including this script). The
# restart command SSHes into the host (localhost) and launches this DETACHED (setsid),
# so it survives the very container it replaces. The build happens as part of
# `up --build`; if it fails, compose leaves the running container in place. Safe to run
# repeatedly.
set -euo pipefail

REPO=/data/claude_spawner
TARGET_USER=bam

# If invoked as root, hand back to the ordinary user — docker (group membership) and
# the mounted state are owned by that user.
if [ "$(id -u)" -eq 0 ]; then
  exec runuser -l "$TARGET_USER" -c "$(printf '%q ' "$0" "$@")"
fi

cd "$REPO"
# SPAWNER_UID/GID (not UID/GID — readonly in some shells) let the container run as the
# host user so it can read/write the mounted home, state, and roots.
export SPAWNER_UID="$(id -u)" SPAWNER_GID="$(id -g)"

echo "==> [1/2] resident whisper servers (accurate :8571 + fast :8572)"
docker compose up -d whisper whisper-fast

echo "==> [2/2] rebuild image + recreate the server container"
docker compose -f deploy/spawner-container.yml up -d --build

echo "==> done."
