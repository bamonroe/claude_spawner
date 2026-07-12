# Host deployment

The server runs in a **Docker container** that builds the Go binary from source — this is the one
supported deployment. It runs as your ordinary user (never host root) and drives the host over
**SSH** (`SPAWNER_SSH=1`): `claude` for host turns and rootless Podman for sandbox turns both run
**on the host**, over the same SSH connection, so the container needs no host root and no separate
broker. The only other container is transcription (the resident whisper servers). This directory
holds the server's compose file, its env template, the rebuild script, and a transcript helper.

| File                        | What it is                                                                 |
|-----------------------------|----------------------------------------------------------------------------|
| `spawner-container.yml`     | Compose file for the containerized SSH-native server (builds the binary; drives the host over SSH). |
| `spawner-container.env.example`| template for the server's env file (token, addr, root, SSH key/known_hosts, whisper, restart, sandbox). |
| `rebuild-container.sh`      | host-side rebuild + recreate of the container; the restart button runs this over SSH.       |
| `claude-log.sh`             | helper to read a session's Claude transcript by name.                     |

## Transcription depends on the resident whisper servers

The server transcribes via one **resident whisper.cpp HTTP server** (on `:8571`) defined in the root
[`docker-compose.yml`](../docker-compose.yml). It carries `restart: unless-stopped`, so once created
it survives reboots, but a `docker compose down` removes it and voice goes silent until it's
recreated. Bring it back with `docker compose up -d whisper`. An optional second "fast" draft model
on `:8572` (`whisper-fast`) can offload the live hands-free draft — start it and set
`SPAWNER_WHISPER_FAST_URL` to enable it. See [`../whisper/README.md`](../whisper/README.md).

## Running the server

[`spawner-container.yml`](spawner-container.yml) runs it. It uses **host networking** (so
`localhost:22` is the host's own sshd and an empty `Session.Host` drives the host) and mounts the
user's home + project roots at the **same paths** the host uses (so the server browses and reads
transcripts where the host writes them; `claude` runs on the host over SSH). Its config vars are
documented in [`../CLAUDE.md`](../CLAUDE.md) (the config section — the authoritative list),
templated in `spawner-container.env.example`.

Prereqs: the server user must have a **key-based SSH login to itself** (its public key in
`~/.ssh/authorized_keys`). The server's SSH auth material is **self-contained in `deploy/state/`**,
not read from the host home: put the private key at `deploy/state/ssh/` (`0600`) and point
`SPAWNER_SSH_KEY` at it (`/state/ssh/...`). Host keys are verified against the server's **own**
known_hosts — `SPAWNER_SSH_KNOWN_HOSTS=/state/known_hosts` (in `deploy/state/`), deliberately
**independent of the host's `~/.ssh/known_hosts`** so the server owns its trust set. Seed it with
every host the server dials (loopback for the restart button and local turns, plus each registered
SSH host), e.g. `ssh-keyscan -t ed25519,rsa localhost your.remote.host >> deploy/state/known_hosts`
after eyeballing the fingerprints. Then:

```bash
cp deploy/spawner-container.env.example deploy/spawner-container.env   # edit token, key, port
mkdir -p deploy/state
# SPAWNER_UID/GID (not UID/GID — those are readonly in some shells) so it runs as you:
SPAWNER_UID=$(id -u) SPAWNER_GID=$(id -g) docker compose -f deploy/spawner-container.yml up -d --build
```

The `deploy/spawner-container.env` (it holds the token) and `deploy/state/` are git-ignored — keep
the token out of the repo. Point a client at its port (`SPAWNER_ADDR`, e.g. `:8098`) to exercise it.
Verified end to end: a turn dictated through the container runs `claude` on the host over SSH and
streams the reply back. (Transcription needs the resident whisper servers if you want the voice
path; text turns work without them.)

## The restart button rebuilds

For the container the app's **restart** button is a *one-tap deploy*, not just a bounce:
`SPAWNER_RESTART_CMD` SSHes to the host over loopback and launches
[`rebuild-container.sh`](rebuild-container.sh) detached (`setsid`), which runs `compose up -d --build`
to rebuild the image from current source and recreate the container. It **must** run on the host —
`up --build` replaces the very container the server lives in, so an in-container command would be
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
`docker compose -f deploy/spawner-container.yml up -d --build`, or the restart button) once it's solid.

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
