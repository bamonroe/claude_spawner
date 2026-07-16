package session

// nullTranscript is the transcriptReader for backends whose on-disk transcript
// isn't wired to a reader (currently Antigravity — see TranscriptAntigravity). It
// reports no history, no context usage, and deletes nothing, so a session on such
// a backend degrades cleanly: its live turn replies still stream off stdout, but
// past-turn scrollback/context/deletion are simply empty rather than mistakenly
// reading another backend's co-located transcripts.
type nullTranscript struct{}

func (nullTranscript) readTranscriptChain(ids []string) ([]Message, error) { return nil, nil }
func (nullTranscript) lastContextUsage(ids []string) *ContextSnapshot       { return nil }
func (nullTranscript) deleteByIDs(ids []string) (int, error)                { return 0, nil }
