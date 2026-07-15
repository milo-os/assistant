package capability

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validDocJSON = `{
  "apiVersion": "services.miloapis.com/v1alpha1",
  "kind": "AgentBinding",
  "metadata": { "name": "streamco-binding", "namespace": "demo-project" },
  "spec": {
    "serviceRef": { "name": "streamco" },
    "serviceName": "streaming.streamco.example",
    "serviceAgentRef": { "name": "streamco-agent" },
    "configurationVersion": "v1",
    "knowledge": {
      "sources": [{ "type": "LLMDocs", "title": "Overview", "url": "http://127.0.0.1:7810/llms-full.txt" }],
      "concepts": [{ "gvk": { "group": "streaming.streamco.example", "kind": "Stream" }, "summary": "A live stream" }]
    },
    "tools": {
      "mcpServers": [{ "name": "streamco", "endpoint": "http://127.0.0.1:7810/mcp", "toolSelector": { "include": ["streams_list", "streams_get", "pipeline_diagnose"] }, "mutating": [] }]
    },
    "authority": { "reads": [{ "gvk": { "group": "streaming.streamco.example", "kind": "*" } }], "maxTaskDurationSeconds": 60 }
  },
  "status": { "conditions": [{ "type": "Ready", "status": "True" }] }
}`

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFixtureSource_ParsesBareArray(t *testing.T) {
	path := writeFixture(t, "["+validDocJSON+"]")
	docs, err := NewFixtureSource(path, nil).Documents(context.Background(), "demo-project")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 doc, got %d", len(docs))
	}
	s := docs[0].Spec
	if s.ServiceName != "streaming.streamco.example" || s.ServiceRef.Name != "streamco" || s.ConfigurationVersion != "v1" {
		t.Fatalf("spec = %+v", s)
	}
	if got := s.Tools.MCPServers[0].ToolSelector.Include; len(got) != 3 || got[0] != "streams_list" {
		t.Fatalf("include = %v", got)
	}
	if s.Knowledge.Sources[0].Type != KnowledgeLLMDocs {
		t.Fatalf("source type = %v", s.Knowledge.Sources[0].Type)
	}
	if s.Authority == nil || s.Authority.MaxTaskDurationSeconds == nil || *s.Authority.MaxTaskDurationSeconds != 60 {
		t.Fatalf("authority = %+v", s.Authority)
	}
}

func TestFixtureSource_AcceptsListObject(t *testing.T) {
	path := writeFixture(t, `{"kind":"List","items":[`+validDocJSON+`]}`)
	docs, err := NewFixtureSource(path, nil).Documents(context.Background(), "demo-project")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Spec.ServiceName != "streaming.streamco.example" {
		t.Fatalf("docs = %+v", docs)
	}
}

func TestFixtureSource_AppliesDefaultsForOmittedFields(t *testing.T) {
	minimal := `[{"spec":{"serviceRef":{"name":"s"},"serviceName":"svc.example.com","serviceAgentRef":{"name":"a"},"configurationVersion":"v1","tools":{"mcpServers":[{"name":"srv","endpoint":"http://x/mcp","toolSelector":{"include":["t"]}}]}}}]`
	path := writeFixture(t, minimal)
	docs, err := NewFixtureSource(path, nil).Documents(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if docs[0].Spec.Tools.MCPServers[0].Mutating != nil && len(docs[0].Spec.Tools.MCPServers[0].Mutating) != 0 {
		t.Fatalf("mutating should default to empty, got %v", docs[0].Spec.Tools.MCPServers[0].Mutating)
	}
	if docs[0].Spec.Knowledge != nil {
		t.Fatalf("knowledge should be absent, got %+v", docs[0].Spec.Knowledge)
	}
}

func TestFixtureSource_SkipsInvalidKeepsValidWithWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger(&buf)
	invalid := `{"spec":{"serviceName":"missing-required-fields.example"}}`
	path := writeFixture(t, "["+invalid+","+validDocJSON+"]")

	docs, err := NewFixtureSource(path, logger).Documents(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Spec.ServiceName != "streaming.streamco.example" {
		t.Fatalf("want only the valid doc, got %+v", docs)
	}
	if !strings.Contains(buf.String(), "entry_skipped") {
		t.Fatalf("expected a skip warning; logs:\n%s", buf.String())
	}
}

func TestFixtureSource_ErrorsOnInvalidJSON(t *testing.T) {
	path := writeFixture(t, "{not json")
	_, err := NewFixtureSource(path, nil).Documents(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "parse capability documents") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestFixtureSource_ErrorsWhenRootNeitherArrayNorList(t *testing.T) {
	path := writeFixture(t, `{"spec":{}}`)
	_, err := NewFixtureSource(path, nil).Documents(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "JSON array") {
		t.Fatalf("want array/list error, got %v", err)
	}
}

func TestFixtureSource_PropagatesMissingFile(t *testing.T) {
	_, err := NewFixtureSource(filepath.Join(t.TempDir(), "nope.json"), nil).Documents(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "read capability documents fixture") {
		t.Fatalf("want read error, got %v", err)
	}
}
