// Package capabilitygapreport is the bespoke read-only REST storage backing
// the conversations aggregated apiserver's capabilitygapreports resource.
// Like internal/apiserver/registry/conversation it wraps a shared relational
// store (internal/gapreport) directly — no etcd, no double-storage.
//
// The namespace here is the PROVIDER project (spec.reportingProject on the
// capability document that raised the gap), never the consumer project the
// conversation ran in — see internal/gapreport's package doc. tenant.
// ProjectFromContext enforces the SAME single-project-per-request rule as
// Conversation, just applied to a different axis: a caller's k8s identity is
// pinned to one project either way, and here that project is read as "which
// PROVIDER's reports may I see," not "which consumer project's conversations."
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

var gapReportsResource = assistant.Resource("capabilitygapreports")

// CapabilityGapReportREST serves list for CapabilityGapReports from the
// shared gap-report store. It is read-only: create/update/delete/get are not
// implemented (v1) — a report is never fetched by id alone, only listed
// within a provider project.
type CapabilityGapReportREST struct {
	store gapreport.Store
	rest.TableConvertor
}

var (
	_ rest.Storage              = (*CapabilityGapReportREST)(nil)
	_ rest.Scoper               = (*CapabilityGapReportREST)(nil)
	_ rest.Lister               = (*CapabilityGapReportREST)(nil)
	_ rest.SingularNameProvider = (*CapabilityGapReportREST)(nil)
)

// NewCapabilityGapReportREST builds the CapabilityGapReport REST over the
// given store.
func NewCapabilityGapReportREST(store gapreport.Store) *CapabilityGapReportREST {
	return &CapabilityGapReportREST{
		store:          store,
		TableConvertor: rest.NewDefaultTableConvertor(gapReportsResource),
	}
}

func (r *CapabilityGapReportREST) New() runtime.Object { return &assistant.CapabilityGapReport{} }
func (r *CapabilityGapReportREST) NewList() runtime.Object {
	return &assistant.CapabilityGapReportList{}
}
func (r *CapabilityGapReportREST) Destroy()                {}
func (r *CapabilityGapReportREST) NamespaceScoped() bool   { return true }
func (r *CapabilityGapReportREST) GetSingularName() string { return "capabilitygapreport" }

// List returns the caller's provider project's gap reports, newest first.
// Label/field selectors are not supported in v1 (reports carry neither), so
// options are ignored beyond the namespace itself.
func (r *CapabilityGapReportREST) List(ctx context.Context, _ *metainternalversion.ListOptions) (runtime.Object, error) {
	providerProject, err := tenant.ProjectFromContext(ctx, gapReportsResource)
	if err != nil {
		return nil, err
	}
	reports, err := r.store.List(ctx, providerProject)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	list := &assistant.CapabilityGapReportList{Items: make([]assistant.CapabilityGapReport, 0, len(reports))}
	for _, rep := range reports {
		list.Items = append(list.Items, *newCapabilityGapReport(rep))
	}
	return list, nil
}

// newCapabilityGapReport maps a stored report to the internal API object.
func newCapabilityGapReport(rep gapreport.Report) *assistant.CapabilityGapReport {
	return &assistant.CapabilityGapReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              rep.ID,
			Namespace:         rep.ProviderProject,
			CreationTimestamp: metav1.NewTime(rep.CreatedAt),
		},
		Status: assistant.CapabilityGapReportStatus{
			ServiceName:     rep.ServiceName,
			ConsumerProject: rep.ConsumerProject,
			ContextID:       rep.ContextID,
			Capability:      rep.Capability,
			Summary:         rep.Summary,
		},
	}
}
