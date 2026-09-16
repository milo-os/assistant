package tenant

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
)

var gr = schema.GroupResource{Group: "assistant.miloapis.com", Resource: "conversations"}

// withUser attaches a milo identity whose Extra carries the given parent
// type/name (the shape the milo apiserver front end stamps).
func withUser(ctx context.Context, parentType, parentName string) context.Context {
	return request.WithUser(ctx, &user.DefaultInfo{
		Name: "caller",
		Extra: map[string][]string{
			ExtraParentType: {parentType},
			ExtraParentName: {parentName},
		},
	})
}

// TestProjectFromContextNamespaceOnly is the dev path: an in-cluster identity
// with no milo Extra, so the namespace stands alone as the project.
func TestProjectFromContextNamespaceOnly(t *testing.T) {
	ctx := request.WithNamespace(context.Background(), "demo")
	p, err := ProjectFromContext(ctx, gr)
	if err != nil {
		t.Fatalf("ProjectFromContext: %v", err)
	}
	if p != "demo" {
		t.Errorf("project = %q, want demo", p)
	}
}

// A milo parent Project that matches the requested namespace is allowed.
func TestProjectFromContextExtraMatchesNamespace(t *testing.T) {
	ctx := withUser(request.WithNamespace(context.Background(), "demo"), "Project", "demo")
	p, err := ProjectFromContext(ctx, gr)
	if err != nil {
		t.Fatalf("ProjectFromContext: %v", err)
	}
	if p != "demo" {
		t.Errorf("project = %q, want demo", p)
	}
}

// A token minted for project A must not read project B by aiming the URL at B.
func TestProjectFromContextExtraMismatchIsForbidden(t *testing.T) {
	ctx := withUser(request.WithNamespace(context.Background(), "demo"), "Project", "other")
	_, err := ProjectFromContext(ctx, gr)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want Forbidden", err)
	}
}

// A parent that is not a Project (e.g. an Organization identity) carries no
// project constraint, so the namespace stands alone rather than forbidding.
func TestProjectFromContextNonProjectParentIgnored(t *testing.T) {
	ctx := withUser(request.WithNamespace(context.Background(), "demo"), "Organization", "acme")
	p, err := ProjectFromContext(ctx, gr)
	if err != nil {
		t.Fatalf("ProjectFromContext: %v", err)
	}
	if p != "demo" {
		t.Errorf("project = %q, want demo (org parent imposes no project scope)", p)
	}
}

func TestProjectFromContextMissingNamespaceIsBadRequest(t *testing.T) {
	_, err := ProjectFromContext(context.Background(), gr)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest", err)
	}
}

func TestProjectFromContextEmptyNamespaceIsBadRequest(t *testing.T) {
	ctx := request.WithNamespace(context.Background(), "")
	_, err := ProjectFromContext(ctx, gr)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest", err)
	}
}

func TestProjectFromContextNullByteNamespaceIsBadRequest(t *testing.T) {
	ctx := request.WithNamespace(context.Background(), "demo-project\x00evil")
	_, err := ProjectFromContext(ctx, gr)
	if !apierrors.IsBadRequest(err) {
		t.Fatalf("err = %v, want BadRequest", err)
	}
}

func TestFromContextNoUser(t *testing.T) {
	if id := FromContext(context.Background()); id != (Identity{}) {
		t.Errorf("Identity = %+v, want zero (no authenticated user)", id)
	}
}

// FromContext extracts the full milo parent identity from Extra.
func TestFromContextParsesExtra(t *testing.T) {
	ctx := request.WithUser(context.Background(), &user.DefaultInfo{
		Extra: map[string][]string{
			ExtraParentAPIGroup: {"iam.miloapis.com"},
			ExtraParentType:     {"Project"},
			ExtraParentName:     {"demo"},
		},
	})
	got := FromContext(ctx)
	want := Identity{APIGroup: "iam.miloapis.com", Kind: "Project", Name: "demo"}
	if got != want {
		t.Errorf("Identity = %+v, want %+v", got, want)
	}
}

// A milo identity present but carrying no parent Extra (empty maps) resolves to
// a zero Identity, i.e. no project constraint.
func TestFromContextUserWithoutParentExtra(t *testing.T) {
	ctx := request.WithUser(context.Background(), &user.DefaultInfo{Name: "caller"})
	if id := FromContext(ctx); id != (Identity{}) {
		t.Errorf("Identity = %+v, want zero (no parent stamped)", id)
	}
}

// Extra with multiple values for a key takes the first — the parent stamp is
// single-valued, so first() must not concatenate or panic on extras.
func TestFromContextMultiValuedExtraTakesFirst(t *testing.T) {
	ctx := request.WithUser(context.Background(), &user.DefaultInfo{
		Extra: map[string][]string{
			ExtraParentType: {"Project", "Organization"},
			ExtraParentName: {"demo", "other"},
		},
	})
	if got := FromContext(ctx).Project(); got != "demo" {
		t.Errorf("Project = %q, want demo (first value)", got)
	}
}

func TestIdentityProject(t *testing.T) {
	cases := []struct {
		name string
		id   Identity
		want string
	}{
		{"project parent", Identity{Kind: "Project", Name: "demo"}, "demo"},
		{"org parent", Identity{Kind: "Organization", Name: "acme"}, ""},
		{"zero identity", Identity{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.Project(); got != tc.want {
				t.Errorf("Project() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ScopeFromContext resolves the same namespace and identity as
// ProjectFromContext but reports a mismatch instead of refusing it, so a verb
// can decide for itself what a mismatch means (a list answers empty, every
// per-object verb forbids).
func TestScopeFromContext(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want Scope
	}{
		{
			name: "namespace only (dev path)",
			ctx:  request.WithNamespace(context.Background(), "demo"),
			want: Scope{Namespace: "demo"},
		},
		{
			name: "identity project matches namespace",
			ctx:  withUser(request.WithNamespace(context.Background(), "demo"), "Project", "demo"),
			want: Scope{Namespace: "demo", IdentityProject: "demo"},
		},
		{
			name: "identity project differs from namespace",
			ctx:  withUser(request.WithNamespace(context.Background(), "milo-system"), "Project", "project-a"),
			want: Scope{Namespace: "milo-system", IdentityProject: "project-a", Mismatch: true},
		},
		{
			name: "non-project parent imposes no constraint",
			ctx:  withUser(request.WithNamespace(context.Background(), "demo"), "Organization", "acme"),
			want: Scope{Namespace: "demo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScopeFromContext(tc.ctx)
			if err != nil {
				t.Fatalf("ScopeFromContext: %v", err)
			}
			if got != tc.want {
				t.Errorf("Scope = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The namespace checks are shared with ProjectFromContext: a request without a
// usable namespace is a 400 before any identity or mismatch question arises.
func TestScopeFromContextBadNamespaceIsBadRequest(t *testing.T) {
	cases := map[string]context.Context{
		"missing":  context.Background(),
		"empty":    request.WithNamespace(context.Background(), ""),
		"NUL byte": request.WithNamespace(context.Background(), "demo-project\x00evil"),
	}
	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ScopeFromContext(ctx)
			if !apierrors.IsBadRequest(err) {
				t.Fatalf("err = %v, want BadRequest", err)
			}
		})
	}
}
