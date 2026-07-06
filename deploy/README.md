# Host deployment

The server runs as a **Docker container** ([`docker-compose.broker.yml`](../docker-compose.broker.yml));
the only thing installed on the host is the small **broker** that executes turns on the server's
behalf. This directory holds the broker's host service and a transcript helper.

| File                        | What it is                                                                 |
|-----------------------------|----------------------------------------------------------------------------|
| `spawner-broker.service`    | systemd **user** service for the host-side broker (`cmd/broker`).          |
| `spawner-broker.env.example`| template for the broker's `EnvironmentFile` (socket, root, claude, sandbox, restart cmd). |
| `rebuild.sh`                | rebuild + relaunch the whole stack (whisper servers, broker, server container). |
| `rebuild-broker.sh`         | rebuild the broker binary + restart its user service (wired to the restart button's self-cmd). |
| `claude-log.sh`             | helper to read a session's Claude transcript by name.                     |

## Transcription depends on the resident whisper servers

The live server transcribes via two **resident whisper.cpp HTTP servers** (accurate on `:8571`,
fast draft on `:8572`) defined in the root [`docker-compose.yml`](../docker-compose.yml) — the
broker compose does **not** start them. They carry `restart: unless-stopped`, so once created they
survive reboots, but a `docker compose down` removes them and voice goes silent until they're
recreated. Bring them back with `docker compose up -d whisper whisper-fast` (or just run
`rebuild.sh`, which does it first). See [`../whisper/README.md`](../whisper/README.md).

## Rebuilding the stack

`rebuild.sh` rebuilds and relaunches everything in order — whisper servers, the broker binary +
user service, then the server container image. It needs **no root**: user `bam` is in the `docker`
group and the broker is a systemd *user* service, so run it directly:

```bash
deploy/rebuild.sh
```

## The broker service

`spawner-broker.service` runs `cmd/broker` as your ordinary user (never root): it forks `claude`
for host turns and drives rootless Podman for sandbox turns, on behalf of the unprivileged server
container. The install steps (build the binary, drop the env file, enable the lingering user
service) live in the unit file's header comment — follow those. Its config vars are documented in
[`../CLAUDE.md`](../CLAUDE.md) (the config section — the authoritative list), templated in
`spawner-broker.env.example`.

The server container itself is brought up with `docker compose -f docker-compose.broker.yml up -d
--build` after the broker is running; see the repo [`README.md`](../README.md) for the full
end-to-end bring-up. The app's **restart** button rebuilds and relaunches that container by asking
the broker to run `SPAWNER_BROKER_RESTART_CMD` (set it in the broker env file). Because the broker
is a **separate host process** from the container, it also runs `SPAWNER_BROKER_RESTART_SELF_CMD`
right after — point that at `rebuild-broker.sh` so the broker binary is rebuilt and restarted in the
same press. Otherwise a server rebuild leaves the old broker running, and it rejects any newly-added
broker op with `unknown op ...` until you rebuild it by hand (e.g. `deploy/rebuild.sh`).

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
