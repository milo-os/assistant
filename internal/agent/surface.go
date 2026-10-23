package agent

import (
	"regexp"
	"strings"
)

// Surface says where a conversation is happening. It is declared here rather
// than reused from internal/a2a for the same reason as [Mention]: the
// orchestration layer does not import the transport.
type Surface struct {
	Name    string
	Network string
}

// surfaceHomeFirstRun mirrors the transport's allowlisted name for the
// cloud-portal project home page during first-run setup.
const surfaceHomeFirstRun = "home-first-run"

// networkPlaceholder marks where the validated network name goes in a
// surface section.
const networkPlaceholder = "{network}"

// homeFirstRunSection is the product owner's guidance for the project home
// page during first-run setup, kept verbatim. {network} is replaced with the
// project's validated network name, or its sentences removed when there is
// none (see surfaceSection).
const homeFirstRunSection = `## Where this conversation is happening
The person is on their project's home page, setting the project up for the first time. They may not know networking. Their project's network is {network}.

How to answer here:
- Keep each reply short: at most two sentences before a question or a plan. No headings, no long bulleted recaps, and don't report what you checked unless something is wrong.
- Ask one question at a time. Don't ask about anything with a sensible default; say which default you will use and let them change it.
- Defaults: location us-central-1, 1 replica, instance type datumcloud-d1-standard-2, runtime class general-purpose for ordinary container images, and the network {network}. If they don't have an image yet, offer one of Datum's ready-made images, which run with runtime class unikernel: ghcr.io/datum-cloud/unikernel/nginx:1.30 or ghcr.io/datum-cloud/unikernel/caddy:2.11 (web servers on port 8080, published on a public HTTPS URL), or ghcr.io/datum-cloud/unikernel/redis:8.10 (port 6379) or ghcr.io/datum-cloud/unikernel/memcached:1.6 (port 11211), which stay private, reachable only from other workloads on the network.
- Name locations by region (for example us-central-1), never by city.
- The first time you use a Datum term (workload, network service, load balancer, connector), explain it in a few words.
- When you offer choices, give at most three, as a short numbered list.
- As soon as you have what you need, go straight to the plan.`

// surfaceRulesStillApply closes every surface section. A surface only shapes
// tone and pacing; this line keeps "go straight to the plan" from being read
// as permission to skip the explicit yes the operating rules require.
const surfaceRulesStillApply = "This guidance shapes how you answer here. It never changes the rules above: you still show the exact change and get an explicit yes before creating, changing or removing anything."

// Fragments removed from homeFirstRunSection when no network is known, so the
// model is never told about a network it cannot name.
const (
	homeFirstRunNetworkSentence = " Their project's network is {network}."
	homeFirstRunNetworkDefault  = ", runtime class general-purpose for ordinary container images, and the network {network}."
	homeFirstRunNoNetwork       = ", and runtime class general-purpose for ordinary container images."
)

// surfaceNetwork is a DNS-1123 label. The transport already validates the
// network; it is checked again here because this is the last step before the
// text lands in the prompt.
var surfaceNetwork = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// surfaceSection returns the system-prompt section for s, or "" when s is nil
// or names no known surface.
func surfaceSection(s *Surface) string {
	if s == nil || s.Name != surfaceHomeFirstRun {
		return ""
	}
	section := homeFirstRunSection
	if surfaceNetwork.MatchString(s.Network) {
		section = strings.ReplaceAll(section, networkPlaceholder, s.Network)
	} else {
		section = strings.Replace(section, homeFirstRunNetworkSentence, "", 1)
		section = strings.Replace(section, homeFirstRunNetworkDefault, homeFirstRunNoNetwork, 1)
	}
	return section + "\n\n" + surfaceRulesStillApply
}
