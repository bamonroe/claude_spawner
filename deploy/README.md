# Host deployment

The server runs **bare metal** as a single Go binary under a systemd *user*
service ([`spawner-server.service`](spawner-server.service)) — it forks `claude`
for host turns and drives rootless Podman for sandbox turns itself (no separate
broker). The only thing still containerized is transcription (the resident
whisper servers). This directory holds the server's host service, the rebuild
script, and a transcript helper.

| File                        | What it is                                                                 |
|-----------------------------|----------------------------------------------------------------------------|
| `spawner-server.service`    | systemd **user** service for the bare-metal server.                        |
| `spawner-server.env.example`| template for the server's `EnvironmentFile` (token, addr, root, whisper, restart cmd, sandbox). |
| `rebuild.sh`                | rebuild + relaunch the stack (whisper servers, then the server binary + service). |
| `claude-log.sh`             | helper to read a session's Claude transcript by name.                     |
| `spawner-container.yml`     | Compose file for the **containerized SSH-native** server (drives the host over SSH). |
| `spawner-container.env.example`| template for the containerized server's env file.                       |
| `rebuild-container.sh`      | host-side rebuild + recreate of the container; the container's restart button runs this over SSH. |

## Transcription depends on the resident whisper servers

The server transcribes via two **resident whisper.cpp HTTP servers** (accurate on
`:8571`, fast draft on `:8572`) defined in the root
[`docker-compose.yml`](../docker-compose.yml). They carry `restart: unless-stopped`,
so once created they survive reboots, but a `docker compose down` removes them and
voice goes silent until they're recreated. Bring them back with `docker compose up
-d whisper whisper-fast` (or just run `rebuild.sh`, which does it first). See
[`../whisper/README.md`](../whisper/README.md).

## The server service

`spawner-server.service` runs the server as your ordinary user (never root). The
install steps (build the binary, drop the env file, enable the lingering user
service) live in the unit file's header comment — follow those. Its config vars
are documented in [`../CLAUDE.md`](../CLAUDE.md) (the config section — the
authoritative list), templated in `spawner-server.env.example`.

## Containerized (SSH-native) alternative

Because host turns now run over **SSH** (`SPAWNER_SSH=1`), the server can run in a
container that drives the host over SSH — `claude` runs on the host, not in the
image — with **no host broker and no host root**. This is the clean version of the
containerization the 2026-07-06 revert removed: the thing that made it not worth it
(a bespoke privileged host broker) is gone, replaced by standard SSH.

[`spawner-container.yml`](spawner-container.yml) runs it. It uses **host networking**
(so `localhost:22` is the host's own sshd and an empty `Session.Host` drives the
host) and mounts the user's home + project roots at the **same paths** the host uses
(so the server browses and reads transcripts where the host writes them). Because
execution is over SSH, it can run **in parallel** with the bare-metal service on a
different port — a safe way to try it or cut over.

Prereqs: the server user must have a **key-based SSH login to itself** (its public
key in `~/.ssh/authorized_keys`, the localhost host key in `~/.ssh/known_hosts`).
Then:

```bash
cp deploy/spawner-container.env.example deploy/spawner-container.env   # edit token, key, port
mkdir -p deploy/state
# SPAWNER_UID/GID (not UID/GID — those are readonly in some shells) so it runs as you:
SPAWNER_UID=$(id -u) SPAWNER_GID=$(id -g) docker compose -f deploy/spawner-container.yml up -d --build
```

The `deploy/spawner-container.env` (it holds the token) and `deploy/state/` are
git-ignored — keep the token out of the repo.

Point a client at its port (`SPAWNER_ADDR`, e.g. `:8098`) to exercise it. Verified
end to end: a turn dictated through the container runs `claude` on the host over SSH
and streams the reply back. (Transcription still needs the resident whisper servers
if you want the voice path; text turns work without them.)

## Rebuilding the stack

`rebuild.sh` rebuilds and relaunches everything in order — whisper servers, then
the server binary + user service. It needs **no root**: user `bam` is in the
`docker` group and the server is a systemd *user* service, so run it directly:

```bash
deploy/rebuild.sh
```

The app's **restart** button is a *pure service restart*, not a rebuild: the
server fires `SPAWNER_RESTART_CMD` (set it to `systemctl --user restart --no-block
spawner-server` in the env file), relaunching whatever binary is already built.
`--no-block` plus the unit's `KillMode=process` let the restart proceed without the
command being killed as the unit tears down. Shipping new code is the separate
manual `rebuild.sh` step above.

**Containerized deployment — the button rebuilds.** For the container
(`spawner-container.yml`) the restart button is a *one-tap deploy*, not just a
bounce: `SPAWNER_RESTART_CMD` SSHes to the host over loopback and launches
[`rebuild-container.sh`](rebuild-container.sh) detached (`setsid`), which runs
`compose up -d --build` to rebuild the image from current source and recreate the
container. It **must** run on the host — `up --build` replaces the very container
the server lives in, so an in-container command would be killed mid-recreate;
`setsid` over SSH decouples it so it survives. The image ships `openssh-client` for
exactly this, and the compose file mounts the host `/etc/passwd` read-only — without
a passwd entry the openssh client aborts with *"No user exists for uid"* (the container
runs as a bare uid; Go's SSH for turns doesn't care, but the CLI does). **Bootstrap:**
the running container must already have been built from this Dockerfile (with
`openssh-client`), have `SPAWNER_RESTART_CMD` in its env, and have the `/etc/passwd`
mount, so do one manual `SPAWNER_UID=$(id -u) SPAWNER_GID=$(id -g) docker compose -f
deploy/spawner-container.yml up -d --build` first; after that the button self-serves.
A rebuild started this way will drop any in-flight turn as the container is recreated.

### Testing a build without touching the live server

Restarting the live unit kills any in-flight turn (including one you're driving
over voice). To test a server change safely, run the freshly-built binary by hand
on a scratch port with a separate session store, leaving the live `:8558` service
running:

```bash
go build -C server -o /tmp/spawner-dev .
SPAWNER_TOKEN=devsecret SPAWNER_ADDR=:8557 \
  SPAWNER_STATE=$HOME/.local/share/claude_spawner_dev/sessions.json \
  SPAWNER_ROOT=/data:$HOME \
  SPAWNER_WHISPER_URL=http://localhost:8571 SPAWNER_WHISPER_FAST_URL=http://localhost:8572 \
  /tmp/spawner-dev
```

Point the app at `:8557` to exercise it; promote by rebuilding the real service
(`deploy/rebuild.sh`) once it's solid.

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
