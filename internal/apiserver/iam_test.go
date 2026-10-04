package apiserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/yaml"

	"github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

const iamDir = "../../config/milo/iam"

type protectedResource struct {
	Spec struct {
		ServiceRef struct {
			Name string `json:"name"`
		} `json:"serviceRef"`
		Plural       string   `json:"plural"`
		Permissions  []string `json:"permissions"`
		Subresources []struct {
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		} `json:"subresources"`
	} `json:"spec"`
}

type role struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		IncludedPermissions []string `json:"includedPermissions"`
	} `json:"spec"`
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func globYAML(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(iamDir, dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if filepath.Base(f) != "kustomization.yaml" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no manifests under %s/%s", iamDir, dir)
	}
	return out
}

func declaredPermissions(t *testing.T) sets.Set[string] {
	t.Helper()
	declared := sets.New[string]()
	for _, f := range globYAML(t, "resources") {
		var pr protectedResource
		readYAML(t, f, &pr)
		base := pr.Spec.ServiceRef.Name + "/" + pr.Spec.Plural
		for _, verb := range pr.Spec.Permissions {
			declared.Insert(base + "." + verb)
		}
		for _, sub := range pr.Spec.Subresources {
			for _, verb := range sub.Permissions {
				declared.Insert(base + "/" + sub.Name + "." + verb)
			}
		}
	}
	return declared
}

func roles(t *testing.T) []role {
	t.Helper()
	var out []role
	for _, f := range globYAML(t, "roles") {
		var r role
		readYAML(t, f, &r)
		out = append(out, r)
	}
	return out
}

func servedSubresourceRequests(t *testing.T) []*http.Request {
	t.Helper()
	srv, _ := newTestAPI(t)
	var subresources []string
	for path := range v1alpha1Storage(&ExtraConfig{}) {
		if strings.Contains(path, "/") {
			subresources = append(subresources, path)
		}
	}
	if len(subresources) == 0 {
		t.Fatal("the storage map serves no subresources")
	}
	sort.Strings(subresources)

	var served []*http.Request
	for _, path := range subresources {
		resource, sub, _ := strings.Cut(path, "/")
		url := "/apis/" + v1alpha1.GroupName + "/v1alpha1/namespaces/demo/" + resource + "/ctx-a/" + sub
		routed := 0
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			// Only 405 means unrouted; any other answer reached the storage.
			if code, _ := do(t, srv, method, url, "application/json", "{}"); code == http.StatusMethodNotAllowed {
				continue
			}
			served = append(served, httptest.NewRequest(method, url, nil))
			routed++
		}
		if routed == 0 {
			t.Fatalf("%s is in the storage map but no method reaches it", path)
		}
	}
	return served
}

// A subresource permission belongs in every role granting the base verb.
func TestSubresourcePermissionsAreDeclaredAndGranted(t *testing.T) {
	declared := declaredPermissions(t)
	allRoles := roles(t)
	resolver := &request.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}

	for _, req := range servedSubresourceRequests(t) {
		t.Run(req.Method+" "+req.URL.Path, func(t *testing.T) {
			info, err := resolver.NewRequestInfo(req)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if info.Subresource == "" {
				t.Fatalf("resolved %+v, want a subresource", info)
			}
			base := info.APIGroup + "/" + info.Resource + "." + info.Verb
			permission := info.APIGroup + "/" + info.Resource + "/" + info.Subresource + "." + info.Verb
			if !declared.Has(permission) {
				t.Errorf("Milo asks for %s, which no ProtectedResource declares", permission)
			}
			granted := false
			for _, r := range allRoles {
				has := sets.New(r.Spec.IncludedPermissions...)
				if has.Has(base) && !has.Has(permission) {
					t.Errorf("%s grants %s but not %s", r.Metadata.Name, base, permission)
				}
				granted = granted || has.Has(permission)
			}
			if !granted {
				t.Errorf("no role grants %s", permission)
			}
		})
	}
}

func TestRolesGrantOnlyDeclaredPermissions(t *testing.T) {
	declared := declaredPermissions(t)
	for _, r := range roles(t) {
		for _, permission := range r.Spec.IncludedPermissions {
			if !declared.Has(permission) {
				t.Errorf("%s grants %s, which no ProtectedResource declares", r.Metadata.Name, permission)
			}
		}
	}
}
