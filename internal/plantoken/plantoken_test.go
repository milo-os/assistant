package plantoken_test

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/milo-os/assistant/internal/plantoken"
)

var key = []byte("a-key-long-enough-to-be-a-secret")

func binding() plantoken.Binding {
	return plantoken.Binding{
		Project:          "demo-project",
		Manifests:        [][]byte{[]byte(`{"kind":"Workload","spec":{"replicas":2}}`)},
		ResourceVersions: []string{"991"},
	}
}

func mintNow(t *testing.T, b plantoken.Binding) string {
	t.Helper()
	return plantoken.Mint(key, b, time.Now().Add(plantoken.TTL))
}

func TestATokenCoversTheThingItWasMintedFor(t *testing.T) {
	if err := plantoken.Verify(key, mintNow(t, binding()), binding(), time.Now(), "plan"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// One changed character is enough.
func TestATamperedManifestIsRefused(t *testing.T) {
	token := mintNow(t, binding())

	tampered := binding()
	tampered.Manifests = [][]byte{[]byte(`{"kind":"Workload","spec":{"replicas":3}}`)}

	err := plantoken.Verify(key, token, tampered, time.Now(), "resources_plan")
	if err == nil {
		t.Fatal("a manifest changed after the plan was applied anyway")
	}
	assertRefusalIsUseful(t, err.Error(), "resources_plan")
}

// The same manifests reordered are a different plan: order is part of the
// agreement, and applying two changes the other way round can mean something
// else entirely.
func TestReorderedManifestsAreRefused(t *testing.T) {
	planned := binding()
	planned.Manifests = [][]byte{[]byte(`{"kind":"Network"}`), []byte(`{"kind":"Workload"}`)}
	planned.ResourceVersions = []string{"", ""}
	token := mintNow(t, planned)

	swapped := planned
	swapped.Manifests = [][]byte{[]byte(`{"kind":"Workload"}`), []byte(`{"kind":"Network"}`)}

	if err := plantoken.Verify(key, token, swapped, time.Now(), "resources_plan"); err == nil {
		t.Fatal("a reordered plan was accepted")
	}
}

func TestATokenFromAnotherProjectIsRefused(t *testing.T) {
	token := mintNow(t, binding())

	elsewhere := binding()
	elsewhere.Project = "someone-elses-project"

	if err := plantoken.Verify(key, token, elsewhere, time.Now(), "resources_plan"); err == nil {
		t.Fatal("a token minted in another project was spent here")
	}
}

// Somebody else changing the object means the agreed change no longer
// describes what would happen.
func TestAMovedResourceVersionIsRefused(t *testing.T) {
	token := mintNow(t, binding())

	moved := binding()
	moved.ResourceVersions = []string{"992"}

	err := plantoken.Verify(key, token, moved, time.Now(), "resources_plan")
	if err == nil {
		t.Fatal("a resource somebody else changed was overwritten")
	}
	if !strings.Contains(err.Error(), "changed by somebody else") {
		t.Fatalf("the refusal should name this case: %q", err)
	}
}

// A create and an update of the same manifest are different plans.
func TestAPlanForACreateDoesNotAuthorizeAnUpdate(t *testing.T) {
	planned := binding()
	planned.ResourceVersions = []string{""} // nothing there when the plan was made
	token := mintNow(t, planned)

	nowExists := planned
	nowExists.ResourceVersions = []string{"1"}

	if err := plantoken.Verify(key, token, nowExists, time.Now(), "resources_plan"); err == nil {
		t.Fatal("a plan to create was spent on an update")
	}
}

func TestAnExpiredPlanIsRefused(t *testing.T) {
	token := plantoken.Mint(key, binding(), time.Now().Add(-time.Second))

	err := plantoken.Verify(key, token, binding(), time.Now(), "resources_plan")
	if err == nil {
		t.Fatal("an expired plan was applied")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("the refusal should say so plainly: %q", err)
	}
	assertRefusalIsUseful(t, err.Error(), "resources_plan")
}

// Moving the expiry does not extend it: the expiry is covered by the hash.
func TestTheExpiryCannotBeMovedForward(t *testing.T) {
	token := plantoken.Mint(key, binding(), time.Now().Add(-time.Hour))
	encoded, _, _ := strings.Cut(token, ".")
	extended := encoded + "." + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)

	if err := plantoken.Verify(key, extended, binding(), time.Now(), "resources_plan"); err == nil {
		t.Fatal("an expiry was moved forward and accepted")
	}
}

func TestAMalformedTokenIsRefused(t *testing.T) {
	for _, token := range []string{"", "not-a-token", "abc.notanumber", "....", "abc."} {
		if err := plantoken.Verify(key, token, binding(), time.Now(), "resources_plan"); err == nil {
			t.Fatalf("%q was accepted", token)
		}
	}
}

func TestATokenFromAnotherKeyIsRefused(t *testing.T) {
	token := plantoken.Mint([]byte("a-completely-different-secret-key"), binding(), time.Now().Add(plantoken.TTL))

	if err := plantoken.Verify(key, token, binding(), time.Now(), "resources_plan"); err == nil {
		t.Fatal("a token minted with another key was accepted")
	}
}

// Two bindings differing only in where the manifest boundary falls must not
// hash alike.
func TestManifestBoundariesAreUnambiguous(t *testing.T) {
	one := plantoken.Binding{Project: "p", Manifests: [][]byte{[]byte("ab"), []byte("c")}}
	two := plantoken.Binding{Project: "p", Manifests: [][]byte{[]byte("a"), []byte("bc")}}
	expiry := time.Now().Add(plantoken.TTL)

	if plantoken.Mint(key, one, expiry) == plantoken.Mint(key, two, expiry) {
		t.Fatal("two different plans hashed the same")
	}
}

func TestResolveKeyPrefersAConfiguredSecret(t *testing.T) {
	raw := "a-configured-secret-that-is-long"
	got, err := plantoken.ResolveKey(raw, nil)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if !bytes.Equal(got, []byte(raw)) {
		t.Fatalf("key = %q, want the configured one", got)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	got, err = plantoken.ResolveKey(encoded, nil)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if !bytes.Equal(got, []byte("0123456789abcdef0123456789abcdef")) {
		t.Fatalf("key = %q, want the decoded one", got)
	}
}

func TestResolveKeyRefusesASecretTooShortToBeOne(t *testing.T) {
	if _, err := plantoken.ResolveKey("short", nil); err == nil {
		t.Fatal("a five-character key was accepted")
	}
}

// Without a configured key the guarantee still holds. A plan just cannot
// cross a restart, and the service says so loudly.
func TestResolveKeyGeneratesOneAndWarns(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))

	first, err := plantoken.ResolveKey("", logger)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if len(first) < 32 {
		t.Fatalf("generated key is %d bytes", len(first))
	}
	if !strings.Contains(logged.String(), "PLAN_TOKEN_KEY") {
		t.Fatalf("the warning does not name the setting: %q", logged.String())
	}

	second, _ := plantoken.ResolveKey("", logger)
	if bytes.Equal(first, second) {
		t.Fatal("two generated keys were identical")
	}
}

// Every refusal is read by a person who has just been told nothing happened.
func assertRefusalIsUseful(t *testing.T, refusal, planTool string) {
	t.Helper()
	if !strings.Contains(refusal, "nothing was changed") {
		t.Errorf("the refusal does not say nothing happened: %q", refusal)
	}
	if !strings.Contains(refusal, planTool) {
		t.Errorf("the refusal does not say what to do instead: %q", refusal)
	}
}
