package command

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Command is the canonical definition of one "hey buddy" control command: its
// intent Kind, a display Title, the spoken phrasings that trigger it (shown in
// the app's command reference and used to anchor the per-command alias editor),
// a short user-facing Description, and one canonical Example utterance.
//
// Registry (below) is the SINGLE SOURCE OF TRUTH for the command set. A command
// must appear here to be documented, shown in the app, and — per registry_test —
// recognized by Parse. Adding a command means adding an entry here; the Android
// build generates its command list from the JSON emitted off this registry (see
// cmd/gencommands), so the app can never drift out of sync or ship an
// undocumented command.
type Command struct {
	Kind        Kind     `json:"-"`
	Title       string   `json:"title"`       // display name, e.g. "read last"
	Aliases     []string `json:"aliases"`     // spoken phrasings, e.g. "clear context"
	Description string   `json:"description"` // one-line user-facing description
	Example     string   `json:"-"`           // a parseable utterance; must Parse to Kind (test-enforced)
}

// Registry is the authoritative, ordered set of user-facing control commands.
// Keep it in sync with Parse (registry_test enforces that every Example parses
// to its Kind and that every user-facing Kind is registered) and with
// docs/commands.md. The app renders these alphabetically; order here is for
// human readability only.
var Registry = []Command{
	{Spawn, "spawn", []string{"spawn a new session", "spawn a session in <dir>", "new project in <dir>"},
		"Start a new session or project", "spawn a new session"},
	{Attach, "attach", []string{"attach to <name>"},
		"Attach to a session by name", "attach to bam"},
	{Detach, "detach", []string{"detach", "stop dictating"},
		"Leave the current session", "detach"},
	{List, "list", []string{"list sessions", "what sessions"},
		"List your sessions", "list sessions"},
	{Status, "status", []string{"status", "what's it doing"},
		"What the attached session is doing", "status"},
	{Kill, "kill", []string{"kill session <name>"},
		"Delete a session by name", "kill session bam"},
	{ReadLast, "read last", []string{"read last", "read last 3", "say that again", "repeat that"},
		"Re-read Claude's recent replies aloud", "read last"},
	{Clear, "clear", []string{"clear context", "clear session", "reset context", "start fresh"},
		"Start Claude fresh — keeps your history, stops re-reading it", "clear context"},
	{Cancel, "cancel", []string{"cancel", "never mind"},
		"Discard the message you're composing", "cancel"},
	{Stop, "stop", []string{"stop", "quiet", "stop talking"},
		"Barge-in: stop the voice reading aloud", "stop talking"},
	{AbortTurn, "abort", []string{"stop the turn", "cancel the turn", "abort"},
		"Cancel the running Claude turn", "stop the turn"},
	{Help, "help", []string{"help", "what can you do"},
		"Speak the list of commands", "help"},
}

// RegistryJSON returns the registry as indented JSON (an object with a
// "commands" array of {title, aliases, description}), sorted alphabetically by
// title. This is the artifact the Android build parses to generate its command
// list, so it is the app's source of truth. The generator (cmd/gencommands)
// writes it to docs/commands.json; a drift test asserts the committed file
// matches, so it can never fall stale.
func RegistryJSON() ([]byte, error) {
	sorted := append([]Command(nil), Registry...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Title < sorted[j].Title })
	// Use an Encoder with HTML escaping off so placeholders like "<name>" stay
	// literal instead of "<name>". Encode appends a trailing newline.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Commands []Command `json:"commands"`
	}{sorted}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
