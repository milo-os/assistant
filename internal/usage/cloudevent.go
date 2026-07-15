package usage

// ToCloudEvent converts a service-internal [Event] into the CloudEvents v1.0
// envelope the billing Ingestion Gateway requires. This is the single seam
// between service-internal data and the platform wire format — every field the
// gateway validates is set here. Ported verbatim from the TS to-cloud-event.ts.
func ToCloudEvent(e Event, source string) CloudEvent {
	ce := CloudEvent{
		ID:              e.EventID,
		SpecVersion:     "1.0",
		Type:            e.MeterName,
		Source:          source,
		Subject:         "projects/" + e.ProjectRef.Name,
		DataContentType: "application/json",
		Time:            e.Timestamp,
		Data: CloudEventData{
			Value: e.Value,
			Resource: &CloudEventResource{
				Group:     e.Resource.Ref.Group,
				Kind:      e.Resource.Ref.Kind,
				Namespace: e.Resource.Ref.Namespace,
				Name:      e.Resource.Ref.Name,
				UID:       e.Resource.Ref.UID,
			},
		},
	}
	// The TS builder spreads dimensions only when non-empty; omitempty on the
	// map field reproduces that (a nil/empty map drops the key entirely).
	if len(e.Dimensions) > 0 {
		ce.Data.Dimensions = e.Dimensions
	}
	return ce
}
