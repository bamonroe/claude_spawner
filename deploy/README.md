# Host deployment (systemd)

Files for running the spawner as a long-lived service on the host (as opposed to `docker compose`
or a bare `go run`). This is the setup that uses the [resident GPU whisper servers](../whisper/README.md).

| File                  | What it is                                                                 |
|-----------------------|----------------------------------------------------------------------------|
| `spawner.service`     | systemd unit — rebuilds and runs the server under user `bam`.              |
| `spawner.env.example` | template for the unit's `EnvironmentFile` (token, root, whisper wiring).   |
| `claude-log.sh`       | helper to read a session's Claude transcript by name.                     |

## The unit

`spawner.service` runs the server as user/group `bam` from `/data/claude_spawner/server`, listening
on `SPAWNER_ADDR` (`:8555` in the example — the port the Android app defaults to). It rebuilds on
every (re)start so it always runs current code, then `exec`s the built binary (not `go run`, so
systemd supervises the real process):

```ini
ExecStartPre=/usr/bin/go build -o /data/claude_spawner/server/spawner .
ExecStart=/data/claude_spawner/server/spawner
Restart=on-failure
```

It reads config from `EnvironmentFile=/home/bam/.config/claude_spawner/spawner.env` and sets `HOME`
+ a `PATH` that includes `~/.local/bin` (where `go`, `claude`, and `whisper-cli` live). It starts
`After=docker.service` so the resident whisper containers are up first.

## Install

```bash
# 1. config from the template (never commit the real token)
mkdir -p ~/.config/claude_spawner
cp deploy/spawner.env.example ~/.config/claude_spawner/spawner.env
$EDITOR ~/.config/claude_spawner/spawner.env      # set SPAWNER_TOKEN, roots, whisper URLs

# 2. resident whisper servers (see ../whisper/README.md)
docker compose up -d whisper whisper-fast

# 3. the unit
sudo cp deploy/spawner.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now spawner
journalctl -u spawner -f                          # logs
```

Config vars referenced by `spawner.env.example` are documented in [`../CLAUDE.md`](../CLAUDE.md)
(the config section — the authoritative list). The whisper URLs point at the resident containers,
with the `whisper-cli` paths as the fallback used only when the URLs are blank.

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
