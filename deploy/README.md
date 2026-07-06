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

## Rebuilding the stack

`rebuild.sh` rebuilds and relaunches everything in order — whisper servers, then
the server binary + user service. It needs **no root**: user `bam` is in the
`docker` group and the server is a systemd *user* service, so run it directly:

```bash
deploy/rebuild.sh
```

The app's **restart** button does the same thing: the server fires
`SPAWNER_RESTART_CMD` (set it to `deploy/rebuild.sh` in the env file), which
rebuilds the binary and restarts the unit. The rebuild is launched detached in its
own process group and the unit uses `KillMode=process`, so it survives the very
restart it triggers and swaps in the new binary.

### Testing a build without touching the live server

Restarting the live unit kills any in-flight turn (including one you're driving
over voice). To test a server change safely, run the freshly-built binary by hand
on a scratch port with a separate session store, leaving the live `:8556` service
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
