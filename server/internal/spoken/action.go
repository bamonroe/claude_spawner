// Package spoken defines the "spoken token" model: a small, code-defined set of
// ACTIONS a spoken phrase can trigger (wake a command, end/commit a message, open
// the dictation gate) and the app-managed TOKENS that bind phrases — and, when the
// dedicated detector sidecar is enabled, ONNX models — to those actions.
//
// The action set is closed and lives here in code; it is NOT user-editable. It is
// a registry, though, so introducing a future action is a single slice entry —
// nothing else in the server hardcodes the set. The token LIST, by contrast, is
// app-managed and server-persisted (session.SpokenTokenStore), synced to clients
// like the profile/host catalogues; the available actions are advertised to the
// app so its token editor knows which features a phrase can be bound to.
//
// Several tokens may share one action — "hey buddy" and "hey gecko" both waking a
// command — so they are independent ways to reach a feature, NOT sound-alike
// aliases of one phrase. A token with a Model is scored by that model when the
// wakeword sidecar is on; a token with no Model falls back to Whisper
// string-matching its phrase.
package spoken

// Action ids — the wire values carried in Token.Action and advertised to the app.
const (
	ActionWake       = "wake"        // flags an utterance as a control command (the wake token)
	ActionEnd        = "end"         // commits the hands-free buffer (the end token)
	ActionSpeechGate = "speech_gate" // opens the dictation gate (the speak token)
)

// Action describes one bindable feature for the app's token editor.
type Action struct {
	ID    string `json:"id"`    // stable wire id (one of the Action* constants)
	Label string `json:"label"` // human name shown in the app
	Desc  string `json:"desc"`  // one-line description of what a token bound here does
}

// actions is the closed registry, in display order. Add a row to introduce a new
// bindable action; nothing else here hardcodes the set.
var actions = []Action{
	{ID: ActionWake, Label: "Wake", Desc: `Starts a spoken command, e.g. "hey buddy".`},
	{ID: ActionEnd, Label: "End", Desc: `Commits a hands-free message, e.g. "beep".`},
	{ID: ActionSpeechGate, Label: "Speech gate", Desc: "Opens the dictation gate; only speech after it is dictated."},
}

// Actions returns a copy of the action registry, for advertising to the app.
func Actions() []Action { return append([]Action(nil), actions...) }

// IsAction reports whether id names a known action.
func IsAction(id string) bool {
	for _, a := range actions {
		if a.ID == id {
			return true
		}
	}
	return false
}
