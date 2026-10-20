package a2a

import "regexp"

// SurfaceHomeFirstRun is the cloud-portal project home page while the project
// is being set up for the first time.
const SurfaceHomeFirstRun = "home-first-run"

// knownSurfaces is the allowlist of surface names a caller may send. A surface
// selects a fixed, server-owned prompt section, so a name outside this set has
// nothing to select and is ignored.
var knownSurfaces = map[string]bool{
	SurfaceHomeFirstRun: true,
}

// dnsLabel is a DNS-1123 label, the shape of a network's object name. The
// network is the only caller-supplied text that reaches the prompt, so it is
// held to the exact shape a real name has rather than trimmed or escaped.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Surface says where a conversation is happening, so the agent can adapt how
// it answers there. It arrives from the client and is untrusted until it has
// been through [ValidateSurface].
type Surface struct {
	Name    string `json:"name"`
	Network string `json:"network,omitempty"`
}

// ValidateSurface returns the part of s that is safe to act on. An unknown or
// missing name yields nil; a network that is not a DNS-1123 label is dropped
// while the rest of the surface is kept. note describes what was discarded (or
// is empty when nothing was) so the caller can log it. Neither case is an
// error: a surface only tunes the answer, and a stale or odd client should
// still get one.
func ValidateSurface(s *Surface) (valid *Surface, note string) {
	if s == nil {
		return nil, ""
	}
	if !knownSurfaces[s.Name] {
		return nil, "unknown surface name"
	}
	out := &Surface{Name: s.Name}
	if s.Network == "" {
		return out, ""
	}
	if !dnsLabel.MatchString(s.Network) {
		return out, "network is not a DNS-1123 label"
	}
	out.Network = s.Network
	return out, ""
}
