# Host deployment

The server runs as a **Docker container** ([`docker-compose.broker.yml`](../docker-compose.broker.yml));
the only thing installed on the host is the small **broker** that executes turns on the server's
behalf. This directory holds the broker's host service and a transcript helper.

| File                        | What it is                                                                 |
|-----------------------------|----------------------------------------------------------------------------|
| `spawner-broker.service`    | systemd **user** service for the host-side broker (`cmd/broker`).          |
| `spawner-broker.env.example`| template for the broker's `EnvironmentFile` (socket, root, claude, sandbox, restart cmd). |
| `claude-log.sh`             | helper to read a session's Claude transcript by name.                     |

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
the broker to run `SPAWNER_BROKER_RESTART_CMD` (set it in the broker env file).

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
