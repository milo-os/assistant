package projectapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/projectapi"
)

// recorder captures what the client actually put on the wire, which is the
// only place the "acts as the caller" claim can be checked.
type recorder struct {
	method        string
	path          string
	escapedPath   string
	rawQuery      string
	authorization string
	body          string
}

// newServer answers every request with reply and records the last one.
func newServer(t *testing.T, rec *recorder, reply func(path string) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.escapedPath = r.URL.EscapedPath()
		rec.rawQuery = r.URL.RawQuery
		rec.authorization = r.Header.Get("Authorization")
		rec.body = string(body)

		code, payload := reply(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const workloadsDiscovery = `{"kind":"APIResourceList","resources":[
	{"name":"workloads","kind":"Workload","namespaced":true},
	{"name":"workloads/status","kind":"Workload","namespaced":true}
]}`

func clientFor(t *testing.T, url string) *projectapi.Client {
	t.Helper()
	c, err := projectapi.New(projectapi.Config{BaseURL: url})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// The credential on the wire is the caller's own. There is no field on the
// client for a service credential, so this is the whole of what can be sent.
func TestActsAsTheCaller(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) { return 200, workloadsDiscovery })

	project := clientFor(t, srv.URL).As("demo-project", "caller-token")
	if _, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := rec.authorization, "Bearer caller-token"; got != want {
		t.Fatalf("authorization = %q, want %q", got, want)
	}
}

// A caller with no credential sends none. Reading as the service instead is
// the one outcome this must never have.
func TestNoCallerTokenSendsNoCredential(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) { return 200, workloadsDiscovery })

	project := clientFor(t, srv.URL).As("demo-project", "")
	_, _ = project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if rec.authorization != "" {
		t.Fatalf("authorization = %q, want empty", rec.authorization)
	}
}

// Every request is scoped by the project's own prefix, so nothing reaches
// another project's resources.
func TestRequestsAreScopedToTheProject(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(path string) (int, string) {
		if strings.HasSuffix(path, "/apis/compute.datumapis.com/v1alpha1") {
			return 200, workloadsDiscovery
		}
		return 200, `{"items":[{"metadata":{"name":"api"}}]}`
	})

	project := clientFor(t, srv.URL).As("we/ird", "tok")
	resource, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := project.List(context.Background(), resource, "default"); err != nil {
		t.Fatalf("List: %v", err)
	}

	const wantPrefix = "/apis/resourcemanager.miloapis.com/v1alpha1/projects/we%2Fird/control-plane"
	if !strings.HasPrefix(rec.escapedPath, wantPrefix) {
		t.Fatalf("path = %q, want prefix %q", rec.escapedPath, wantPrefix)
	}
	if !strings.HasSuffix(rec.path, "/apis/compute.datumapis.com/v1alpha1/namespaces/default/workloads") {
		t.Fatalf("path = %q, want the namespaced workloads collection", rec.path)
	}
}

func TestResolveAcceptsKindOrPluralAndReportsNamespacing(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) { return 200, workloadsDiscovery })
	project := clientFor(t, srv.URL).As("demo", "tok")

	for _, name := range []string{"Workload", "workload", "workloads"} {
		got, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if got.Resource != "workloads" || got.Kind != "Workload" || !got.Namespaced {
			t.Fatalf("Resolve(%q) = %+v", name, got)
		}
	}
}

// A subresource is not a kind anyone works with, so it must never be chosen.
func TestResolveIgnoresSubresources(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) {
		return 200, `{"resources":[{"name":"workloads/status","kind":"Workload","namespaced":true}]}`
	})
	project := clientFor(t, srv.URL).As("demo", "tok")

	_, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if !errors.Is(err, projectapi.ErrKindNotServed) {
		t.Fatalf("err = %v, want ErrKindNotServed", err)
	}
}

func TestResolveReportsAKindTheProjectDoesNotServe(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) { return 200, workloadsDiscovery })
	project := clientFor(t, srv.URL).As("demo", "tok")

	if _, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Widget"); !errors.Is(err, projectapi.ErrKindNotServed) {
		t.Fatalf("err = %v, want ErrKindNotServed", err)
	}
}

func TestResolveReportsAGroupTheProjectDoesNotServe(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) {
		return 404, `{"kind":"Status","code":404,"message":"the server could not find the requested resource"}`
	})
	project := clientFor(t, srv.URL).As("demo", "tok")

	if _, err := project.Resolve(context.Background(), "nope.example", "v1", "Thing"); !errors.Is(err, projectapi.ErrKindNotServed) {
		t.Fatalf("err = %v, want ErrKindNotServed", err)
	}
}

func TestGetReportsMissingObjectsDistinctly(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(path string) (int, string) {
		if strings.HasSuffix(path, "/v1alpha1") {
			return 200, workloadsDiscovery
		}
		return 404, `{"kind":"Status","code":404,"reason":"NotFound","message":"workloads.compute.datumapis.com \"api\" not found"}`
	})
	project := clientFor(t, srv.URL).As("demo", "tok")

	resource, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = project.Get(context.Background(), resource, "default", "api")
	if !projectapi.IsNotFound(err) {
		t.Fatalf("err = %v, want a not-found", err)
	}
	if projectapi.IsForbidden(err) {
		t.Fatal("a missing object must not read as a refusal")
	}
}

func TestForbiddenIsRecognized(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) {
		return 403, `{"kind":"Status","code":403,"reason":"Forbidden","message":"workloads is forbidden"}`
	})
	project := clientFor(t, srv.URL).As("demo", "tok")

	_, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if !projectapi.IsForbidden(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// A rejection's field paths are the part a person acts on, so they have to
// survive the trip out of the client.
func TestFieldErrorsCarryThePathsTheServerNamed(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(path string) (int, string) {
		if strings.HasSuffix(path, "/v1alpha1") {
			return 200, workloadsDiscovery
		}
		return 422, `{"kind":"Status","code":422,"message":"Workload \"api\" is invalid",
			"details":{"causes":[{"field":"spec.replicas","message":"must be at least 1"}]}}`
	})
	project := clientFor(t, srv.URL).As("demo", "tok")

	resource, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = project.Create(context.Background(), resource,
		projectapi.Object{"metadata": map[string]any{"name": "api", "namespace": "default"}}, true)

	var apiErr *projectapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	fields := apiErr.FieldErrors()
	if len(fields) != 2 || fields[0].Field != "spec.replicas" {
		t.Fatalf("FieldErrors() = %+v", fields)
	}
	if !strings.Contains(fields[1].Message, "is invalid") {
		t.Fatalf("the platform's own summary is missing: %+v", fields)
	}
}

// A dry run has to say so on the wire; a write that quietly persisted would
// be the single worst bug this package could have.
func TestDryRunIsAskedForOnTheWire(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(path string) (int, string) {
		if strings.HasSuffix(path, "/v1alpha1") {
			return 200, workloadsDiscovery
		}
		return 201, `{"metadata":{"name":"api"}}`
	})
	project := clientFor(t, srv.URL).As("demo", "tok")
	resource, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	obj := projectapi.Object{"metadata": map[string]any{"name": "api", "namespace": "default"}}

	if _, err := project.Create(context.Background(), resource, obj, true); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.rawQuery != "dryRun=All" {
		t.Fatalf("query = %q, want dryRun=All", rec.rawQuery)
	}
	if _, err := project.Create(context.Background(), resource, obj, false); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.rawQuery != "" {
		t.Fatalf("query = %q, want none for a real write", rec.rawQuery)
	}
	if rec.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", rec.method)
	}
}

func TestUpdateAddressesTheNamedObject(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(path string) (int, string) {
		if strings.HasSuffix(path, "/v1alpha1") {
			return 200, workloadsDiscovery
		}
		return 200, `{"metadata":{"name":"api"}}`
	})
	project := clientFor(t, srv.URL).As("demo", "tok")
	resource, err := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	obj := projectapi.Object{"metadata": map[string]any{"name": "api", "namespace": "team-a"}}
	if _, err := project.Update(context.Background(), resource, obj, false); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if rec.method != http.MethodPut {
		t.Fatalf("method = %q, want PUT", rec.method)
	}
	if !strings.HasSuffix(rec.path, "/namespaces/team-a/workloads/api") {
		t.Fatalf("path = %q", rec.path)
	}
}

// A kind held in no namespace addresses the collection directly.
func TestClusterScopedKindsSkipTheNamespaceSegment(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(path string) (int, string) {
		if strings.HasSuffix(path, "/v1alpha1") {
			return 200, `{"resources":[{"name":"locations","kind":"Location","namespaced":false}]}`
		}
		return 200, `{"items":[]}`
	})
	project := clientFor(t, srv.URL).As("demo", "tok")
	resource, err := project.Resolve(context.Background(), "locations.miloapis.com", "v1alpha1", "Location")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := project.List(context.Background(), resource, "ignored"); err != nil {
		t.Fatalf("List: %v", err)
	}
	if strings.Contains(rec.path, "/namespaces/") {
		t.Fatalf("path = %q, want no namespace segment", rec.path)
	}
}

func TestListReturnsAnEmptySliceRatherThanNil(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(path string) (int, string) {
		if strings.HasSuffix(path, "/v1alpha1") {
			return 200, workloadsDiscovery
		}
		return 200, `{"items":null}`
	})
	project := clientFor(t, srv.URL).As("demo", "tok")
	resource, _ := project.Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	items, err := project.List(context.Background(), resource, "default")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if items == nil {
		t.Fatal("List returned nil; an empty project is an answer, not an absence")
	}
}

func TestNewRejectsAConfigurationThatCouldNeverWork(t *testing.T) {
	if _, err := projectapi.New(projectapi.Config{}); err == nil {
		t.Fatal("New with no BaseURL should fail")
	}
	if _, err := projectapi.New(projectapi.Config{BaseURL: "https://x.test", CACert: []byte("not pem")}); err == nil {
		t.Fatal("New with a bad CA should fail")
	}
}

func TestNestedHelpers(t *testing.T) {
	var obj projectapi.Object
	if err := json.Unmarshal([]byte(`{"spec":{"n":3,"s":"x","m":{"a":"b"},"l":[1]},
		"status":{"conditions":[{"type":"Ready","status":"True"}]}}`), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := projectapi.NestedInt64(obj, "spec", "n"); !ok || v != 3 {
		t.Fatalf("NestedInt64 = %v %v", v, ok)
	}
	if v, ok := projectapi.NestedString(obj, "spec", "s"); !ok || v != "x" {
		t.Fatalf("NestedString = %v %v", v, ok)
	}
	if _, ok := projectapi.NestedMap(obj, "spec", "m"); !ok {
		t.Fatal("NestedMap")
	}
	if _, ok := projectapi.NestedSlice(obj, "spec", "l"); !ok {
		t.Fatal("NestedSlice")
	}
	if _, ok := projectapi.NestedString(obj, "spec", "missing"); ok {
		t.Fatal("a missing path must report so")
	}
	if !projectapi.ConditionTrue(obj, "Ready") {
		t.Fatal("ConditionTrue(Ready)")
	}
	if projectapi.ConditionTrue(obj, "Available") {
		t.Fatal("ConditionTrue(Available) should be false")
	}
}
