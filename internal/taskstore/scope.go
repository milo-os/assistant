package taskstore

import "context"

// projectScope is the tenant filter carried on a context for the interface-level
// [PostgresStore.List]. It is unexported and only settable via [WithProjectScope]
// so a caller cannot accidentally widen visibility — the zero value (no scope)
// yields an empty listing, never a cross-tenant leak.
type projectScope struct {
	projects []string
	all      bool
}

type scopeKey struct{}

// WithProjectScope returns a context that scopes an interface-level List to the
// given projects. It exists so a future scoped ListTasks endpoint can pass the
// caller's granted projects through a2a-go's project-blind List signature.
// Passing all=true lists every project's tasks and is intended for operational
// tooling only, never a tenant-facing path.
func WithProjectScope(ctx context.Context, projects []string, all bool) context.Context {
	return context.WithValue(ctx, scopeKey{}, projectScope{projects: projects, all: all})
}

// projectScopeFromContext reads the scope set by [WithProjectScope]. When none
// is present it reports no projects and all=false — i.e. nothing visible.
func projectScopeFromContext(ctx context.Context) (projects []string, all bool) {
	if s, ok := ctx.Value(scopeKey{}).(projectScope); ok {
		return s.projects, s.all
	}
	return nil, false
}
