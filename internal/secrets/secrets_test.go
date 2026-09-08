package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}

	plaintext := []byte("super-secret-value")
	ciphertext, err := Encrypt(id.Recipient().String(), plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if string(ciphertext) == string(plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}

	got, err := Decrypt(id.String(), ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("decrypted = %q, want %q", got, plaintext)
	}
}

func TestDecryptWrongIdentityFails(t *testing.T) {
	id1, _ := age.GenerateX25519Identity()
	id2, _ := age.GenerateX25519Identity()

	ciphertext, err := Encrypt(id1.Recipient().String(), []byte("x"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := Decrypt(id2.String(), ciphertext); err == nil {
		t.Fatal("want error decrypting with the wrong identity")
	}
}

func TestLoadParsesAgeKeygenFormat(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}

	// Mirrors the file age-keygen writes: comments then the identity line.
	content := "# created: 2026-09-08T00:00:00Z\n" +
		"# public key: " + id.Recipient().String() + "\n" +
		id.String() + "\n"

	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.String() != id.String() {
		t.Fatalf("loaded identity = %q, want %q", loaded.String(), id.String())
	}
	if loaded.Recipient().String() != id.Recipient().String() {
		t.Fatalf("loaded recipient = %q, want %q", loaded.Recipient().String(), id.Recipient().String())
	}
}

func TestLoadMissingIdentityErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("# only comments\n"), 0600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("want error for key file with no identity line")
	}
}
