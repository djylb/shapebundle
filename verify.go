package shapebundle

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// SignatureHeader is the response header that carries a bundle's
// signature: the Ed25519 signature of the raw response body, in hex.
const SignatureHeader = "X-Bundle-Signature"

// ErrSignature reports a missing or invalid signature.
var ErrSignature = errors.New("shapebundle: bad signature")

// Verify checks sig, the hex Ed25519 signature of body, against pub.
func Verify(pub ed25519.PublicKey, body []byte, sig string) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("shapebundle: public key of %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	sig = strings.TrimSpace(sig)
	if sig == "" {
		return fmt.Errorf("%w: none", ErrSignature)
	}
	if len(sig) != 2*ed25519.SignatureSize {
		return fmt.Errorf("%w: %d hex digits, want %d", ErrSignature, len(sig), 2*ed25519.SignatureSize)
	}
	s, err := hex.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSignature, err)
	}
	if !ed25519.Verify(pub, body, s) {
		return ErrSignature
	}
	return nil
}

// ParsePublicKey decodes a hex Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("shapebundle: public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("shapebundle: public key of %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return b, nil
}
