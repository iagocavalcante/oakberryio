// Package secrets wraps filippo.io/age for oakd: encrypting app secrets to
// the host's public key at rest in SQLite, and decrypting them into a
// machine's boot environment.
package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

// Encrypt encrypts plaintext to recipient, an age X25519 public key
// ("age1...").
func Encrypt(recipient string, plaintext []byte) ([]byte, error) {
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("secrets: parse recipient: %w", err)
	}

	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return nil, fmt.Errorf("secrets: encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("secrets: write plaintext: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("secrets: close ciphertext writer: %w", err)
	}
	return buf.Bytes(), nil
}

// Decrypt decrypts ciphertext with identity, an age X25519 secret key
// ("AGE-SECRET-KEY-1...").
func Decrypt(identity string, ciphertext []byte) ([]byte, error) {
	id, err := age.ParseX25519Identity(identity)
	if err != nil {
		return nil, fmt.Errorf("secrets: parse identity: %w", err)
	}

	r, err := age.Decrypt(bytes.NewReader(ciphertext), id)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt: %w", err)
	}
	plaintext, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("secrets: read plaintext: %w", err)
	}
	return plaintext, nil
}

// Load reads the host's age identity from keyFile, in the format age-keygen
// writes: a "# created: ..." comment, a "# public key: age1..." comment, and
// the AGE-SECRET-KEY-1... identity on its own line. The recipient (public
// key) is not parsed from the comment; it's derived directly from the
// identity via Identity.Recipient(), which is simpler and can't drift from
// the comment.
func Load(keyFile string) (*age.X25519Identity, error) {
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("secrets: read key file %s: %w", keyFile, err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, err := age.ParseX25519Identity(line)
		if err != nil {
			return nil, fmt.Errorf("secrets: parse identity in %s: %w", keyFile, err)
		}
		return id, nil
	}
	return nil, fmt.Errorf("secrets: no identity line found in %s", keyFile)
}
