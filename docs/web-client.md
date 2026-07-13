# Web client — developer guide

How to *work on* the Compose Multiplatform browser client (Kotlin/Wasm). How a **user** builds
and runs it is in `README.md` ("The browser client"); the shared-UI architecture and repository
layout are in `docs/architecture.md`; the wire protocol is in `docs/protocol.md`. This file owns
the wasmJs-specific development knowledge: the source-set split, the `js()` interop idiom, the
build/iterate loop, and what keeps the client honest against the server.

## Source sets: what lives where

The client is one Gradle module (`android/app`) with three source sets:

- **`commonMain`** — everything shared: every composable (`MainScreen`, `ChatScreen`,
  `SettingsScreens`, `BrowseScreen`…), the wire protocol (`net/Protocol.kt` — see below), the
  Ktor `SpawnerClient`, the `Prefs` settings interface (all defaults live in its companion —
  never inline a default in a platform backend), and `tts/Markdown.kt` (markdown → speech
  stripping). **Default here.** Code goes in a platform set only when it touches a platform API.
- **`androidMain`** — Android-only: recorders (Opus/VAD), Android TTS,
  `SettingsStore` (SharedPreferences backend), the Activity. (Wake-word matching is server-side,
  in the transcript — no on-device engine here.)
- **`wasmJsMain`** — browser-only: `WebAppController` (the browser `AppController`), `WebAudio.kt`
  (mic + SpeechSynthesis + server-TTS playback via Web Audio), `WebPrefs` (localStorage backend), `WebTransfer.kt` (file pick/save),
  `WebRoot.kt` (entry point).

The compile gate for **both** targets (run before any commit that touches shared code) needs
JDK 17 or newer on `JAVA_HOME` (the Gradle 8.x wrapper handles the rest):

```bash
cd android && ./gradlew :app:compileKotlinWasmJs :app:compileDebugKotlinAndroid --no-daemon
```

A `commonMain` change that compiles on only one target is not done. The wasmJs tasks need **no
Android SDK** (the first build downloads Gradle/Node/Binaryen from the network); the Android
compile does need one (see `android/README.md`). The production bundle build
(`:app:wasmJsBrowserDistribution`) additionally runs `wasm-opt` and takes **minutes** — use the
compile task for iteration and build the bundle once at the end; output lands in
`android/app/build/dist/wasmJs/productionExecutable`.

## The `js()` interop idiom

Kotlin/Wasm calls browser APIs two ways; this repo uses both, by rule:

1. **Typed bindings** (`kotlinx.browser`, `org.w3c.dom`) when a binding exists — `localStorage`
   in `WebPrefs`, DOM types in `WebTransfer`.
2. **`js("...")` one-shots** for anything without a usable binding or where a JS closure must
   hold state across calls (`WebAudio.kt` is the reference example — the AudioContext and the
   accumulating sample chunks live on `window.__spawnerMic`, not in Kotlin).

Conventions for `js()` functions (follow `WebAudio.kt` / `WebPrefs.kt`):

- A `js()` body must be a **single expression or a self-contained function body**; keep each one
  small and give it a named Kotlin `fun` wrapper.
- Only JS-compatible types cross the boundary (`JsString`, `JsAny?`, `Boolean`, `Int`, …).
  Binary data crosses as **base64 strings** (encode in JS with `btoa` over ~32 KB slices — one
  big `String.fromCharCode(...spread)` overflows the argument limit; decode in Kotlin with
  `kotlin.io.encoding.Base64`).
- Async JS returns a `Promise<JsString>` consumed with `.then { }` in Kotlin; report failures
  **in-band** by resolving to an `"err:<name>"` string rather than rejecting — rejected promises
  crossing the Wasm boundary are awkward to type.
- **Secure-context traps:** `getUserMedia` and `crypto.randomUUID` exist only on https or
  localhost. Never call them unguarded — `WebPrefs.randomUuid()` shows the fallback pattern.

## Iterating

1. Compile gate (above) — this is the real correctness check for shared code.
2. For anything visual/interactive, build the bundle and let the server serve it
   (`SPAWNER_WEB_DIR`, see README) — there is no hot-reload loop worth using here.
3. Browser voice (mic permission, SpeechSynthesis) can only be verified by a human in a real
   browser over https/localhost; say so in the handoff instead of claiming it verified.

## What keeps the client honest

The client's wire strings live in **one file**, `commonMain/net/Protocol.kt` (`Outbound` builders,
`ServerMsg.parse`, the `Codecs` object) — never scatter a `"type"` literal or codec string
elsewhere. Server-side drift tests in `server/internal/docsync` (`clientsync_test.go`) parse that
file and fail `go test ./...` when the two ends disagree: a type the client sends that the gateway
doesn't handle, a server message the client doesn't parse, or a codec mismatch. Deliberately
one-sided messages are recorded in the exemption maps *in the test*, with reasons. The voice
command list is generated, not hand-written: `docs/commands.json` → the `generateCommands` Gradle
task → `Commands.kt` (see `docs/commands.md`).
