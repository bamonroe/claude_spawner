# Sandbox image (Arch Linux)

The container image for `sandbox`-target sessions (see the "host vs sandbox" section in the root
[`README.md`](../README.md)). It's **Arch Linux** to match the host, carries a general dev
toolchain, and does **not** bake in either backend — the host's claude/codex binaries and your
credentials are bind-mounted in at run time, so a sandbox always runs the same versions as the host.

## Build

```bash
podman build -t spawner-sandbox:latest -f sandbox/Containerfile sandbox
```

(Rootless Podman is the default runtime; `docker build` works too. Extend it with your own
`FROM spawner-sandbox` layer to add project toolchains.)

## Wire it into the server

Point the spawner at the image and bind-mount claude + auth. These are the values verified by
`internal/session` live tests (`SPAWNER_LIVE=1 go test ./internal/session -run TestLive`).
**Substitute your real home path** — the server does *not* shell-expand `$HOME` in these vars.

```sh
SPAWNER_SANDBOX_IMAGE=spawner-sandbox:latest
SPAWNER_SANDBOX_RUNTIME=podman
# host claude binary bundle + wrapper (read-only) and your credentials (read-write so
# in-sandbox transcripts land in the host ~/.claude and stay discoverable); ~/.codex (read-write)
# brings codex's standalone binary, auth.json, and sessions dir the same way:
SPAWNER_SANDBOX_MOUNTS=/opt/claude-code:/opt/claude-code:ro,/usr/bin/claude:/usr/bin/claude:ro,/home/you/.claude:/home/you/.claude,/home/you/.claude.json:/home/you/.claude.json,/home/you/.codex:/home/you/.codex
# absolute path to the codex binary inside the mounted ~/.codex (host `codex` on ~/.local/bin is
# usually a symlink into this packages dir) — set so it's found regardless of the container's PATH:
SPAWNER_SANDBOX_CODEX_BIN=/home/you/.codex/packages/standalone/current/bin/codex
# rootless: map your host user into the container (claude refuses to run as root) and set HOME to
# the mounted credentials. `podman exec` for each turn inherits this user + HOME.
SPAWNER_SANDBOX_RUN_ARGS=--userns=keep-id -e HOME=/home/you
```

Why these:

- **claude isn't baked in.** `/opt/claude-code` is the Arch `claude-code` package's bundle and
  `/usr/bin/claude` its wrapper; mounting both keeps the sandbox's claude version-locked to the
  host with no image rebuild on updates.
- **codex isn't baked in either.** `~/.codex` holds codex's self-contained standalone binary (under
  `packages/…`), `auth.json`, and the `sessions/` rollout dir. Mounting it read-write gives the
  sandbox the host's exact codex + login, and its transcripts land back in the host `~/.codex` where
  the server reads them. `SPAWNER_SANDBOX_CODEX_BIN` points at the resolved binary inside that mount.
- **`--userns=keep-id`.** Claude refuses `--dangerously-skip-permissions` as root, and rootless
  Podman would otherwise map the container user to a subuid that can't write your host-owned
  `~/.claude`. keep-id maps your host UID straight through, so the turn runs non-root *and* can
  write the mounted credentials/transcripts.
- **Same-path project mount.** The executor bind-mounts the session's working directory at the
  same absolute path (and `-w`s into it), so claude's on-disk transcript is keyed the same as on
  the host and history/discovery keep working.

The container is **persistent for the session's lifetime**: created at spawn (`run -d … sleep
infinity`), each turn `exec`s into it, and it's removed on delete. Orphans (a session deleted while
the server was down) are swept at startup. See [`docs/architecture.md`](../docs/architecture.md).

The server reads these `SPAWNER_SANDBOX_*` vars and drives the rootless runtime on the host over SSH
(the same connection host turns use), so there's no broker in between. `SPAWNER_SANDBOX_IMAGE` must
be set (to any value) for the spawn dialog to offer the sandbox target.
