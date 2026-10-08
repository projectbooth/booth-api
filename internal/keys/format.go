// Package keys issues, stores, revokes and verifies booth-api's API keys (ADR 0100): created in the
// module's UI, shown once, stored only as a hash, scoped to datasets, revocable.
//
// A key is `booth_ak_<id>_<secret>`. The id is public (it is how a key is listed, revoked and
// looked up), the secret is 256 random bits and is never stored: only SHA-256(secret) is. A slow
// password hash (bcrypt, argon2) buys nothing here, because it exists to slow down guessing a
// low-entropy human password; nobody can enumerate 2^256 secrets, so a single fast hash is the
// standard choice for random API tokens and keeps verification cheap on every request.
//
// The `booth_ak_` prefix makes a key recognisable in a leaked log or a secret scanner, and tells it
// apart from a JWT in the same `Authorization: Bearer` header (ADR 0101).
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"strings"
)

// Prefix starts every booth-api key.
const Prefix = "booth_ak_"

const (
	idBytes     = 8  // 13 base32 characters
	secretBytes = 32 // 256 bits
	idLen       = 13
)

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrMalformed is returned for anything that is not shaped like a booth-api key.
var ErrMalformed = errors.New("malformed API key")

// newKey returns a fresh key's public id, its full presented form, and the hash to store.
func newKey() (id, presented string, hash []byte, err error) {
	raw := make([]byte, idBytes+secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", nil, err
	}
	id = strings.ToLower(idEncoding.EncodeToString(raw[:idBytes]))
	secret := base64.RawURLEncoding.EncodeToString(raw[idBytes:])
	return id, Prefix + id + "_" + secret, hashSecret(secret), nil
}

// parse splits a presented key into its id and secret. The id has a fixed length, so a secret
// containing '_' (base64url allows it) is not ambiguous.
func parse(presented string) (id, secret string, err error) {
	rest, ok := strings.CutPrefix(presented, Prefix)
	if !ok || len(rest) < idLen+2 || rest[idLen] != '_' {
		return "", "", ErrMalformed
	}
	id, secret = rest[:idLen], rest[idLen+1:]
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return "", "", ErrMalformed
		}
	}
	if b, err := base64.RawURLEncoding.DecodeString(secret); err != nil || len(b) != secretBytes {
		return "", "", ErrMalformed
	}
	return id, secret, nil
}

func hashSecret(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// matches compares in constant time, so response timing reveals nothing about the stored hash.
func matches(secret string, stored []byte) bool {
	return subtle.ConstantTimeCompare(hashSecret(secret), stored) == 1
}
