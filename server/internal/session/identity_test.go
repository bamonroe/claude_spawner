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

	id, err := s.Create("work", "bam", "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if id.User != "bam" {
		t.Fatalf("identity user = %q, want bam", id.User)
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
	if _, derr := s.Create("work", "bam", "", true, 0); derr == nil {
		t.Fatal("duplicate identity should error")
	}
	if _, derr := s.Create("", "bam", "", true, 0); derr == nil {
		t.Fatal("empty name should error")
	}
	if _, derr := s.Create("nouser", "", "", true, 0); derr == nil {
		t.Fatal("empty username should error")
	}
	// A keyless identity must carry a password; with a password it's allowed.
	if _, derr := s.Create("empty", "bam", "", false, 0); derr == nil {
		t.Fatal("keyless + passwordless identity should error")
	}
	pw, perr := s.Create("pw", "bam", "secret", false, 0)
	if perr != nil {
		t.Fatal(perr)
	}
	if pw.PublicKey != "" {
		t.Fatalf("password-only identity should have no public key: %q", pw.PublicKey)
	}
	if _, serr := os.Stat(s.KeyPath("pw")); !os.IsNotExist(serr) {
		t.Fatalf("password-only identity should write no key file, stat err = %v", serr)
	}

	// Persistence survives a reopen (public key only; private stays on disk).
	s2, err := OpenIdentityStore(path, keys)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get("work"); got == nil || got.PublicKey != id.PublicKey || got.User != "bam" {
		t.Fatalf("reloaded identity wrong: %+v", got)
	}
	// The password persists on disk (server-side only).
	if got := s2.Get("pw"); got == nil || got.Password != "secret" {
		t.Fatalf("reloaded password-only identity wrong: %+v", got)
	}

	// Delete removes the entry and the private key file (leaving the password-only one).
	if derr := s2.Delete("work", 1); derr != nil {
		t.Fatal(derr)
	}
	if _, serr := os.Stat(keyPath); !os.IsNotExist(serr) {
		t.Fatalf("private key should be gone, stat err = %v", serr)
	}
	s3, _ := OpenIdentityStore(path, keys)
	if s3.Get("work") != nil || len(s3.List()) != 1 {
		t.Fatalf("delete should persist, leaving only pw: %v", s3.List())
	}

	// Update: change the user (keeping the key), and set/clear the password.
	if _, uerr := s3.Update("pw", "root", false, "", 0); uerr != nil {
		t.Fatal(uerr)
	}
	if got := s3.Get("pw"); got.User != "root" || got.Password != "secret" {
		t.Fatalf("update should change user, keep password: %+v", got)
	}
	if _, uerr := s3.Update("pw", "root", true, "newpw", 0); uerr != nil {
		t.Fatal(uerr)
	}
	if s3.Get("pw").Password != "newpw" {
		t.Fatal("update should set the new password")
	}
	// Clearing a key-less identity's password is rejected (it'd have no auth left).
	if _, uerr := s3.Update("pw", "root", true, "", 0); uerr == nil {
		t.Fatal("clearing a key-less identity's password should error")
	}
	if _, uerr := s3.Update("pw", "", false, "", 0); uerr == nil {
		t.Fatal("empty user on update should error")
	}

	// Import: register an existing on-disk private key as a managed identity.
	src, err := s3.Create("src", "bam", "", true, 0) // reuse Create to mint a real key file on disk
	if err != nil {
		t.Fatal(err)
	}
	imported, err := s3.Import("copied", "bam", "", s3.KeyPath("src"), 0)
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
	if _, ierr := s3.Import("copied", "bam", "", s3.KeyPath("src"), 0); ierr == nil {
		t.Fatal("duplicate import should error")
	}
	if _, ierr := s3.Import("nope", "bam", "", filepath.Join(dir, "does-not-exist"), 0); ierr == nil {
		t.Fatal("import from missing path should error")
	}
}
