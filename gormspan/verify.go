package gormspan

import (
	"crypto/ed25519"
	"encoding/base64"

	"github.com/rs/zerolog/log"
)

// verifyFunc returns a function suitable for registration as an SQLite
// scalar function. It performs Ed25519 signature verification:
//
//	verify(message TEXT, signature_b64 TEXT) → INTEGER
//
// Returns 1 if valid, 0 otherwise. This matches the behaviour of the
// original Rust C-extension and Python verify() implementations.
//
// The publicKeyB64 parameter is the base64url-encoded Ed25519 public
// key. If empty or invalid, the returned function always returns 0.
func verifyFunc(publicKeyB64 string) func(message, signatureB64 string) int64 {
	if publicKeyB64 == "" {
		log.Warn().Msg("gormspan: verify() public key not configured, function will always return 0")
		return func(_, _ string) int64 { return 0 }
	}

	pubBytes, err := base64.URLEncoding.DecodeString(publicKeyB64)
	if err != nil {
		// Try standard base64 as fallback.
		pubBytes, err = base64.StdEncoding.DecodeString(publicKeyB64)
		if err != nil {
			log.Error().Err(err).Msg("gormspan: failed to decode verify() public key")
			return func(_, _ string) int64 { return 0 }
		}
	}

	if len(pubBytes) != ed25519.PublicKeySize {
		log.Error().
			Int("got", len(pubBytes)).
			Int("want", ed25519.PublicKeySize).
			Msg("gormspan: verify() public key wrong length")
		return func(_, _ string) int64 { return 0 }
	}

	pubKey := ed25519.PublicKey(pubBytes)

	log.Info().Msg("gormspan: verify() SQL function configured")

	return func(message, signatureB64 string) int64 {
		sigBytes, err := base64.URLEncoding.DecodeString(signatureB64)
		if err != nil {
			sigBytes, err = base64.StdEncoding.DecodeString(signatureB64)
			if err != nil {
				return 0
			}
		}

		if ed25519.Verify(pubKey, []byte(message), sigBytes) {
			return 1
		}
		return 0
	}
}
