// Package plantoken is the write gate: proof that what is about to be applied
// is exactly what somebody was already shown.
//
// The problem it solves is not authorization. Whether a caller may change a
// resource is decided by the platform, on the caller's own credential, on every
// request. The problem is agreement. A model proposes a change, a person reads
// it and says yes, and the model then calls apply — and between those two
// moments the model has read tool output, provider knowledge and status
// messages, any of which could try to talk it into applying something else.
//
// So a plan returns a token that is a hash of what the plan covered: the exact
// manifests, the project they belong to, and the version of each object the
// plan saw. Apply re-derives that hash from what it was actually handed and
// refuses anything that does not match. A manifest edited after the plan — one
// character is enough — a token minted for another project, or an object
// somebody else changed in the meantime all fail to match, so the only thing
// that can reach the platform is the change already put in front of the person
// who asked. A change nobody was shown has no token and cannot be applied at
// all.
//
// The token proves agreement was about THIS change. It cannot prove agreement
// happened: that is a human step, and the platform prompt is what requires it.
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
	// TTL is how long a token stays good: long enough for the change to be
	// shown and read and answered, short enough that agreement cannot outlive
	// the conversation it was given in.
	TTL = 15 * time.Minute

	// version prefixes the hashed payload so a future change to what is
	// covered cannot be confused with today's.
	version = "plan.v1"

	// keyBytes is the size of a generated key.
	keyBytes = 32
)

// Binding is everything a plan assumed. All of it is hashed, so any of it
// moving invalidates the token.
type Binding struct {
	// Project is the project the plan was made in. A token minted elsewhere
	// cannot be spent here.
	Project string
	// Manifests are the canonical forms of what would be applied, in the order
	// they would be applied in. Order is part of the agreement: the same set in
	// a different order is a different plan.
	Manifests [][]byte
	// ResourceVersions is the version of the object each manifest would
	// replace, positionally, and empty where the manifest would create one. A
	// version that has moved since the plan means somebody else changed the
	// object, and what the person agreed to is no longer what would happen.
	ResourceVersions []string
}

// ResolveKey returns the key tokens are minted and checked with.
//
// A configured key is what a real deployment uses: it survives a restart and is
// shared between replicas, so a plan made by one process can be applied by
// another. Without one a random key is generated for this process, which still
// enforces the whole guarantee within a process — and loses every outstanding
// plan on restart, and refuses a plan made by a sibling replica. That is a
// deployment worth warning about loudly, and much better than either refusing
// to write at all or minting tokens with a key an attacker could guess.
func ResolveKey(configured string, logger *slog.Logger) ([]byte, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if configured = strings.TrimSpace(configured); configured != "" {
		// Accept base64 for a key generated the obvious way, and raw bytes for
		// one that is simply a long secret string.
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

// Mint returns the token that authorizes applying exactly this plan, until
// expiry.
//
// The form is base64(HMAC-SHA256(payload)) + "." + the expiry in seconds. The
// expiry travels in the clear because it is also covered by the hash, so moving
// it invalidates the token rather than extending it.
func Mint(key []byte, binding Binding, expiry time.Time) string {
	unix := expiry.Unix()
	return base64.RawURLEncoding.EncodeToString(mac(key, binding, unix)) + "." + strconv.FormatInt(unix, 10)
}

// Verify refuses anything that is not a token minted for exactly this plan and
// still inside its window.
//
// Every refusal is read by a person, so each one says what happened, that
// nothing was changed, and what to do instead. planTool and applyTool are the
// tool names to name in that advice, so this package can be used by anything
// without hard-coding one caller's vocabulary.
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
// two different bindings can be built out of the same bytes — without that, two
// manifests could be split differently and hash the same.
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
