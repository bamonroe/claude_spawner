package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestIdentityStoreCreateListDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identities.json")
	keys := filepath.Join(dir, "keys")
	s, err := OpenIdentityStore(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatal("fresh store should be empty")
	}

	id, err := s.Create("work")
	if err != nil {
		t.Fatal(err)
	}
	// Public key is a parseable authorized_keys line ending in the identity name.
	if _, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(id.PublicKey)); perr != nil {
		t.Fatalf("public key not a valid authorized_keys line: %v (%q)", perr, id.PublicKey)
	}
	if !strings.HasSuffix(id.PublicKey, " work") {
		t.Fatalf("public key should carry the name as comment: %q", id.PublicKey)
	}
	// The private key exists (0600) at the derived path and is a usable signer.
	keyPath := s.KeyPath("work")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("private key not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key perms = %v, want 0600", info.Mode().Perm())
	}
	pemBytes, _ := os.ReadFile(keyPath)
	if _, perr := ssh.ParsePrivateKey(pemBytes); perr != nil {
		t.Fatalf("private key not parseable: %v", perr)
	}

	// A duplicate name is rejected (regenerating would invalidate the trusted key).
	if _, derr := s.Create("work"); derr == nil {
		t.Fatal("duplicate identity should error")
	}
	if _, derr := s.Create(""); derr == nil {
		t.Fatal("empty name should error")
	}

	// Persistence survives a reopen (public key only; private stays on disk).
	s2, err := OpenIdentityStore(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get("work"); got == nil || got.PublicKey != id.PublicKey {
		t.Fatalf("reloaded identity wrong: %+v", got)
	}

	// Delete removes the entry and the private key file.
	if derr := s2.Delete("work"); derr != nil {
		t.Fatal(derr)
	}
	if _, serr := os.Stat(keyPath); !os.IsNotExist(serr) {
		t.Fatalf("private key should be gone, stat err = %v", serr)
	}
	s3, _ := OpenIdentityStore(path, keys)
	if len(s3.List()) != 0 {
		t.Fatal("delete should persist")
	}

	// Import: register an existing on-disk private key as a managed identity.
	src, err := s3.Create("src") // reuse Create to mint a real key file on disk
	if err != nil {
		t.Fatal(err)
	}
	imported, err := s3.Import("copied", s3.KeyPath("src"))
	if err != nil {
		t.Fatal(err)
	}
	// Same underlying key → same public material (bar the trailing name comment).
	srcBody := src.PublicKey[:strings.LastIndex(src.PublicKey, " ")]
	impBody := imported.PublicKey[:strings.LastIndex(imported.PublicKey, " ")]
	if srcBody != impBody {
		t.Fatalf("imported key differs from source: %q vs %q", impBody, srcBody)
	}
	if _, serr := os.Stat(s3.KeyPath("copied")); serr != nil {
		t.Fatalf("imported private key not written: %v", serr)
	}
	// Importing onto a taken name, or from a bad path, errors.
	if _, ierr := s3.Import("copied", s3.KeyPath("src")); ierr == nil {
		t.Fatal("duplicate import should error")
	}
	if _, ierr := s3.Import("nope", filepath.Join(dir, "does-not-exist")); ierr == nil {
		t.Fatal("import from missing path should error")
	}
}
