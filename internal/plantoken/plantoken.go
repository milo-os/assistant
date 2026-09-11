// Package plantoken proves that what is about to be applied is exactly what
// somebody was already shown.
//
// Authorization is a separate question, decided by the platform on the
// caller's own credential on every request. This package settles agreement. A
// plan returns a token hashing the manifests, their order, the project, and
// the resource version each manifest saw. Apply re-derives the hash and
// refuses any mismatch, so an edited manifest, a reordered list, another
// project's token, or an object somebody else changed cannot reach the
// platform. A change nobody was shown has no token at all.
//
// The token proves agreement covered this change. It cannot prove agreement
// happened; the platform prompt requires that human step.
package plantoken

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	// TTL bounds how long agreement stays good: long enough to read and
	// answer, short enough not to outlive the conversation it was given in.
	TTL = 15 * time.Minute

	// version prefixes the hashed payload so a later change to what the token
	// covers cannot be confused with today's.
	version = "plan.v1"

	// keyBytes is the size of a generated key.
	keyBytes = 32
)

// Binding is everything a plan assumed. All of it is hashed, so any of it
// moving invalidates the token.
type Binding struct {
	// Project scopes the token. One minted elsewhere cannot be spent here.
	Project string
	// Manifests are the canonical forms to apply, in apply order. Order is
	// part of the agreement: the same set reordered is a different plan.
	Manifests [][]byte
	// ResourceVersions is the version each manifest would replace,
	// positionally, and empty where it would create. A moved version means
	// somebody else changed the object, so the agreed change no longer
	// describes what would happen.
	ResourceVersions []string
}

// ResolveKey returns the key tokens are minted and checked with.
//
// A configured key survives restarts and is shared between replicas, so one
// process can apply another's plan. Without one, this process generates a
// random key: the guarantee still holds within the process, but a restart
// loses every outstanding plan and a sibling replica's plan is refused. That
// warrants a loud warning, and beats refusing to write or minting tokens with
// a guessable key.
func ResolveKey(configured string, logger *slog.Logger) ([]byte, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if configured = strings.TrimSpace(configured); configured != "" {
		// Accept base64 for a generated key, raw bytes for a long secret
		// string.
		if decoded, err := base64.StdEncoding.DecodeString(configured); err == nil && len(decoded) >= 16 {
			logger.Info("plantoken.key", "source", "configured", "encoding", "base64")
			return decoded, nil
		}
		if len(configured) < 16 {
			return nil, errors.New("plantoken: the configured key is too short to be a secret (want at least 16 bytes)")
		}
		logger.Info("plantoken.key", "source", "configured", "encoding", "raw")
		return []byte(configured), nil
	}

	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("plantoken: generating a key: %w", err)
	}
	logger.Warn("plantoken.key",
		"source", "generated",
		"reason", "PLAN_TOKEN_KEY is unset — a key was generated for this process only",
		"impact", "a plan cannot be applied after a restart, or by another replica; the person is asked to plan again")
	return key, nil
}

// Mint returns the token that authorizes applying exactly this plan until
// expiry.
//
// The form is base64(HMAC-SHA256(payload)) + "." + expiry in seconds. The
// expiry travels in the clear but is covered by the hash, so moving it
// invalidates the token rather than extending it.
func Mint(key []byte, binding Binding, expiry time.Time) string {
	unix := expiry.Unix()
	return base64.RawURLEncoding.EncodeToString(mac(key, binding, unix)) + "." + strconv.FormatInt(unix, 10)
}

// Verify refuses anything that is not a token minted for exactly this plan and
// still inside its window.
//
// A person reads every refusal, so each one says what happened, that nothing
// was changed, and what to do next. planTool names the tool that advice points
// at, so this package hard-codes no caller's vocabulary.
func Verify(key []byte, token string, binding Binding, now time.Time, planTool string) error {
	encoded, expiryText, found := strings.Cut(strings.TrimSpace(token), ".")
	if !found {
		return fmt.Errorf(
			"this is not a plan token in the form %s issues, so nothing was changed. Call %s with the "+
				"manifests to apply and use the token it returns", planTool, planTool)
	}
	unix, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil {
		return fmt.Errorf(
			"this plan token does not carry a readable expiry, so nothing was changed. Call %s again "+
				"and use the token it returns", planTool)
	}
	if expiry := time.Unix(unix, 0); now.After(expiry) {
		return fmt.Errorf(
			"this plan expired at %s and nothing was changed. A plan is good for %s, so that what gets "+
				"changed is still what was agreed to. Call %s again, show the person who asked the "+
				"manifests and the differences it returns, and ask again before applying",
			expiry.UTC().Format(time.RFC3339), TTL, planTool)
	}

	presented, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		presented = nil
	}
	if !hmac.Equal(presented, mac(key, binding, unix)) {
		return fmt.Errorf(
			"this plan token does not cover what it was given, so nothing was changed. That happens "+
				"when a manifest changed after the plan was made — one character is enough — when the "+
				"manifests were reordered, when the plan was made for a different project, or when one "+
				"of these resources was changed by somebody else in the meantime. This refusal is the "+
				"check working. Call %s again with the manifests to apply, show the person who asked "+
				"what it returns, and ask again", planTool)
	}
	return nil
}

// mac hashes everything the plan assumed. Each part is length-prefixed so no
// two bindings can be built from the same bytes; without it, manifests split
// differently would hash alike.
func mac(key []byte, binding Binding, expiryUnix int64) []byte {
	var payload bytes.Buffer
	writePart(&payload, []byte(version))
	writePart(&payload, []byte(binding.Project))
	writePart(&payload, []byte(strconv.Itoa(len(binding.Manifests))))
	for _, manifest := range binding.Manifests {
		writePart(&payload, manifest)
	}
	writePart(&payload, []byte(strconv.Itoa(len(binding.ResourceVersions))))
	for _, resourceVersion := range binding.ResourceVersions {
		writePart(&payload, []byte(resourceVersion))
	}
	writePart(&payload, []byte(strconv.FormatInt(expiryUnix, 10)))

	hash := hmac.New(sha256.New, key)
	hash.Write(payload.Bytes())
	return hash.Sum(nil)
}

func writePart(buf *bytes.Buffer, part []byte) {
	buf.WriteString(strconv.Itoa(len(part)))
	buf.WriteByte(':')
	buf.Write(part)
}
