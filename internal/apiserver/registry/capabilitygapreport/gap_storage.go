package capabilitygapreport

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/milo-os/assistant/internal/gapreport"
	"github.com/milo-os/assistant/internal/tenant"
	"github.com/milo-os/assistant/pkg/apis/assistant"
)

var gapsResource = assistant.Resource("capabilitygaps")

// CapabilityGapREST serves the aggregate view over the same store and the
// same namespace rule as [CapabilityGapReportREST]: one item per distinct
// gap, so a provider's team reads "one gap, seven conversations" instead of
// counting seven rows that describe it seven different ways.
//
// It is a second view, not a replacement. The occurrence rows stay listable
// as capabilitygapreports and are where the per-occurrence evidence lives —
// three MisleadingOutput reports with three different contradictedBy values
// tell a team far more than a counter does, so the aggregate summarises them
// rather than standing in for them.
type CapabilityGapREST struct {
	store gapreport.Store
	rest.TableConvertor
}

var (
	_ rest.Storage              = (*CapabilityGapREST)(nil)
	_ rest.Scoper               = (*CapabilityGapREST)(nil)
	_ rest.Lister               = (*CapabilityGapREST)(nil)
	_ rest.SingularNameProvider = (*CapabilityGapREST)(nil)
)

// NewCapabilityGapREST builds the CapabilityGap REST over the given store.
func NewCapabilityGapREST(store gapreport.Store) *CapabilityGapREST {
	return &CapabilityGapREST{
		store:          store,
		TableConvertor: rest.NewDefaultTableConvertor(gapsResource),
	}
}

func (r *CapabilityGapREST) New() runtime.Object     { return &assistant.CapabilityGap{} }
func (r *CapabilityGapREST) NewList() runtime.Object { return &assistant.CapabilityGapList{} }
func (r *CapabilityGapREST) Destroy()                {}
func (r *CapabilityGapREST) NamespaceScoped() bool   { return true }
func (r *CapabilityGapREST) GetSingularName() string { return "capabilitygap" }

// List returns the caller's provider project's distinct gaps, most-hit first.
// Selectors are not supported in v1, same as capabilitygapreports.
func (r *CapabilityGapREST) List(ctx context.Context, _ *metainternalversion.ListOptions) (runtime.Object, error) {
	providerProject, err := tenant.ProjectFromContext(ctx, gapsResource)
	if err != nil {
		return nil, err
	}
	groups, err := r.store.Aggregate(ctx, providerProject)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	list := &assistant.CapabilityGapList{Items: make([]assistant.CapabilityGap, 0, len(groups))}
	for _, g := range groups {
		list.Items = append(list.Items, *newCapabilityGap(providerProject, g))
	}
	return list, nil
}

// newCapabilityGap maps one aggregated gap to the internal API object.
// Deliberately carries no consumer project and no context id: the count of
// conversations is the prioritisation signal, and which customers they were
// is a question this view does not answer — see [gapreport.Aggregate].
func newCapabilityGap(providerProject string, g gapreport.Aggregate) *assistant.CapabilityGap {
	kind := g.Kind
	if kind == "" {
		kind = gapreport.KindMissingCapability
	}
	return &assistant.CapabilityGap{
		ObjectMeta: metav1.ObjectMeta{
			// Named by the key, so the object identity is the gap itself and
			// stays stable as occurrences accumulate. A keyless report falls
			// back to its own id — it is a group of one.
			Name:              g.Key,
			Namespace:         providerProject,
			CreationTimestamp: metav1.NewTime(g.FirstSeen),
		},
		Status: assistant.CapabilityGapStatus{
			ServiceName:   g.ServiceName,
			CapabilityKey: g.CapabilityKey,
			Capability:    g.Capability,
			Kind:          string(kind),
			Conversations: int32(g.Conversations),
			Occurrences:   int32(g.Occurrences),
			FirstSeen:     metav1.NewTime(g.FirstSeen),
			LastSeen:      metav1.NewTime(g.LastSeen),
		},
	}
}
