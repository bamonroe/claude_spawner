package spoken

// Token binds a spoken phrase to an action. It is one record of the app-managed
// spoken-token catalogue (session.SpokenTokenStore persists the list); the app is
// the source of truth and the server re-broadcasts it on change, exactly like the
// host/profile catalogues.
type Token struct {
	// Name is the stable record key (what put/delete address; the app generates it).
	Name string `json:"name"`
	// Phrase is the spoken words to match, e.g. "hey buddy" — matched whole-word,
	// case- and punctuation-insensitive (see match.go).
	Phrase string `json:"phrase"`
	// Action is the feature this phrase triggers — one of the Action ids.
	Action string `json:"action"`
	// Model is the optional dedicated-detector (ONNX) model key that scores this
	// token when the wakeword sidecar is enabled; empty falls back to Whisper
	// string-matching the phrase.
	Model string `json:"model,omitempty"`
	// UpdatedAt is the client-stamped last-edit time in unix MILLISECONDS, driving
	// last-writer-wins arbitration in the store (see session/hosts.go).
	UpdatedAt int64 `json:"updated_at,omitempty"`
}
