# Host deployment

The server runs in a **Docker container** that builds the Go binary from source — this is the one
supported deployment. It runs as your ordinary user (never host root) and drives the host over
**SSH** (`SPAWNER_SSH=1`): `claude` for host turns and rootless Podman for sandbox turns both run
**on the host**, over the same SSH connection, so the container needs no host root and no separate
broker. Transcription is a second service in the **same** compose stack. Both are defined in the
root [`docker-compose.yml`](../docker-compose.yml), so **one command launches the whole backend**.
This directory holds the server's env template, the rebuild script, and a transcript helper.

| File                        | What it is                                                                 |
|-----------------------------|----------------------------------------------------------------------------|
| `../docker-compose.yml`     | The whole stack: the `spawner-server` gateway (builds the binary; drives the host over SSH) + the `whisper` transcription server. |
| `spawner-container.env.example`| template for the server's env file (token, addr, root, SSH key/known_hosts, whisper, restart, sandbox). |
| `rebuild-container.sh`      | host-side rebuild + recreate of the `spawner-server` container; the restart button runs this over SSH.       |
| `claude-log.sh`             | helper to read a session's Claude transcript by name.                     |

## The whisper transcription service

The `whisper` service is the resident whisper.cpp HTTP server (on `:8571`) the gateway transcribes
through. It carries `restart: unless-stopped`, so once created it survives reboots, but a
`docker compose down` removes it and voice goes silent until the next `up`. An optional second "fast"
draft model on `:8572` (`whisper-fast`) can offload the live hands-free draft — start it and set
`SPAWNER_WHISPER_FAST_URL` to enable it. See [`../whisper/README.md`](../whisper/README.md).

## Running the whole stack — one command

The `spawner-server` service uses **host networking** (so `localhost:22` is the host's own sshd and
an empty `Session.Host` drives the host; `localhost:8571` reaches the whisper service) and mounts the
user's home + project roots at the **same paths** the host uses (so the server browses and reads
transcripts where the host writes them; `claude` runs on the host over SSH). Its config vars are
documented in [`../CLAUDE.md`](../CLAUDE.md) (the config section — the authoritative list), templated
in `spawner-container.env.example`.

The server comes up **bare** — it mints its own SSH identity and seeds its own trust set, so there is
nothing to place by hand up front:

```bash
cp deploy/spawner-container.env.example deploy/spawner-container.env   # edit the token
mkdir -p deploy/state
# Run the server as you (so it can read/write the mounts). Put these in the git-ignored
# root .env once (cp .env.example .env; set your uid/gid) and drop the prefix thereafter:
SPAWNER_UID=$(id -u) SPAWNER_GID=$(id -g) docker compose up -d --build
```

That single command builds the Go binary, starts the gateway, and brings up the whisper server.
(Text-only / no GPU: `docker compose up -d --build spawner-server` runs just the gateway.)

### Enabling the restart button (and loopback host turns)

The server drives the host over SSH, so it needs a key the host accepts. It **owns its own keypair**,
separate from the host's `~/.ssh` keys: on first boot it generates one at `deploy/state/ssh/id_ed25519`
(`0600`, persisted on the volume) and writes the public key to `id_ed25519.pub` (also logged at
startup). To let the container SSH into the machine it runs on — for host turns and, crucially, the
**restart button** — add that public key to the login user's `~/.ssh/authorized_keys` on the host:

```bash
cat deploy/state/ssh/id_ed25519.pub >> ~/.ssh/authorized_keys
```

Host keys are verified against the server's **own** known_hosts (`SPAWNER_SSH_KNOWN_HOSTS=/state/known_hosts`,
independent of the host's `~/.ssh/known_hosts`), which the server **auto-seeds**: the loopback host
(the local machine actually running the container) is trusted on first boot, and each host you add in
the app is trusted when you save it. No manual `ssh-keyscan` step. `SPAWNER_SSH_KEY` can override the
key path if you'd rather supply your own; leave it empty to let the server self-manage.

The `deploy/spawner-container.env` (it holds the token), the root `.env`, and `deploy/state/` are git-ignored — keep
the token out of the repo. Point a client at its port (`SPAWNER_ADDR`, e.g. `:8098`) to exercise it.
Verified end to end: a turn dictated through the container runs `claude` on the host over SSH and
streams the reply back. (Transcription needs the resident whisper servers if you want the voice
path; text turns work without them.)

## The restart button rebuilds

For the container the app's **restart** button is a *one-tap deploy*, not just a bounce:
`SPAWNER_RESTART_CMD` SSHes to the host over loopback and launches
[`rebuild-container.sh`](rebuild-container.sh) detached (`setsid`), which runs
`compose build --no-cache spawner-server` then `compose up -d spawner-server` to rebuild the image
from current source and recreate the gateway container (the whisper service is left untouched). The
build is deliberately `--no-cache`: `up --build` alone once reused a stale layer and shipped an old
binary in a fresh container, so the button appeared to do nothing — a full recompile guarantees the
running server is the current code.

**Rebuild is optional.** The button has a *Rebuild from source* checkbox (default on). The server
substitutes the `%REBUILD%` token in `SPAWNER_RESTART_CMD` with `rebuild` or `bounce` and passes it
to the script as its first arg: `rebuild` (default) does the `--no-cache` recompile above; `bounce`
skips the build and just recreates from the existing image — a fast restart that ships no code
change. A command without a `%REBUILD%` token always rebuilds (older configs). It **must** run on the host —
recreating the container replaces the very container the server lives in, so an in-container command would be
killed mid-recreate; `setsid` over SSH decouples it so it survives. The image ships `openssh-client`
for exactly this, and the compose file mounts the host `/etc/passwd` read-only — without a passwd
entry the openssh client aborts with *"No user exists for uid"* (the container runs as a bare uid;
Go's SSH for turns doesn't care, but the CLI does). **Bootstrap:** the running container must already
have been built from this Dockerfile (with `openssh-client`), have `SPAWNER_RESTART_CMD` in its env,
and have the `/etc/passwd` mount, so do the manual `up -d --build` above first; after that the button
self-serves. A rebuild started this way drops any in-flight turn as the container is recreated.

### Testing a build without touching the live server

Rebuilding/recreating the live container drops any in-flight turn (including one you're driving over
voice). To test a server change safely, build and run the freshly-built binary by hand on a scratch
port with a separate session store, leaving the live container running:

```bash
go build -C server -o /tmp/spawner-dev .
SPAWNER_TOKEN=devsecret SPAWNER_ADDR=:8557 \
  SPAWNER_STATE=$HOME/.local/share/claude_spawner_dev/sessions.json \
  SPAWNER_ROOT=/data:$HOME \
  SPAWNER_WHISPER_URL=http://localhost:8571 \
  /tmp/spawner-dev
```

Point the app at `:8557` to exercise it; promote by recreating the real container (the manual
`docker compose up -d --build spawner-server`, or the restart button) once it's solid.

## `claude-log.sh` — inspect a session's transcript

Prints the exact conversation (your dictated input + Claude's replies) for a spawner session,
resolved from the session store (`SPAWNER_STATE`, default
`~/.local/share/claude_spawner/sessions.json`) to the on-disk
`~/.claude/projects/<dir>/<session_id>.jsonl`. Needs `jq`.

```bash
deploy/claude-log.sh                 # list known sessions (name + dir)
deploy/claude-log.sh <session-name>  # print the whole conversation
deploy/claude-log.sh <session-name> -f   # follow live (tail -f)
```
