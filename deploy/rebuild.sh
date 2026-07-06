#!/usr/bin/env bash
# Rebuild + relaunch the claude_spawner stack, in order:
#   1. resident whisper.cpp servers (transcription backend, docker-compose.yml)
#   2. the server binary + its systemd *user* service
#
# It is also wired to the app's "restart" button via SPAWNER_RESTART_CMD: the
# server fires it detached, it rebuilds the binary, then restarts the unit with
# --no-block. The build happens FIRST (while the old server keeps serving); the
# unit is only restarted once the build succeeds, so a failed build leaves the
# running server untouched. KillMode=process on the unit lets this script survive
# the old server's teardown and finish the swap.
#
# It needs NO root: the target user is in the `docker` group and the server is a
# user service. It also works if invoked via `sudo` — when run as root it re-execs
# as $TARGET_USER so docker (group), `go build` (file ownership) and
# `systemctl --user` all behave. Safe to run repeatedly.
set -euo pipefail

REPO=/data/claude_spawner
TARGET_USER=bam
GO=/usr/bin/go
OUT="$HOME/.local/bin/spawner-server"

# If invoked as root (via sudo), hand the whole thing back to the ordinary user —
# docker/group, go build, and the systemd *user* service must run as that user.
if [ "$(id -u)" -eq 0 ]; then
  exec runuser -l "$TARGET_USER" -c "$(printf '%q ' "$0" "$@")"
fi

cd "$REPO"
# systemctl --user needs the user bus; set it in case we came through a login
# shell or the minimal systemd-user environment.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

echo "==> [1/2] resident whisper servers (accurate :8571 + fast :8572)"
docker compose up -d whisper whisper-fast

echo "==> [2/2] server binary + user service"
# Build FIRST, while the old server is still serving; only swap it in (via
# restart) once the build succeeds. The Go module is in server/, not the repo
# root — build with -C so `go` finds it. --no-block: don't wait on the unit that
# is tearing us down (KillMode=process lets this detached rebuild survive it).
"$GO" build -C server -o "$OUT" .
systemctl --user restart --no-block spawner-server

echo "==> done."
