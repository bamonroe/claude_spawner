# Scope — make the speech gate a front door, not a text filter

Status: **proposal**, not built. Owner doc for the shipped behaviour stays `README.md`
(user-facing) + `docs/architecture.md` (pipeline); this file is the scoping note for the
rework and should be deleted once the work lands and those docs are updated.

## Today

The gate is a post-transcription string filter. Every utterance is fully captured, buffered,
fast-transcribed, and (on end token) accurately re-transcribed; only at dictation time does
`conn.gateDictation` (`server/internal/gateway/conn.go:344`) split the text on the
`speech_gate` phrase and keep what follows. No phrase ⇒ the string is dropped.

Consequences of that placement:

- Ambient chatter still consumes the `maxHandsFreePCM` buffer (`stream.go:24`), so a long
  background conversation can fill the buffer before you ever address the machine.
- Ambient chatter is still echoed to the phone as a user transcript bubble — `stream.go:141`
  is **not** gated, only dictation is.
- Ambient chatter still pays for two Whisper passes.
- `hey buddy` commands bypass the gate entirely (`splitWakeAll` peels commands first,
  `stream.go:150`), as does barge-in stop (`stream.go:44`).
- The gate is inert unless the user has both added a `speech_gate` token to the catalogue
  (nothing is seeded — `spoken/match.go` `DefaultTokens`) and turned on the per-device
  `dictation_gate` flag in `hello` (`gateway/auth.go:39`).

## Wanted

The gate becomes the entry condition for the whole pipeline. While closed, audio is scored
for the gate phrase and **nothing else is retained** — no buffered PCM, no draft, no bubble,
no accurate pass. The moment the gate opens, normal capture begins and everything after it
behaves exactly as it does today: gate → `hey buddy` command, or gate → dictation, either
way terminated by the end token.

Target shape of an utterance: `<chatter…> <gate> [hey buddy <cmd> | <dictation>] <end token>`.

## In scope

1. **Gate state on the connection.** A `gateOpen bool` (plus the clip index/offset where it
   opened) on `conn`, reset to closed on every `commitMessage`, on session switch, and on
   an idle timeout so a gate opened by a misfire can't hang open forever.
2. **Rework `gatedChunk` into two modes** (`server/internal/gateway/stream.go:15`):
   - *closed*: score this clip only. Do not append to `c.audioPCM`, do not append to
     `c.buffer`, do not `msgPending`, do not echo. If the gate phrase is found, open the
     gate and keep this clip's audio from the phrase onward (see #4).
   - *open*: exactly today's behaviour — buffer, draft, watch the end token, commit.
3. **Cheap gate scoring.** Reuse the end-token machinery: let a `speech_gate` token carry a
   detector model, and score it with the same `srv.detector` path as `endTokenFired`
   (`stream.go:78`). With a detector configured, the closed state never calls Whisper at
   all — that is the real cost win. With no detector, fall back to the tiny fast pass as a
   pure matcher whose output is discarded, matching today's accuracy.
4. **The straddling clip.** The clip containing the gate phrase also contains the first
   words after it. Keep that whole clip's audio and let the existing text-level
   `command.SplitOn` on the accurate transcript strip everything up to and including the
   phrase — so the audio boundary can be sloppy and the text stays exact.
5. **Gate the echo.** `msgTranscript` (`stream.go:141`) must not fire for pre-gate speech.
   With #2 this falls out for free, since closed clips never reach `commitMessage`.
6. **Decide the wake-word relationship.** Proposal: `hey buddy` commands require the gate
   too when the gate is on (the user's stated model is gate-then-wake), with **one
   exception** — barge-in `hey buddy stop` stays ungated, because being unable to stop
   runaway TTS without first saying the gate phrase is a trap. Flagged as the one behaviour
   change that needs the user's sign-off before coding.
7. **Push-to-talk and mic lock stay ungated.** A held or locked mic is already an explicit
   front door; requiring a spoken gate on top of it would be silly. Gate applies to the
   hands-free path only.
8. **Docs, in the same pass.** `README.md` (how it feels to use), `docs/architecture.md`
   (the gate's new position in the data flow), `docs/protocol.md` if any wire field changes,
   `docs/config.md` if a timeout env var is added.
9. **Tests.** Extend `server/internal/gateway/stream_test.go:59` — closed-gate clips retain
   nothing (no PCM growth, no pending, no transcript), gate-then-dictation, gate-then-wake,
   ungated barge-in, gate auto-close on commit, idle-timeout close.

## Out of scope

- Training the actual gate detector model (separate task; the code path is built to accept
  one, seeded empty).
- Changing the end token, the wake word grammar, or the cancel semantics.
- Client-side changes beyond whatever #6 implies; VAD stays as it is, and the gate stays
  server-side so both clients get it for free.
- Seeding a default `speech_gate` phrase.

## Risks

- **Missed gate = silence.** Today a missed gate loses one dictation; after this it also
  loses the audio, so there is no recovery and no transcript to show what was heard. Wants
  a visible "gate closed" state on the client, or at minimum a log line.
- **Detector false positives** open the gate on background speech, which is strictly worse
  than today because the following chatter is then treated as dictation until the end token.
  The idle timeout in #1 is the mitigation.
- **Whisper bias.** `detectBias`/`vocabBias` (`stream.go:228`, `:247`) already inject the
  gate phrase; the closed-state matcher must keep doing so or the phrase gets mis-heard.
