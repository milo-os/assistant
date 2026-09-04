package capability

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPSource_FetchesAndParses(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[" + validDocJSON + "]"))
	}))
	defer srv.Close()

	docs, err := NewHTTPSource(srv.URL, nil, nil).Documents(context.Background(), "demo-project")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/projects/demo-project/capability-documents" {
		t.Fatalf("request path = %q", gotPath)
	}
	if len(docs) != 1 || docs[0].Spec.ServiceName != "streaming.streamco.example" {
		t.Fatalf("docs = %+v", docs)
	}
}

func TestHTTPSource_AcceptsListObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"kind":"List","items":[` + validDocJSON + `]}`))
	}))
	defer srv.Close()

	docs, err := NewHTTPSource(srv.URL, nil, nil).Documents(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 doc, got %d", len(docs))
	}
}

func TestHTTPSource_SkipsInvalidKeepsValidWithWarning(t *testing.T) {
	invalid := `{"spec":{"serviceName":"missing-required-fields.example"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("[" + invalid + "," + validDocJSON + "]"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	docs, err := NewHTTPSource(srv.URL, nil, testLogger(&buf)).Documents(context.Background(), "p")
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

func TestHTTPSource_TrimsTrailingSlashOnBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	if _, err := NewHTTPSource(srv.URL+"/", nil, nil).Documents(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/projects/p/capability-documents" {
		t.Fatalf("path = %q (base URL trailing slash not trimmed?)", gotPath)
	}
}

func TestHTTPSource_EmptyListYieldsNoDocs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	docs, err := NewHTTPSource(srv.URL, nil, nil).Documents(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Fatalf("want 0 docs, got %d", len(docs))
	}
}

func TestHTTPSource_DegradesOnServerDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	var buf bytes.Buffer
	docs, err := NewHTTPSource(url, nil, testLogger(&buf)).Documents(context.Background(), "p")
	if err != nil {
		t.Fatalf("server-down must degrade, not error: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("want 0 docs on degradation, got %d", len(docs))
	}
	if !strings.Contains(buf.String(), "fetch_failed") {
		t.Fatalf("expected a fetch_failed warning; logs:\n%s", buf.String())
	}
}

func TestHTTPSource_DegradesOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	docs, err := NewHTTPSource(srv.URL, nil, testLogger(&buf)).Documents(context.Background(), "p")
	if err != nil {
		t.Fatalf("non-2xx must degrade, not error: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("want 0 docs, got %d", len(docs))
	}
	if !strings.Contains(buf.String(), "status") {
		t.Fatalf("expected a status-stage warning; logs:\n%s", buf.String())
	}
}

func TestHTTPSource_DegradesOnMalformedRoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	docs, err := NewHTTPSource(srv.URL, nil, testLogger(&buf)).Documents(context.Background(), "p")
	if err != nil {
		t.Fatalf("malformed root must degrade, not error: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("want 0 docs, got %d", len(docs))
	}
	if !strings.Contains(buf.String(), "fetch_failed") {
		t.Fatalf("expected a fetch_failed warning; logs:\n%s", buf.String())
	}
}
