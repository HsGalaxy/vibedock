package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const keySize = 32

type Keyring struct {
	aead cipher.AEAD
}

func New(path, configured string) (*Keyring, error) {
	key, err := loadKey(path, configured)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create credential AEAD: %w", err)
	}
	return &Keyring{aead: aead}, nil
}

func (k *Keyring) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate credential nonce: %w", err)
	}
	sealed := k.aead.Seal(nil, nonce, []byte(plaintext), nil)
	payload := append(nonce, sealed...)
	return "v1:" + base64.RawURLEncoding.EncodeToString(payload), nil
}

func (k *Keyring) Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	encoded, ok := strings.CutPrefix(ciphertext, "v1:")
	if !ok {
		return "", errors.New("unsupported credential ciphertext version")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode credential ciphertext: %w", err)
	}
	if len(payload) < k.aead.NonceSize() {
		return "", errors.New("credential ciphertext is truncated")
	}
	nonce, sealed := payload[:k.aead.NonceSize()], payload[k.aead.NonceSize():]
	plaintext, err := k.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", errors.New("credential decryption failed; check VIBEDOCK_CREDENTIAL_KEY")
	}
	return string(plaintext), nil
}

func loadKey(path, configured string) ([]byte, error) {
	if strings.TrimSpace(configured) != "" {
		key, err := parseKey(strings.TrimSpace(configured))
		if err != nil {
			return nil, fmt.Errorf("parse VIBEDOCK_CREDENTIAL_KEY: %w", err)
		}
		return key, nil
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("credential key path is empty")
	}
	data, err := os.ReadFile(path)
	if err == nil {
		key, parseErr := parseKey(strings.TrimSpace(string(data)))
		if parseErr != nil {
			return nil, fmt.Errorf("parse credential key file: %w", parseErr)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read credential key: %w", err)
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate credential key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create credential key directory: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(key) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("write credential key: %w", err)
	}
	return key, nil
}

func parseKey(raw string) ([]byte, error) {
	decoders := []func(string) ([]byte, error){
		base64.RawURLEncoding.DecodeString,
		base64.StdEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, decode := range decoders {
		value, err := decode(raw)
		if err == nil && len(value) == keySize {
			return value, nil
		}
	}
	return nil, fmt.Errorf("key must decode to %d bytes (base64url, base64, or hex)", keySize)
}
