package secrets

import (
	"path/filepath"
	"testing"
)

func TestKeyringRoundTripAndPersistentKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.key")
	first, err := New(path, "")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := first.Encrypt("sk-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if ciphertext == "sk-test-secret" || ciphertext == "" {
		t.Fatalf("unexpected ciphertext %q", ciphertext)
	}
	second, err := New(path, "")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := second.Decrypt(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext != "sk-test-secret" {
		t.Fatalf("unexpected plaintext %q", plaintext)
	}
}
