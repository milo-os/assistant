// Package tenant resolves the milo project that scopes a conversations
// apiserver request. Conversations are keyed by project in Postgres
// (conversations.project_name), so every read must be pinned to exactly one
// project — never a cross-project view.
//
// The project is the request namespace. When the caller is a milo identity the
// request also carries the identity's parent-Project in UserInfo.Extra
// (populated by the milo apiserver front end); when present it must agree with
// the namespace, so a token minted for project A cannot read project B by
// aiming the URL at B's namespace. In dev (in-cluster authn against the kind
// apiserver) there is no milo Extra, so the namespace stands alone.
package tenant

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/request"
)

const (
	// ExtraParentAPIGroup, ExtraParentType, ExtraParentName are the UserInfo.Extra
	// keys the milo apiserver stamps with the authenticated identity's parent
	// resource (its owning Project or Organization).
	ExtraParentAPIGroup = "iam.miloapis.com/parent-api-group"
	ExtraParentType     = "iam.miloapis.com/parent-type"
	ExtraParentName     = "iam.miloapis.com/parent-name"
)

// Identity is the milo parent resource of the authenticated caller, extracted
// from UserInfo.Extra. A zero Identity means "no milo parent" (e.g. a dev
// in-cluster identity), in which case scoping falls back to the namespace.
type Identity struct {
	APIGroup string
	Kind     string
	Name     string
}

// Project returns the identity's parent Project name, or "" if the parent is
// not a Project (or there is no parent).
func (id Identity) Project() string {
	if id.Kind == "Project" {
		return id.Name
	}
	return ""
}

// FromContext reads the caller's milo parent identity from the request user's
// Extra. It returns a zero Identity when there is no authenticated user or no
// parent stamped (dev in-cluster path).
func FromContext(ctx context.Context) Identity {
	u, ok := request.UserFrom(ctx)
	if !ok {
		return Identity{}
	}
	extra := u.GetExtra()
	return Identity{
		APIGroup: first(extra[ExtraParentAPIGroup]),
		Kind:     first(extra[ExtraParentType]),
		Name:     first(extra[ExtraParentName]),
	}
}

// ProjectFromContext resolves the single project a request may read. The
// namespace is authoritative (conversations are namespaced by project); if the
// caller's milo identity carries a parent Project it must equal the namespace,
// otherwise the request is refused so a project-scoped token cannot reach
// another project's rows. gr scopes the returned Forbidden/BadRequest error.
func ProjectFromContext(ctx context.Context, gr schema.GroupResource) (string, error) {
	ns, ok := request.NamespaceFrom(ctx)
	if !ok || ns == "" {
		return "", apierrors.NewBadRequest("conversations are namespaced by project; a namespace is required")
	}
	// Postgres' text type (unlike UTF-8 itself) disallows NUL — an unfiltered
	// namespace reaches the driver as a raw "invalid byte sequence" error (a
	// 500 that leaks backend/SQLSTATE detail) instead of a clean 400. A real
	// milo project name can never contain one.
	if strings.ContainsRune(ns, 0) {
		return "", apierrors.NewBadRequest("invalid namespace")
	}
	if p := FromContext(ctx).Project(); p != "" && p != ns {
		return "", apierrors.NewForbidden(gr, "", errProjectMismatch(p, ns))
	}
	return ns, nil
}

func first(vals []string) string {
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

type projectMismatchError struct{ identityProject, namespace string }

func (e projectMismatchError) Error() string {
	return "caller's project \"" + e.identityProject +
		"\" does not match the requested namespace \"" + e.namespace + "\""
}

func errProjectMismatch(identityProject, namespace string) error {
	return projectMismatchError{identityProject, namespace}
}
