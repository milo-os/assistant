package usage

// ProjectRef references a Milo project. The pipeline uses it to attribute the
// event to a BillingAccountBinding.
type ProjectRef struct {
	Name string
}

// ResourceRef references the resource that emitted the event. Its ProjectRef
// MUST equal the event's top-level ProjectRef (the gateway rejects mismatches).
type ResourceRef struct {
	ProjectRef ProjectRef
	Group      string
	Kind       string
	Namespace  string
	Name       string
	UID        string
}

// EventResource is the full resource block on a usage event: a reference plus
// a point-in-time descriptive label set.
type EventResource struct {
	Ref    ResourceRef
	Labels map[string]string
}

// Event is the service-internal usage event shape (ergonomic for the builders
// and unit-testable without CloudEvents trivia). It is bridged onto the wire by
// [ToCloudEvent], the only place that knows the CloudEvents envelope.
//
// EventID is the end-to-end dedup key — generated once per logical sample and
// reused on retry — and must parse as a ULID.
type Event struct {
	EventID    string
	MeterName  string
	Timestamp  string // ISO-8601
	ProjectRef ProjectRef
	Value      string // numeric value as a string (wire spec)
	Dimensions map[string]string
	Resource   EventResource
}

// CloudEvent is the CloudEvents v1.0 envelope posted to the billing Ingestion
// Gateway. Field DECLARATION ORDER matches the TS emitter's object-literal
// order (id, specversion, type, source, subject, datacontenttype, time, data)
// so a raw byte compare against the TS wire is order-identical; the QA sink
// normalizer additionally sorts keys, so either way this is byte-compatible.
//
// Gateway-enforced rules: id is a ULID; specversion is "1.0"; subject is
// projects/{name}; datacontenttype is exactly application/json; data.value is a
// base-10 int64 string.
type CloudEvent struct {
	ID              string         `json:"id"`
	SpecVersion     string         `json:"specversion"`
	Type            string         `json:"type"`
	Source          string         `json:"source"`
	Subject         string         `json:"subject"`
	DataContentType string         `json:"datacontenttype"`
	Time            string         `json:"time"`
	Data            CloudEventData `json:"data"`
}

// CloudEventData is the CloudEvents data payload. dimensions and resource are
// omitted when empty, matching the TS builder's conditional spread.
type CloudEventData struct {
	Value      string              `json:"value"`
	Dimensions map[string]string   `json:"dimensions,omitempty"`
	Resource   *CloudEventResource `json:"resource,omitempty"`
}

// CloudEventResource is the monitored-resource block; uid is omitted when empty.
type CloudEventResource struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
}
