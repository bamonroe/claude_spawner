// Command gencommands writes docs/commands.json from the authoritative command
// registry (internal/command.Registry). Run it after changing the registry:
//
//	go run ./cmd/gencommands        # from the server/ module dir
//
// The Android build parses the emitted JSON to generate its command list, so the
// registry is the single source of truth for both server and app. A drift test
// (internal/command) fails if the committed docs/commands.json is stale.
package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/bam/claude_spawner/server/internal/command"
)

func main() {
	b, err := command.RegistryJSON()
	if err != nil {
		log.Fatalf("marshal registry: %v", err)
	}
	if err := os.WriteFile(commandsJSONPath(), b, 0o644); err != nil {
		log.Fatalf("write commands.json: %v", err)
	}
	log.Printf("wrote %s (%d commands)", commandsJSONPath(), len(command.Registry))
}

// commandsJSONPath resolves <repo>/docs/commands.json relative to this source
// file, so the generator works regardless of the working directory.
func commandsJSONPath() string {
	_, file, _, _ := runtime.Caller(0) // server/cmd/gencommands/main.go
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "docs", "commands.json")
}
