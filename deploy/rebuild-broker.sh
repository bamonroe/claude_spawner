#!/usr/bin/env bash
# Rebuild the host-side broker binary, then restart its systemd *user* service.
#
# Wired to the app's "restart" button via SPAWNER_BROKER_RESTART_SELF_CMD so that
# rebuilding the server ALSO picks up new broker code. The broker is a separate
# host process from the containerized server: restarting the server alone leaves
# the old broker binary running, which then rejects any newly-added broker op
# (e.g. "unknown op mkdir"). This closes that gap. Safe to run repeatedly.
#
# It runs from the broker's own (minimal) systemd-user environment, so it uses
# the absolute go binary and repo path rather than relying on PATH.
set -euo pipefail

REPO=/data/claude_spawner
GO=/usr/bin/go
OUT="$HOME/.local/bin/spawner-broker"

cd "$REPO"
# systemctl --user needs the user bus; set it in case the service env lacks it.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

# Build the new binary FIRST, while the old broker is still serving; only swap it
# in (via restart) once the build succeeds — a failed build leaves the running
# broker untouched. --no-block: don't wait on the unit that is tearing us down.
# KillMode=process on the unit lets this script (and the detached server rebuild
# launched alongside it) survive the old broker's teardown and finish.
# The Go module is in server/, not the repo root — build with -C so `go` finds it.
"$GO" build -C server -o "$OUT" ./cmd/broker
systemctl --user restart --no-block spawner-broker
