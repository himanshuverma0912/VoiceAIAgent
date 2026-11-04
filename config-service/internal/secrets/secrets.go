// Package secrets seals integration credentials (tokens, client secrets) so
// that only the voice agent can open them.
//
// The agent holds an X25519 private key (INTEGRATION_SECRETS_KEY); this
// service is given only the public half (INTEGRATION_SECRETS_PUBLIC_KEY). A
// secret typed into Flow Studio is sealed here the moment it arrives and is
// never stored, logged or returned in the clear: not even this service can
// read it back. flow_engine/secrets.py opens it during a call.
//
// Format: "v1." + base64(ephemeral public key (32) | nonce (12) | AES-256-GCM
// ciphertext), with the key from HKDF-SHA256 over the X25519 shared secret,
// salted with both public keys, and the integration id as additional data (a
// sealed secret can't be moved to another integration).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const (
	prefix = "v1."
	info   = "flow-studio integration secret v1"
)

// PublicKey reads a base64 X25519 public key (32 bytes).
func PublicKey(b64 string) (*ecdh.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("INTEGRATION_SECRETS_PUBLIC_KEY must be the 32-byte public key, base64 (python -m flow_engine.secrets keygen)")
	}
	return ecdh.X25519().NewPublicKey(raw)
}

func key(shared, ephemeral, recipient []byte) ([]byte, error) {
	salt := append(append([]byte{}, ephemeral...), recipient...)
	return hkdf.Key(sha256.New, shared, salt, info, 32)
}

// Seal encrypts plaintext for the holder of to's private key, bound to aad
// (the integration id).
func Seal(to *ecdh.PublicKey, aad, plaintext string) (string, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	shared, err := eph.ECDH(to)
	if err != nil {
		return "", err
	}
	k, err := key(shared, eph.PublicKey().Bytes(), to.Bytes())
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := append(append(eph.PublicKey().Bytes(), nonce...), gcm.Seal(nil, nonce, []byte(plaintext), []byte(aad))...)
	return prefix + base64.StdEncoding.EncodeToString(out), nil
}

// LooksSealed reports whether s has the shape of a sealed secret.
func LooksSealed(s string) bool {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	return strings.HasPrefix(s, prefix) && err == nil && len(raw) >= 32+12+16
}
