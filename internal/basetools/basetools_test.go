package basetools_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/basetools"
	"github.com/milo-os/assistant/internal/projectapi"
)

// ------------------------------------------------------------ test platform

// reply is one canned answer.
type reply struct {
	code int
	body string
}

func ok(body string) reply { return reply{code: 200, body: body} }

func status(code int, message string) reply {
	return reply{code: code, body: `{"kind":"Status","code":` +
		itoa(code) + `,"message":` + quote(message) + `}`}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// platform is a stand-in for the project's own API: routes are matched on a
// path suffix, and every request is recorded so a test can assert on what
// actually went out — which identity carried it and which project it named.
type platform struct {
	mu       sync.Mutex
	routes   []routeEntry
	requests []recorded
	server   *httptest.Server
}

type routeEntry struct {
	// method, when set, narrows the route to one verb, telling a read and a
	// write on the same path apart.
	method string
	suffix string
	// persistedOnly narrows the route to a request that would actually change
	// something, so a dry run can pass and the write after it fail. That is
	// the only way to reach a part-way change.
	persistedOnly bool
	reply         reply
}

type recorded struct {
	method        string
	path          string
	query         string
	authorization string
	body          string
}

// dryRun reports whether this request asked the platform to keep nothing.
func (r recorded) dryRun() bool { return strings.Contains(r.query, "dryRun=All") }

// persisted reports whether this request would have changed something.
func (r recorded) persisted() bool {
	switch r.method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return !r.dryRun()
	}
	return false
}

func newPlatform(t *testing.T) *platform {
	t.Helper()
	p := &platform{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		p.mu.Lock()
		p.requests = append(p.requests, recorded{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"), body: string(body),
		})
		answer := reply{code: 404, body: `{"kind":"Status","code":404,"message":"not served here"}`}
		for _, route := range p.routes {
			if route.method != "" && route.method != r.Method {
				continue
			}
			if route.persistedOnly && strings.Contains(r.URL.RawQuery, "dryRun=All") {
				continue
			}
			if strings.HasSuffix(r.URL.Path, route.suffix) {
				answer = route.reply
				break
			}
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.code)
		_, _ = w.Write([]byte(answer.body))
	}))
	t.Cleanup(p.server.Close)
	return p
}

// route registers an answer for every path ending in suffix. Later routes lose
// to earlier ones, so a test can register a specific path before a general one.
func (p *platform) route(suffix string, r reply) *platform {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes = append(p.routes, routeEntry{suffix: suffix, reply: r})
	return p
}

// routeFor registers an answer for one verb, which wins over a verb-agnostic
// route registered later.
func (p *platform) routeFor(method, suffix string, r reply) *platform {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes = append(p.routes, routeEntry{method: method, suffix: suffix, reply: r})
	return p
}

// override registers an answer beating everything already registered, so a
// test can change the platform's mind part-way through.
func (p *platform) override(method, suffix string, r reply) *platform {
	return p.prepend(routeEntry{method: method, suffix: suffix, reply: r})
}

// overrideWrite is override for the persisting request, leaving the dry run
// before it to answer as it did.
func (p *platform) overrideWrite(method, suffix string, r reply) *platform {
	return p.prepend(routeEntry{method: method, suffix: suffix, persistedOnly: true, reply: r})
}

func (p *platform) prepend(entry routeEntry) *platform {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes = append([]routeEntry{entry}, p.routes...)
	return p
}

func (p *platform) seen() []recorded {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recorded(nil), p.requests...)
}

// tools builds the read-only base tools against this platform, for one project
// and one caller.
func (p *platform) tools(t *testing.T, project, token string) agentcore.ToolSet {
	t.Helper()
	return p.toolSet(t, basetools.Options{Project: p.view(t, project, token)})
}

// writeTools builds the whole set, change path included.
func (p *platform) writeTools(t *testing.T, project, token string) agentcore.ToolSet {
	t.Helper()
	return p.toolSet(t, basetools.Options{
		Project:      p.view(t, project, token),
		PlanTokenKey: testPlanKey,
	})
}

// testPlanKey is the key the change path binds plans with in these tests.
var testPlanKey = []byte("a-test-key-long-enough-to-be-one")

func (p *platform) view(t *testing.T, project, token string) *projectapi.Project {
	t.Helper()
	client, err := projectapi.New(projectapi.Config{BaseURL: p.server.URL})
	if err != nil {
		t.Fatalf("projectapi.New: %v", err)
	}
	return client.As(project, token)
}

func (p *platform) toolSet(t *testing.T, opts basetools.Options) agentcore.ToolSet {
	t.Helper()
	return basetools.Tools(opts)
}

// call runs one tool and returns its output, failing the test on an error.
func call(t *testing.T, tools agentcore.ToolSet, name, input string) string {
	t.Helper()
	tool, exists := tools[name]
	if !exists {
		t.Fatalf("%s was not composed", name)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// callErr runs one tool and returns its refusal, failing the test if it did
// not refuse.
func callErr(t *testing.T, tools agentcore.ToolSet, name, input string) string {
	t.Helper()
	tool, exists := tools[name]
	if !exists {
		t.Fatalf("%s was not composed", name)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(input))
	if err == nil {
		t.Fatalf("%s returned %q, want a refusal", name, out)
	}
	return err.Error()
}

func decodeInto[T any](t *testing.T, raw string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("reading tool output %q: %v", raw, err)
	}
	return out
}

// ----------------------------------------------------------------- the set

func TestToolsNeedSomebodyToActAs(t *testing.T) {
	if got := basetools.Tools(basetools.Options{}); got != nil {
		t.Fatalf("Tools with no project = %v, want nil", got)
	}
}

func TestTheSetIsTheDocumentedOne(t *testing.T) {
	tools := newPlatform(t).tools(t, "demo", "tok")
	want := []string{
		basetools.LocationsListToolName,
		basetools.QuotaGetToolName,
		basetools.ResourcesGetToolName,
		basetools.ResourcesListToolName,
		basetools.SchemaGetToolName,
	}
	if len(tools) != len(want) {
		t.Fatalf("composed %d tools, want %d: %v", len(tools), len(want), tools)
	}
	for _, name := range want {
		if _, exists := tools[name]; !exists {
			t.Fatalf("%s is missing", name)
		}
	}
}

// The base tools are not one service's contribution, so they carry no service
// namespace — and because every provider tool does, no provider can shadow one.
func TestNamesAreNotNamespaced(t *testing.T) {
	for name := range newPlatform(t).tools(t, "demo", "tok") {
		if strings.Contains(name, "__") {
			t.Fatalf("%q is namespaced; base tools belong to no provider", name)
		}
	}
}

// No tool takes a project. The project is the conversation's, decided by a
// request the platform already authorized, and an argument naming one would be
// steerable by anything the model reads.
func TestNoToolAcceptsAProject(t *testing.T) {
	for name, tool := range newPlatform(t).tools(t, "demo", "tok") {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.Definition().InputSchema, &schema); err != nil {
			t.Fatalf("%s: reading the input schema: %v", name, err)
		}
		for property := range schema.Properties {
			switch strings.ToLower(property) {
			case "project", "projectname", "namespace_project", "tenant":
				t.Fatalf("%s accepts %q; the project must come from the conversation", name, property)
			}
		}
	}
}

// Passing one anyway changes nothing: the request still names the
// conversation's project.
func TestAProjectArgumentIsIgnored(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(`{"resources":[{"name":"workloads","kind":"Workload","namespaced":true}]}`)).
		route("/workloads", ok(`{"items":[]}`))

	call(t, p.tools(t, "demo-project", "tok"), basetools.ResourcesListToolName,
		`{"project":"someone-elses","group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload"}`)

	for _, request := range p.seen() {
		if strings.Contains(request.path, "someone-elses") {
			t.Fatalf("a tool argument steered the request: %s", request.path)
		}
		if !strings.Contains(request.path, "/projects/demo-project/control-plane") {
			t.Fatalf("request did not name the conversation's project: %s", request.path)
		}
	}
}

// Every request carries the caller's own credential and no other.
func TestEveryReadRunsAsTheCaller(t *testing.T) {
	p := newPlatform(t).
		route("/apis/compute.datumapis.com/v1alpha1", ok(`{"resources":[{"name":"workloads","kind":"Workload","namespaced":true}]}`)).
		route("/workloads", ok(`{"items":[]}`))

	call(t, p.tools(t, "demo", "caller-token"), basetools.ResourcesListToolName,
		`{"group":"compute.datumapis.com","version":"v1alpha1","kind":"Workload"}`)

	seen := p.seen()
	if len(seen) == 0 {
		t.Fatal("nothing was requested")
	}
	for _, request := range seen {
		if request.authorization != "Bearer caller-token" {
			t.Fatalf("request to %s carried %q", request.path, request.authorization)
		}
	}
}
