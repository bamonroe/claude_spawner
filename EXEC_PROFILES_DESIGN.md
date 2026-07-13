# Design note — session execution-environment profiles

Status: **in progress**. Server-side file-backed profiles, the profile-selection wire slice, and
new-session picker controls landed 2026-07-13; example profile presets and templating remain future
phases.

Prerequisite for the **opencode backend** work: opencode gives one binary that fronts 75+ providers
and local models, but using it (and local models generally) means credentials and network endpoints
have to be provisioned per target — especially into sandbox sessions. This note designs the
configurable, templatable execution environment that makes that tractable. The opencode backend
lands *on top of* this, not before it.

## Problem

Today a sandbox session's execution environment is **global and flat** — `SPAWNER_SANDBOX_MOUNTS`,
`SPAWNER_SANDBOX_RUN_ARGS`, `SPAWNER_SANDBOX_IMAGE` apply to *every* sandbox session, consumed inside
`SandboxExecutor`. Two things this can't express:

1. **Credentials per target.** opencode reads `~/.local/share/opencode/auth.json` (or per-provider
   env keys). To run an opencode turn in a sandbox, that credential must be injected — mounted or as
   an env var — and ideally scoped to just what the profile needs (mounting the whole `auth.json`
   hands every provider key to agent-driven code in the box).
2. **Endpoints across hosts.** A local Ollama model lives on one machine (e.g. `pickle.bam.net`).
   A turn running on another host, or inside a sandbox, must be pointed at it. We want to declare
   that once, not re-specify it per session.

Both are "how this session executes" config that should be **named, per-session-selectable, and
templated** so it isn't retyped.

## Concept: an execution profile

A named, declarative bundle describing how a session's turns execute. **Target-agnostic** — applies
to host and sandbox sessions alike (host profiles mostly carry `env`/endpoints; sandbox profiles
also carry mounts/creds). Rough shape:

```
name:     "ollama-open"
target:   sandbox            # host | sandbox (advisory default; session target still wins)
image:    "<override>"       # sandbox only; empty = SPAWNER_SANDBOX_IMAGE
home_mount: "{{.Home}}"      # sandbox only; host home bind-mounted rw at the same path. Empty = omit
mounts:   [ "{{.Home}}:/home:rw" ]              # bind mounts (sandbox)
env:      { OLLAMA_BASE_URL: "http://{{.OllamaHost}}:11434" }
creds:    [ "{{.OpencodeAuth}}:/root/.local/share/opencode/auth.json:ro" ]   # file injections
run_args: [ "--userns=keep-id" ]               # escape hatch, appended to podman run
```

- **"Locked down" vs "open" is just two profiles.** `locked` = empty mounts/creds/env. `open` =
  mounts `/home`. No new mechanism.
  - ✅ **Resolved 2026-07-13 (was a review finding): the sandbox home mount is now profile-scoped.**
    The home mount moved off the executor onto the profile as a `home_mount` field, and
    `createArgsFor` mounts the host home only when the resolved profile carries it. The built-in
    `default` profile is seeded with the server's `HOME` (so existing behavior is unchanged), while a
    `locked` profile leaves `home_mount` empty and gets no host home inside the box. A profile can now
    *subtract* the home mount, not only *add* to a fixed baseline.
- **Templatable** = `{{.Var}}` substitution (Go `text/template`) so one profile adapts per host
  without duplication.

### Substitution context

**Resolved 2026-07-13.** A small fixed set of **built-in, host/session-derived** variables, plus an
open-ended **user-defined `vars` map** so new setup-specific values never require a code change or a
new named variable (the trap we explicitly wanted to avoid).

Built-ins (resolved at turn time from the executing host + the session):

| Var             | Source                                                         |
|-----------------|---------------------------------------------------------------|
| `{{.Home}}`     | the login user's home on the executing host                   |
| `{{.Session}}`  | the session id                                                 |
| `{{.Dir}}`      | the session's working directory                               |

User-defined (the extensible part):

| Var                | Source                                                                      |
|--------------------|----------------------------------------------------------------------------|
| `{{.Vars.X}}`      | value `X` from the merged vars map — a global set from config, overlaid by  |
|                    | the profile's own `vars` (profile wins on a name clash). e.g. `{{.Vars.OllamaHost}}`. |

Deferred to phase 6: an opencode-auth path variable, once credential staging exists to produce it.

Templating applies to every string-bearing profile field — `image`, `home_mount`, each `mounts` /
`creds` / `run_args` entry, and each `env` value. An unknown `{{.Vars.X}}` is a resolve-time error
(fail loud, don't silently mount an empty path). Keep the built-in set small; grow `vars`, not it.

## The three decisions that shaped this

1. **Ollama reachability → templated endpoint URL, not transparent `localhost` remap.** Inside a
   container `localhost` is the container itself; a transparent `localhost:11434 → remote` remap
   needs a forwarder/proxy sidecar (socat). Simpler and sufficient: the profile sets the model's
   base URL directly (`OLLAMA_BASE_URL`) and opencode reads it. Only go transparent if something is
   hard-wired to literal `localhost` — opencode is not.
   - **Reachability gotchas** (design around, don't ignore): the host actually running the sandbox
     (via the SSH pool on the loopback host) must route to `pickle.bam.net`, and rootless podman DNS
     may not resolve a Tailscale hostname *inside* the container — prefer an IP, or set
     `--dns`/`--add-host` in `run_args`.
2. **Credential form → least-privilege, both mechanisms.** Support env-var API keys
   (`OPENAI_API_KEY`, …) *and* file mounts. Default to injecting only the one credential a profile
   needs. Since Claude stays native, most opencode-in-sandbox use is metered API keys, so a single
   env-var key is the simplest and safest default; the full-`auth.json` mount is reserved for
   profiles that explicitly opt in. **Security note:** any credential in a sandbox is reachable by
   the agent-driven code running there — that is inherent (the agent needs the key to call the
   model), which is exactly why per-profile scoping is the default.
3. **Ownership → app-managed registry (the `hosts.json`/`identities.json` model).** The app is the
   source of truth: the user creates/edits/deletes profiles on a settings page and the server just
   persists the catalogue to `SPAWNER_PROFILES` (`profiles.json`) and re-broadcasts on change. The
   first pass shipped it server-file-only; **revised 2026-07-13** to the full app-managed store
   (`ProfileRegistry` gained `path`/mutex/atomic flush + `Put`/`Delete`/`SetDefault`, mirroring
   `HostStore`).
4. **"Default" is a marker, not a profile.** There is no built-in `default` profile. Each profile is
   a real thing the user defines (e.g. `bare-metal`, `sandbox`, `locked`), and exactly one may carry
   a `default` flag the user sets; a session with no explicit choice resolves to it. If none is
   marked, the first in the catalogue wins. On first run (no file) the server **seeds starters** from
   the flat sandbox config — `bare-metal` (host, marked default so a fresh install always works),
   plus `sandbox` and `locked` when `SPAWNER_SANDBOX_IMAGE` is set — then hands ownership to the app.

## Backward compatibility

The existing env-var sandbox config **seeds the starter profiles on first run** (see decision 4). A
session with no profile resolves to the marked default (the seeded `bare-metal`, which is host-target
— matching the pre-profile behavior of running on the host). Sessions persist only a `profile` name;
empty or unknown resolves to the default marker.

Implemented 2026-07-13: the server reads/writes `SPAWNER_PROFILES` (`profiles.json`) as either a JSON
array or `{ "profiles": [...] }`. A missing file is seeded (see decision 4) and written out. Profile
`env` applies to host turns, SSH host turns, and host-side short commands; for sandbox sessions,
`image`/`mounts`/`creds`/`env`/`run_args` shape the persistent container at create time (an empty
`image` falls back to `SPAWNER_SANDBOX_IMAGE`). A profile's `home_mount` is bind-mounted only when
set, so a `locked` profile with no mounts is genuinely isolated.

## Integration seam

- Session gains a `Profile` field (name; persisted like `Agent`/`Model`).
- The resolved profile is threaded onto the session so `SandboxExecutor.Start` and the SSH executor
  consume it instead of reading globals directly.
- Gateway protocol carries the selectable profile list (implemented 2026-07-13 as `profiles`) and
  the session's chosen profile (`spawn_at.profile`, then `attached` / `session_list` /
  `discovered`), so the visual "new session" picker and voice spawn flow have a durable wire slot
  to choose one. `docs/protocol.md` + docsync/clientsync stay updated in the same pass.
- `default` selection preserves current behavior for pre-profile clients.

## Phasing

1. ✅ 2026-07-13 — **Profile struct + registry + server-side session field.** Current env-var
   sandbox config becomes the built-in `default`. No behavior change for sessions that do not name a
   profile.
2. ✅ 2026-07-13 — **Mounts + credential injection primitives** (env and file) are executable from
   the profile file. Documented `locked` / `open` example presets ship in
   `deploy/profiles.example.json` (a loader guard test keeps them valid).
3. ✅ 2026-07-13 — **Profile selection wire slot.** Clients receive the `profiles` catalogue, can
   send `spawn_at.profile`, and registered-session messages echo the chosen non-default profile.
4. ✅ 2026-07-13 — **Visible new-session picker controls.** `BrowseScreen` shows profile chips,
   defaults to the advertised default, applies the profile's advisory target on selection, and sends
   the chosen profile on both existing spawn paths.
5. ✅ 2026-07-13 — **Network/endpoint config + `{{.Var}}` templating.** Every string-bearing
   profile field is rendered per turn against built-ins `{{.Home}}` / `{{.Session}}` / `{{.Dir}}`
   plus a user-defined `{{.Vars.X}}` map (global `SPAWNER_PROFILE_VARS` overlaid by the profile's
   own `vars`). An undefined var fails the turn loudly. Unlocks Ollama-across-hosts — see the
   `ollama` preset in `deploy/profiles.example.json`.
6. **App-managed profiles registry** (user-defined profiles + default marker). Turns the read-only
   catalogue into a full CRUD registry the app owns, so users define their own profiles instead of
   editing a server file. Broken into sub-steps:
   - ✅ 2026-07-13 — **Server foundation.** `ProfileRegistry` is now a file-backed store (path +
     mutex + atomic flush, `Put`/`Delete`/`SetDefault`/`Get`/`DefaultName`), mirroring `HostStore`.
     "Default" became a per-profile marker (no built-in `default` profile); resolution falls back to
     the marked profile, else the first. First run seeds `bare-metal`/`sandbox`/`locked` starters
     from the sandbox env vars. Covered by store CRUD + seeding + default-marker tests.
   - ✅ 2026-07-13 — **Wire + gateway CRUD.** `profile_put` / `profile_delete` / `profile_set_default`
     inbound handlers (`gateway/profiles.go`) mutate the store and `broadcast(msgProfiles(...))`;
     `msgProfiles` now carries the full ExecProfile per entry so the editor can round-trip them;
     `bad_profile` error code; `docs/protocol.md` + docsync/clientsync + `Protocol.kt` builders
     (`profilePut`/`profileDelete`/`profileSetDefault`) + enriched `ProfileInfo` all green. Covered by
     a gateway CRUD-broadcast test.
   - 🟡 **App profiles settings page — code written, build-verify pending.** A `ProfilesSettings`
     Compose screen (mirroring `HostsSettings`): list with per-row default marker + "Make default" +
     Edit/Delete, and an add/edit form (name, target chips, image, home_mount, and multiline
     mounts/creds/env/run_args/vars). A `ProfilesController` interface (`profiles` + put/delete/
     set-default), impls in both `VoiceController` and `WebAppController`, a `set_profiles` hub row,
     and routing in `MainActivity.kt` + `WebRoot.kt`. **Not yet compiled/tap-tested:** the local
     Gradle toolchain is broken — `~/.gradle/gradle.properties` pins `org.gradle.java.home` to a
     removed JDK 21, and the box only has JDK 25/26 which Gradle 8.10.2 rejects. Needs a working JDK
     (17/21) to build the APK, install on the emulator, then the Pixel 8a, and tap-test the editor.
7. **opencode backend spike** drops in on top: an `opencode.go` agent + a profile that injects its
   creds and points at the Ollama endpoint. (See the multi-backend epic in `TODO.md`.)

## Relationship to the interrupted "Fable" client refactor

The lost Fable work (`CLIENT_REFACTOR_TODO.md`) is almost entirely **client-side** — Compose/Android
UI defaults, resolving the default agent from the server's `agents` registry, a mic-meter lifecycle
fix, and a settings store/reducer. This profile system is **server-side** (config, session model,
both executors, gateway). They do **not** block each other and can proceed independently.

The one adjacency: profile selection eventually surfaces in the client's **new-session UI and its
store/reducer** — the same store Fable was building. So when that client refactor is redone, leave a
`profile` field alongside the `agent`/`model` selection it already resolves from the registry, so the
picker has a home. Nothing here forces the client work to happen first; server phases 1–3 stand alone.
