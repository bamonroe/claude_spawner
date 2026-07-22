#!/bin/sh
# spawner-job — spawner-owned detached background jobs.
#
# Claude's native run_in_background cannot span turns: the bg process shares the
# turn's process-group/pipes and dies at turn teardown, and claude tracks bg
# shells in-memory per process so the next turn's fresh claude can't poll them.
# This wrapper launches a command FULLY detached (its own setsid session, stdin
# from /dev/null, stdout/stderr to a log file) so it escapes the turn's pgid and
# survives turn teardown, and records it in an on-disk registry keyed by the
# ABSOLUTE working directory (stable across session_id rotation). A later turn's
# `list` reports which jobs finished; the server injects a note into the prompt.
#
# Usage:
#   spawner-job start '<shell command>'   launch detached; prints the job id
#   spawner-job list [--json]             list this dir's jobs + status
#   spawner-job tail <id>                 print a bounded tail of a job's log
#   spawner-job reap <id>                 delete a finished job's files
#   spawner-job kill <id>                 terminate a running job (group SIGTERM) + remove it
#   spawner-job hook                      Claude Code PreToolUse enforcement (stdin=payload)
#
# The `hook` subcommand is wired as a Claude Code PreToolUse hook on the Bash tool
# (injected via `claude --settings`). It reads the tool payload on stdin and, when
# it's a background Bash launch (run_in_background true), transparently REWRITES the
# call (PreToolUse `updatedInput`) to run detached through `spawner-job start` — no
# cancellation, Claude's Bash tool simply runs the wrapped command. So surviving-a-
# turn is enforced by the harness itself, not left to Claude remembering the priming
# instruction. (Without jq to rebuild the input safely it falls back to blocking.)
#
# Registry: ${SPAWNER_JOB_ROOT:-$HOME/.spawner-jobs}/reg/<encoded-pwd>/<id>.{json,log}
set -eu

ROOT="${SPAWNER_JOB_ROOT:-$HOME/.spawner-jobs}"

# TAIL_BYTES bounds every log read (tail/list snippet) so a runaway job can't
# blow the token budget when the server injects its completion note.
TAIL_BYTES=4000

# encode_dir maps an absolute path to a single filesystem-safe, injective token:
# each '_' becomes '_5f' and each '/' becomes '_2f', so distinct paths never
# collide (unlike a plain '/'->'_' substitution). Reproducible in any POSIX sh.
encode_dir() {
	printf '%s' "$1" | sed -e 's/_/_5f/g' -e 's#/#_2f#g'
}

regdir() {
	printf '%s/reg/%s' "$ROOT" "$(encode_dir "$(pwd -P)")"
}

# json_escape escapes a string for embedding in a JSON string literal: backslash
# and double-quote are escaped, and any newlines/tabs are folded to spaces so the
# record stays one line.
json_escape() {
	printf '%s' "$1" | tr '\n\t' '  ' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

cmd_start() {
	# owner is the session_id that launched this job, stamped into the record so a
	# later `list` can attribute the job to the session that started it even when
	# several sessions share this directory's registry. It arrives via the hook's
	# `--owner` flag (the enforced background-bash path) or the SPAWNER_SESSION env;
	# a job started with neither leaves it empty and stays dir-attributed (the old
	# behaviour, so pre-existing job files keep working).
	owner="${SPAWNER_SESSION:-}"
	while [ $# -gt 1 ]; do
		case "$1" in
			--owner) owner="$2"; shift 2 ;;
			*) break ;;
		esac
	done
	cmd="${1:-}"
	[ -n "$cmd" ] || { echo "usage: spawner-job start [--owner <id>] '<cmd>'" >&2; exit 2; }
	dir="$(regdir)"
	mkdir -p "$dir"
	# Launch fully detached: nested setsid gives the job a NEW session/pgid distinct
	# from the turn's, nohup ignores SIGHUP, stdin from /dev/null and stdout/stderr to
	# the log file so the closing turn channel can't SIGPIPE/SIGHUP it. The turn's
	# group-kill (host: kill -pgid, ssh: kill -pgid) therefore can't reach it.
	# Id from epoch + pid + a short random, unique even for multiple starts in one
	# turn (same pid, same second) so their job files never collide.
	id="$(date +%s)_$$_$(awk 'BEGIN{srand();printf "%04x", int(rand()*65536)}')"
	log="$dir/$id.log"
	setsid nohup sh -c "$cmd" </dev/null >"$log" 2>&1 &
	pid=$!
	# The backgrounded setsid becomes the job leader; $! is its pid.
	started="$(date +%s)"
	cmd_esc="$(json_escape "$cmd")"
	owner_esc="$(json_escape "$owner")"
	# One file per job (never a shared appended file) so concurrent starts in a single
	# turn are lock-free.
	printf '{"id":"%s","pid":%s,"cmd":"%s","started":%s,"session":"%s"}\n' "$id" "$pid" "$cmd_esc" "$started" "$owner_esc" >"$dir/$id.json"
	printf '%s\n' "$id"
}

# job_status prints "running" or "done <exitcode>" for a pid. A live pid is
# running; a dead one is done. Exit code isn't recoverable for a detached job
# (its parent shell is gone), so a finished job reports exit code 0 by
# convention — the log carries the real outcome, which is what gets injected.
job_status() {
	pid="$1"
	if kill -0 "$pid" 2>/dev/null; then
		printf 'running'
	else
		printf 'done'
	fi
}

cmd_list() {
	json=0
	[ "${1:-}" = "--json" ] && json=1
	dir="$(regdir)"
	[ -d "$dir" ] || { [ "$json" -eq 1 ] && printf '[]\n' || printf 'no background jobs\n'; return 0; }
	first=1
	[ "$json" -eq 1 ] && printf '['
	for f in "$dir"/*.json; do
		[ -e "$f" ] || continue
		id="$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' "$f")"
		pid="$(sed -n 's/.*"pid":\([0-9]*\).*/\1/p' "$f")"
		cmd="$(sed -n 's/.*"cmd":"\(.*\)","started".*/\1/p' "$f")"
		started="$(sed -n 's/.*"started":\([0-9]*\).*/\1/p' "$f")"
		# session is the owning session_id ("" for legacy files written before the field).
		session="$(sed -n 's/.*"session":"\([^"]*\)".*/\1/p' "$f")"
		[ -n "$pid" ] || continue
		st="$(job_status "$pid")"
		if [ "$json" -eq 1 ]; then
			[ "$first" -eq 1 ] || printf ','
			first=0
			done=false; [ "$st" = "done" ] && done=true
			printf '{"id":"%s","pid":%s,"cmd":"%s","started":%s,"done":%s,"exit":0,"session":"%s"}' \
				"$id" "$pid" "$cmd" "$started" "$done" "$session"
		else
			printf '%s\t%s\t%s\n' "$id" "$st" "$cmd"
		fi
	done
	[ "$json" -eq 1 ] && printf ']\n' || true
}

cmd_tail() {
	id="$1"
	dir="$(regdir)"
	log="$dir/$id.log"
	[ -f "$log" ] || { printf '(no log for %s)\n' "$id"; return 0; }
	# Bounded tail so a huge log can't flood the injected note.
	tail -c "$TAIL_BYTES" "$log"
}

cmd_reap() {
	id="$1"
	dir="$(regdir)"
	rm -f "$dir/$id.json" "$dir/$id.log"
}

# cmd_kill terminates a running job and removes its files. The job leader's pid is
# its own session/process-group id (setsid made it the group leader at start), so a
# negative-pid SIGTERM takes the whole tree — the job's command AND anything it
# spawned — down together, mirroring the turn executors' group-kill.
cmd_kill() {
	id="$1"
	dir="$(regdir)"
	f="$dir/$id.json"
	if [ -f "$f" ]; then
		pid="$(sed -n 's/.*"pid":\([0-9]*\).*/\1/p' "$f")"
		[ -n "$pid" ] && kill -TERM -"$pid" 2>/dev/null || true
	fi
	rm -f "$dir/$id.json" "$dir/$id.log"
	printf 'killed %s\n' "$id"
}

# cmd_hook is the Claude Code PreToolUse handler (matcher scopes it to Bash). A
# foreground call passes straight through (exit 0, no output). A BACKGROUND call is
# transparently REWRITTEN — not cancelled — to run detached through `spawner-job
# start` instead: we emit a PreToolUse `updatedInput` that replaces the tool's
# arguments, so from Claude's side the same Bash tool just runs the wrapped command,
# no retry and no confusion. jq does the rewrite (its @sh shell-quotes the original
# command so it reaches spawner-job as one intact argument, and it re-emits valid
# JSON). If jq is absent we can't rebuild the tool input safely, so we fall back to
# BLOCKING (exit 2) with a redirect message — enforcement holds either way.
cmd_hook() {
	# The server bakes the launching session_id into the staged hook command
	# (`spawner-job hook --owner <id>`) so the rewritten `start` stamps the job with
	# its owner. Absent for an older staged settings payload → owner stays empty.
	owner=""
	while [ $# -gt 0 ]; do
		case "$1" in
			--owner) owner="${2:-}"; shift 2 ;;
			*) shift ;;
		esac
	done
	payload="$(cat)"
	bg="$(printf '%s' "$payload" | jq -r '.tool_input.run_in_background // false' 2>/dev/null || true)"
	if [ -z "$bg" ]; then
		# No jq: detect via a whitespace-insensitive substring match instead.
		if printf '%s' "$payload" | tr -d ' \t' | grep -q '"run_in_background":true'; then
			printf 'Background bash does not survive a turn in this environment and will be killed. Re-run it detached with `%s start '\''<command>'\''` instead — it outlives the turn and you will be notified when it finishes.\n' "$0" >&2
			exit 2
		fi
		exit 0
	fi
	[ "$bg" = "true" ] || exit 0
	printf '%s' "$payload" | jq -c --arg sj "$0" --arg owner "$owner" '
		.tool_input as $ti
		| (($ti.command) // "") as $c
		| (if $owner == "" then "" else " --owner " + ($owner | @sh) end) as $own
		| {hookSpecificOutput: {
			hookEventName: "PreToolUse",
			permissionDecision: "allow",
			updatedInput: ($ti | .run_in_background = false | .command = ($sj + " start" + $own + " " + ($c | @sh))),
			additionalContext: ("This command was auto-detached via spawner-job so it survives the turn: it ran in the foreground and printed a job id — do not poll it with BashOutput. Check progress with " + $sj + " list (or " + $sj + " tail <id>).")
		}}'
}

sub="${1:-}"
[ $# -gt 0 ] && shift || true
case "$sub" in
	start) cmd_start "$@" ;;
	list)  cmd_list "${1:-}" ;;
	tail)  [ $# -ge 1 ] || { echo "usage: spawner-job tail <id>" >&2; exit 2; }; cmd_tail "$1" ;;
	reap)  [ $# -ge 1 ] || { echo "usage: spawner-job reap <id>" >&2; exit 2; }; cmd_reap "$1" ;;
	kill)  [ $# -ge 1 ] || { echo "usage: spawner-job kill <id>" >&2; exit 2; }; cmd_kill "$1" ;;
	hook)  cmd_hook "$@" ;;
	*) echo "usage: spawner-job {start|list|tail|reap|kill|hook} ..." >&2; exit 2 ;;
esac
