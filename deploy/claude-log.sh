#!/usr/bin/env bash
# Show the Claude Code transcript (the exact input we sent and the output it
# returned) for a spawner session, by session name.
#
#   claude-log.sh <session-name>        # print the whole conversation
#   claude-log.sh <session-name> -f     # follow live (tail -f)
#   claude-log.sh                       # list known sessions
#
# Claude stores each session as ~/.claude/projects/<dir-with-slashes-as-dashes>/
# <session_id>.jsonl. We resolve dir+session_id from the spawner's session store.
set -euo pipefail
STATE="${SPAWNER_STATE:-$HOME/.local/share/claude_spawner/sessions.json}"

if [[ $# -lt 1 ]]; then
  echo "sessions:"; jq -r '.[] | "  " + .name + "  (" + .dir + ")"' "$STATE"; exit 0
fi
name="$1"; follow="${2:-}"

dir=$(jq -r --arg n "$name" '.[] | select(.name==$n) | .dir' "$STATE")
sid=$(jq -r --arg n "$name" '.[] | select(.name==$n) | .session_id' "$STATE")
[[ -z "$dir" || "$dir" == "null" ]] && { echo "no such session: $name" >&2; exit 1; }

enc="${dir//\//-}"                       # /data/bam-store -> -data-bam-store
jl="$HOME/.claude/projects/${enc}/${sid}.jsonl"
[[ -f "$jl" ]] || { echo "no transcript yet at $jl" >&2; exit 1; }

# Render user turns (our input) and assistant turns (Claude's output) as clean text.
fmt='select(.type=="user" or .type=="assistant") |
  (.message.content) as $c |
  (if ($c|type)=="string" then $c
   else ([($c // [])[] | select(.type=="text") | .text] | join(" ")) end) as $t |
  select($t != "") |
  (if .type=="user" then ">> USER:   " else "<< CLAUDE: " end) + $t'

if [[ "$follow" == "-f" ]]; then
  tail -n0 -f "$jl" | jq -rc --unbuffered "$fmt"
else
  jq -r "$fmt" "$jl"
fi
