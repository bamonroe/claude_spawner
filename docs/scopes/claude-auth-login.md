# Probe: driving `claude auth login` programmatically

Scoping notes for the "re-login to Claude" epic. Measured against the `claude` CLI on the host on
2026-08-10, always with an isolated `HOME` (a throwaway `mktemp -d`) so the real credentials were
never at risk. This file records **what the CLI does**; the resulting implementation tasks live in
`TODO.toml`.

## There is no TUI to scrape

The interactive `/login` slash command is not the seam. The CLI exposes the whole flow as
subcommands:

```
claude auth login [--claudeai | --console] [--sso] [--email <email>]
claude auth logout
claude auth status [--json | --text]
```

`--claudeai` (the default) is the subscription path; `--console` is API/console billing. Driving
these keeps the no-TUI-scraping rule in `docs/architecture.md` intact.

## `auth status` is the authoritative check

`claude auth status --json` prints a small JSON object and is the pre/post-login source of truth:

```json
{ "loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty",
  "email": "…", "orgId": "…", "orgName": "…", "subscriptionType": "max" }
```

Logged out it returns `{"loggedIn": false, "authMethod": "none", "apiProvider": "firstParty"}` —
and **exits 1**. So the exit code alone is a usable liveness check, but parse the JSON for the
detail we show the user. Which account it reports is entirely a function of `HOME`, which is what
makes the isolated-`HOME` probe safe and what makes "log in as the right identity on the right
host" a matter of running the command with the right `HOME`/user.

## What `auth login` emits

On stdout, immediately:

```
Opening browser to sign in…
If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=…
Paste code here if prompted >
```

Load-bearing details:

- **The URL is plain text on stdout** under a plain pipe — no PTY needed just to read it. Match the
  `https://claude.com/cai/oauth/authorize?…` (subscription) or
  `https://platform.claude.com/oauth/authorize?…` (console) URL.
- **Under a PTY the URL is wrapped in an OSC-8 hyperlink**, so the text appears twice with escape
  bytes around it. Strip ANSI/OSC sequences *before* extracting, and de-duplicate.
- **The `redirect_uri` is `https://platform.claude.com/oauth/code/callback`** — a hosted page, not
  `localhost`. That is the single most useful finding: **no local callback listener is involved**,
  so the browser that completes the flow does not have to be on the server's machine. The user can
  finish it on the phone and paste the code back. Nothing needs a port opened.
- The CLI tries to open a browser itself. On a headless server that just fails quietly; we ignore
  it and drive the URL ourselves.
- Each invocation mints a fresh `code_challenge`/`state`, so a URL is bound to **that** process —
  the process must stay alive from URL emission until the code is submitted. Restarting the login
  invalidates the previous URL.

## The code must be written to a PTY

This is the constraint that shapes the driver:

- Piping the code to **stdin does nothing** — the prompt is read from the terminal, so with a plain
  pipe the process ignores the input and hangs forever (killed at timeout, exit 124).
- Run under a PTY and writing `<code>` + newline works: a deliberately bogus code produced
  `Login failed: Request failed with status code 400` and **exit 1**.

So the login driver must allocate a PTY, not just pipes — unlike the headless turn Executor, which
uses plain pipes. Success is confirmed by exit 0 plus a follow-up `auth status --json`.

## Consequences for the implementation

- The login process is long-lived and stateful: start → emit URL → wait (indefinitely) → accept one
  code → exit. It needs its own supervisor with a timeout and cancellation, not the turn path.
- It must run with the `HOME`/user of the identity being logged in, on the target host.
- One login at a time per host+identity; a second start invalidates the first URL.
