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
	c, err := projectapi.New(projectapi.Config{
		BaseURL:                           url,
		AllowInsecureCredentialForwarding: true, // httptest is plain http
	})
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

// Group and version come from model-supplied tool arguments and manifest
// apiVersions. Anything Kubernetes would not accept as a name is refused before
// a request is built, so it can never steer the path to another API.
func TestMalformedGroupOrVersionNeverReachesThePlatform(t *testing.T) {
	bad := []struct{ group, version string }{
		{"..", "v1"},
		{"compute.datumapis.com/../../core", "v1"},
		{"compute.datumapis.com%2F..", "v1"},
		{"compute.datumapis.com?x=1", "v1"},
		{"Compute.Datumapis.com", "v1"},
		{"compute.datumapis.com", ".."},
		{"compute.datumapis.com", "v1/../../v2"},
		{"compute.datumapis.com", "v1%2F.."},
		{"compute.datumapis.com", "v1?watch=true"},
		{"", "../apis/compute.datumapis.com/v1"},
	}
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(workloadsDiscovery))
	}))
	t.Cleanup(srv.Close)
	project := clientFor(t, srv.URL).As("demo", "tok")
	ctx := context.Background()

	for _, tc := range bad {
		t.Run(tc.group+" "+tc.version, func(t *testing.T) {
			if _, err := project.Resolve(ctx, tc.group, tc.version, "Workload"); err == nil {
				t.Errorf("Resolve accepted group %q version %q", tc.group, tc.version)
			}
			if _, err := project.OpenAPI(ctx, tc.group, tc.version); err == nil {
				t.Errorf("OpenAPI accepted group %q version %q", tc.group, tc.version)
			}
			// A Resource built by hand gets the same check on reads and writes,
			// and a refused write reports that nothing was sent.
			r := projectapi.Resource{Group: tc.group, Version: tc.version, Resource: "workloads", Kind: "Workload"}
			if _, err := project.List(ctx, r, ""); err == nil {
				t.Errorf("List accepted group %q version %q", tc.group, tc.version)
			}
			obj := projectapi.Object{"metadata": map[string]any{"name": "w"}}
			if _, err := project.Create(ctx, r, obj, true); !errors.Is(err, projectapi.ErrNotSent) {
				t.Errorf("Create err = %v, want ErrNotSent", err)
			}
		})
	}
	if hits != 0 {
		t.Fatalf("%d malformed requests reached the platform", hits)
	}
}

// Names and namespaces come straight from manifests. Escaping keeps "." and
// ".." as they are, so they are validated before a path is built: none of
// these reaches the platform, and a refused write reports that nothing was
// sent.
func TestMalformedNameOrNamespaceNeverReachesThePlatform(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	project := clientFor(t, srv.URL).As("demo", "tok")
	ctx := context.Background()
	r := projectapi.Resource{Group: "compute.datumapis.com", Version: "v1alpha1",
		Resource: "workloads", Kind: "Workload", Namespaced: true}

	for _, bad := range []string{".", "..", "a/b", "Upper"} {
		t.Run(bad, func(t *testing.T) {
			for _, tc := range []struct{ field, namespace, name string }{
				{"name", "default", bad},
				{"namespace", bad, "api"},
			} {
				if _, err := project.Get(ctx, r, tc.namespace, tc.name); !errors.Is(err, projectapi.ErrNotSent) {
					t.Errorf("Get with %s %q: err = %v, want ErrNotSent", tc.field, bad, err)
				}
				obj := projectapi.Object{"metadata": map[string]any{"name": tc.name, "namespace": tc.namespace}}
				if _, err := project.Create(ctx, r, obj, false); !errors.Is(err, projectapi.ErrNotSent) {
					t.Errorf("Create with %s %q: err = %v, want ErrNotSent", tc.field, bad, err)
				}
				if _, err := project.Update(ctx, r, obj, false); !errors.Is(err, projectapi.ErrNotSent) {
					t.Errorf("Update with %s %q: err = %v, want ErrNotSent", tc.field, bad, err)
				}
			}
			if _, err := project.List(ctx, r, bad); !errors.Is(err, projectapi.ErrNotSent) {
				t.Errorf("List in namespace %q: err = %v, want ErrNotSent", bad, err)
			}
		})
	}
	if hits != 0 {
		t.Fatalf("%d malformed requests reached the platform", hits)
	}

	if _, err := project.Get(ctx, r, "default", "api.v2"); err != nil {
		t.Fatalf("a valid name was refused: %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want the valid request to reach the platform", hits)
	}
}

func TestValidGroupsAndTheCoreGroupResolve(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) { return 200, workloadsDiscovery })
	project := clientFor(t, srv.URL).As("demo", "tok")

	for _, tc := range []struct{ group, version, wantSuffix string }{
		{"compute.datumapis.com", "v1alpha1", "/apis/compute.datumapis.com/v1alpha1"},
		{"networking.datumapis.com", "v1", "/apis/networking.datumapis.com/v1"},
		{"", "v1", "/api/v1"},
	} {
		if _, err := project.Resolve(context.Background(), tc.group, tc.version, "Workload"); err != nil {
			t.Fatalf("Resolve(%q, %q): %v", tc.group, tc.version, err)
		}
		if !strings.HasSuffix(rec.escapedPath, "/control-plane"+tc.wantSuffix) {
			t.Fatalf("path = %q, want suffix %q", rec.escapedPath, tc.wantSuffix)
		}
	}
}

// The resource segment comes from the platform's discovery reply; it is
// escaped like every other segment, so a slash in it stays inside it.
func TestResourceSegmentIsEscaped(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) { return 200, `{"items":[]}` })
	project := clientFor(t, srv.URL).As("demo", "tok")

	r := projectapi.Resource{Group: "compute.datumapis.com", Version: "v1alpha1", Resource: "../secrets", Kind: "Workload"}
	if _, err := project.List(context.Background(), r, ""); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.HasSuffix(rec.escapedPath, "/apis/compute.datumapis.com/v1alpha1/..%2Fsecrets") {
		t.Fatalf("path = %q, want the resource escaped as one segment", rec.escapedPath)
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

// A credential-bearing request must not follow a redirect that downgrades to
// plaintext: the Authorization header would otherwise travel over http.
func TestRefusesPlaintextRedirectWithToken(t *testing.T) {
	var gotAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workloadsDiscovery))
	}))
	t.Cleanup(target.Close)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c, err := projectapi.New(projectapi.Config{BaseURL: srv.URL, Transport: srv.Client().Transport})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.As("demo-project", "caller-token").Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("want a redirect refusal, got %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("the plaintext redirect target received %q", gotAuth)
	}
}

func TestNewRejectsAConfigurationThatCouldNeverWork(t *testing.T) {
	if _, err := projectapi.New(projectapi.Config{}); err == nil {
		t.Fatal("New with no BaseURL should fail")
	}
}

// The caller's token must never be attached to a plaintext request: the call
// fails closed before anything is sent.
func TestRefusesCallerTokenOverPlaintextHTTP(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) { return 200, workloadsDiscovery })

	c, err := projectapi.New(projectapi.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.As("demo-project", "caller-token").Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("want an https refusal, got %v", err)
	}
	if rec.method != "" {
		t.Fatalf("a refused request still reached the server (%s %s)", rec.method, rec.path)
	}
	if strings.Contains(err.Error(), "caller-token") {
		t.Fatalf("refusal leaked the token: %v", err)
	}
}

// With the dev escape hatch on, the same plaintext request is allowed.
func TestEscapeHatchAllowsCallerTokenOverPlaintextHTTP(t *testing.T) {
	var rec recorder
	srv := newServer(t, &rec, func(string) (int, string) { return 200, workloadsDiscovery })

	c, err := projectapi.New(projectapi.Config{
		BaseURL:                           srv.URL,
		AllowInsecureCredentialForwarding: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.As("demo-project", "caller-token").Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rec.authorization != "Bearer caller-token" {
		t.Fatalf("authorization = %q, want the caller's token", rec.authorization)
	}
}

// An https platform receives the token, verified with the transport's own
// (test) trust store rather than a client-held CA.
func TestAllowsCallerTokenOverHTTPS(t *testing.T) {
	var gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workloadsDiscovery))
	}))
	t.Cleanup(srv.Close)

	c, err := projectapi.New(projectapi.Config{
		BaseURL:   srv.URL,
		Transport: srv.Client().Transport,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.As("demo-project", "caller-token").Resolve(context.Background(), "compute.datumapis.com", "v1alpha1", "Workload"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if gotAuth != "Bearer caller-token" {
		t.Fatalf("authorization = %q, want the caller's token", gotAuth)
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

// A write that never reached the platform matches ErrNotSent, so the change
// path can say nothing was changed. One that reached it and lost its answer
// must not, because it may have been applied.
func TestWriteFailuresSayWhetherTheRequestWasSent(t *testing.T) {
	hangUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer hangUp.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	// A redirect means the platform already received the write once. Were it
	// followed, a hop that failed to connect would read as "not sent".
	redirected := 0
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected++
		http.Redirect(w, r, gone.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	tests := []struct {
		name        string
		baseURL     string
		wantNotSent bool
	}{
		{"connection refused", gone.URL, true},
		{"connection dropped after sending", hangUp.URL, false},
		{"redirected to a host that cannot be reached", redirect.URL, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := projectapi.New(projectapi.Config{BaseURL: tt.baseURL, AllowInsecureCredentialForwarding: true})
			if err != nil {
				t.Fatal(err)
			}
			obj := projectapi.Object{"metadata": map[string]any{"name": "api", "namespace": "default"}}
			_, err = client.As("demo", "tok").Create(context.Background(),
				projectapi.Resource{Group: "compute.datumapis.com", Version: "v1alpha1",
					Resource: "workloads", Kind: "Workload", Namespaced: true}, obj, false)
			if err == nil {
				t.Fatal("Create succeeded, want an error")
			}
			if got := errors.Is(err, projectapi.ErrNotSent); got != tt.wantNotSent {
				t.Fatalf("errors.Is(err, projectapi.ErrNotSent) = %v, want %v (err: %v)", got, tt.wantNotSent, err)
			}
		})
	}
	if redirected != 1 {
		t.Fatalf("the redirecting server got %d writes, want 1", redirected)
	}
}
