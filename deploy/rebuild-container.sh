#!/usr/bin/env bash
# Rebuild + recreate the CONTAINERIZED claude_spawner server (the spawner-server
# service in the root docker-compose.yml). This is what the app's restart button runs:
# it rebuilds the image from current source and recreates the container, so one tap
# ships new server code. The whisper service is left untouched.
#
# It MUST run on the HOST, not inside the container — recreating the container would
# kill anything running inside it (including this script). The restart command SSHes
# into the host (localhost) and launches this DETACHED (setsid), so it survives the very
# container it replaces. It builds the image (--no-cache, so a fresh compile every time)
# and only then recreates the container; if the build fails, `set -e` aborts before the
# recreate, leaving the running container in place. Safe to run repeatedly.
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

# Mode from the restart button: `bounce` recreates from the existing image (fast, ships
# no code changes); anything else (default, incl. the voice command) does a full
# --no-cache rebuild. The server substitutes this arg into SPAWNER_RESTART_CMD's
# %REBUILD% token.
MODE="${1:-rebuild}"

if [ "$MODE" = "bounce" ]; then
  echo "==> bounce: recreate the server container from the existing image (no rebuild)"
else
  echo "==> rebuild image + recreate the server container (whisper left as-is)"
  # Stage the pre-built web bundle into the image build context so it bakes into the
  # image (served at SPAWNER_WEB_DIR=/srv/web) — no host mount. We do NOT run Gradle
  # here: the bundle is built out-of-band by `:app:wasmJsBrowserDistribution` when the
  # UI changes; a rebuild just packages whatever bundle is currently built. Preserve
  # the tracked .gitkeep so the (gitignored) dir always exists for the Dockerfile COPY.
  WEB_SRC="$REPO/android/app/build/dist/wasmJs/productionExecutable"
  mkdir -p server/webdist
  find server/webdist -mindepth 1 ! -name .gitkeep -delete
  if [ -d "$WEB_SRC" ] && [ -n "$(ls -A "$WEB_SRC" 2>/dev/null)" ]; then
    cp -a "$WEB_SRC"/. server/webdist/
    echo "==> staged web bundle from $WEB_SRC"
  else
    echo "==> no web bundle at $WEB_SRC — image will serve no web client"
  fi
  # --no-cache: force a fresh compile every time. `up --build` alone trusts Docker's
  # layer cache to notice changed source, but it has silently reused a stale build layer
  # and shipped an old binary in a fresh container — so the restart button appeared to do
  # nothing. A full no-cache build is slower but guarantees the running server is the
  # current code, which is the whole point of the button.
  docker compose build --no-cache spawner-server
fi

# Force-remove any container already holding the fixed name `spawner-server` before the
# recreate. A container left over from a DIFFERENT compose project name (e.g. one brought
# up from the deploy/ dir → project `deploy`, vs. this repo-root project `claude_spawner`)
# collides on the fixed container_name: `up` then fails with a name conflict and silently
# leaves the STALE container running — which is exactly why the restart button once looked
# like a no-op. Removing it first makes the recreate deterministic and re-homes the
# container under this project so future restarts recreate normally. Safe: this script is
# setsid-detached on the host, so killing the container doesn't kill the rebuild.
docker rm -f spawner-server 2>/dev/null || true
docker compose up -d spawner-server

echo "==> done."
