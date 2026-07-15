package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadTLSValidation covers the cert/key/CA cross-checks in Load. A token is
// always set so validation reaches the TLS clauses.
func TestLoadTLSValidation(t *testing.T) {
	tests := []struct {
		name    string
		cert    string
		key     string
		ca      string
		wantErr bool
	}{
		{"no tls (plain ws)", "", "", "", false},
		{"cert and key together", "c.pem", "k.pem", "", false},
		{"full mTLS", "c.pem", "k.pem", "ca.pem", false},
		{"cert without key", "c.pem", "", "", true},
		{"key without cert", "", "k.pem", "", true},
		{"client CA without server tls", "", "", "ca.pem", true},
		{"client CA with only cert", "c.pem", "", "ca.pem", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SPAWNER_TOKEN", "secret")
			t.Setenv("SPAWNER_TLS_CERT", tt.cert)
			t.Setenv("SPAWNER_TLS_KEY", tt.key)
			t.Setenv("SPAWNER_TLS_CLIENT_CA", tt.ca)
			_, err := Load()
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildTLSConfig(t *testing.T) {
	// TLS disabled → nil config, no error.
	c := &Config{}
	if tc, err := c.BuildTLSConfig(); err != nil || tc != nil {
		t.Fatalf("disabled: got (%v, %v), want (nil, nil)", tc, err)
	}

	// Server TLS, no client CA → a config that doesn't demand client certs.
	c = &Config{TLSCert: "c.pem", TLSKey: "k.pem"}
	tc, err := c.BuildTLSConfig()
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	if tc.ClientAuth != tls.NoClientCert {
		t.Errorf("without a CA, ClientAuth=%v, want NoClientCert", tc.ClientAuth)
	}

	// Bad CA path → error.
	c = &Config{TLSCert: "c.pem", TLSKey: "k.pem", TLSClientCA: filepath.Join(t.TempDir(), "missing.pem")}
	if _, err := c.BuildTLSConfig(); err == nil {
		t.Error("missing CA file should error")
	}

	// A file with no PEM certs → error.
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	c = &Config{TLSCert: "c.pem", TLSKey: "k.pem", TLSClientCA: empty}
	if _, err := c.BuildTLSConfig(); err == nil {
		t.Error("non-PEM CA file should error")
	}

	// A real CA PEM → mTLS: RequireAndVerifyClientCert with the CA in the pool.
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, selfSignedCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	c = &Config{TLSCert: "c.pem", TLSKey: "k.pem", TLSClientCA: caPath}
	tc, err = c.BuildTLSConfig()
	if err != nil {
		t.Fatalf("mTLS: %v", err)
	}
	if tc.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth=%v, want RequireAndVerifyClientCert", tc.ClientAuth)
	}
	if tc.ClientCAs == nil {
		t.Error("ClientCAs pool not set")
	}
}

// selfSignedCAPEM returns a minimal self-signed certificate in PEM form, enough
// for AppendCertsFromPEM to accept.
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<31, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
