package basetools_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/milo-os/assistant/internal/basetools"
)

// Everything this package puts into words is read either by the model or, via
// the model, by a customer. So the vocabulary has to be theirs.
//
// The test for a word: does a customer meet it in something they write or
// something they are already shown? Then it survives. Does it only exist inside
// how the platform is built? Then reading it teaches them nothing, and there is
// always a sentence that says what actually happened instead.
var internalVocabulary = []struct {
	pattern *regexp.Regexp
	why     string
}{
	{regexp.MustCompile(`(?i)\bcontrol planes?\b`),
		"how the platform is built, not something a person with a stuck resource reasons about. " +
			"The thing they own and can see is their project."},
	{regexp.MustCompile(`(?i)\breconcil`),
		"describes how the platform works internally, never what happened to the customer's resource."},
	{regexp.MustCompile(`(?i)\bcontrollers?\b`),
		"the reader does not know what one is. Say the platform, or say what it has not done yet."},
	{regexp.MustCompile(`(?i)\brbac\b`),
		"internal authorization vocabulary. Say who can grant access instead."},
	{regexp.MustCompile(`(?i)\badmission\b`),
		"the extension point that rejected the request. They need to know they were rejected and by " +
			"what, not which part did it."},
	{regexp.MustCompile(`(?i)\bpods?\b`),
		"the runtime unit underneath somebody's resource. Customers never see one."},
	{regexp.MustCompile(`(?i)\bcrds?\b`),
		"how a kind gets served. Say the kind, or say the project does not offer it."},
	{regexp.MustCompile(`(?i)\bmilo\b`),
		"the name of an internal service. It means nothing outside the platform team."},
	{regexp.MustCompile(`(?i)\ballowance\s?buckets?\b`),
		"the internal object holding an allowance. Say the project's allowance — that is the word " +
			"they already meet when something is refused for want of one."},
	{regexp.MustCompile(`(?i)\bwebhooks?\b`),
		"an implementation detail of how a rejection reached them."},
}

// The prose is checked, not the identifiers: a group name or a label key is
// evidence a person can quote, and several of them legitimately contain a word
// that is banned as English. A literal with no space in it is an identifier.
func TestCopyIsInTheCustomersVocabulary(t *testing.T) {
	for _, literal := range prose(t) {
		for _, banned := range internalVocabulary {
			if banned.pattern.MatchString(literal) {
				t.Errorf("customer-facing copy uses %q:\n  %s\n  why: %s",
					banned.pattern.String(), literal, banned.why)
			}
		}
	}
}

// The tool descriptions are what the model reads to decide what to call, and
// they are the copy most likely to be paraphrased straight back to a person.
func TestEveryToolSaysWhatItDoesAndWhetherItWrites(t *testing.T) {
	for name, tool := range newPlatform(t).tools(t, "demo", "tok") {
		definition := tool.Definition()
		if definition.Name != name {
			t.Errorf("%s is registered under a different name than it announces (%s)", name, definition.Name)
		}
		if len(definition.Description) < 120 {
			t.Errorf("%s: the description is too thin for a model to choose it well", name)
		}
		switch name {
		case basetools.ResourcesApplyToolName:
			// The one tool that writes. It has to say what it takes to call it.
			if !strings.Contains(definition.Description, "said yes") {
				t.Errorf("%s: the only tool that writes must say what it takes to call it", name)
			}
		case basetools.ResourcesPlanToolName:
			if !strings.Contains(definition.Description, "changes nothing") {
				t.Errorf("%s: planning writes nothing and must say so", name)
			}
		case basetools.ResourcesValidateToolName:
			if !strings.Contains(definition.Description, "Writes nothing.") {
				t.Errorf("%s: a tool that changes nothing must say so", name)
			}
		default:
			if !strings.Contains(definition.Description, "Read-only.") {
				t.Errorf("%s: a tool that changes nothing must say so", name)
			}
		}
	}
}

func TestThePromptSectionNamesTheToolsItDescribes(t *testing.T) {
	section := basetools.PromptSection(false)
	for _, name := range []string{
		basetools.ResourcesListToolName,
		basetools.ResourcesGetToolName,
		basetools.SchemaGetToolName,
	} {
		if !strings.Contains(section, name) {
			t.Errorf("the prompt section does not mention %s", name)
		}
	}
	// A turn with no change path must not advertise one.
	for _, name := range basetools.MutatingToolNames() {
		if strings.Contains(section, name) {
			t.Errorf("the read-only prompt section mentions %s", name)
		}
	}
}

// The step between a plan and an apply is a person, and the prompt is the only
// place that can require it.
func TestThePromptSectionRequiresAnExplicitYes(t *testing.T) {
	section := basetools.PromptSection(true)
	for _, name := range []string{
		basetools.ResourcesValidateToolName,
		basetools.ResourcesPlanToolName,
		basetools.ResourcesApplyToolName,
	} {
		if !strings.Contains(section, name) {
			t.Errorf("the prompt section does not mention %s", name)
		}
	}
	for _, phrase := range []string{"explicit yes", "not a yes"} {
		if !strings.Contains(section, phrase) {
			t.Errorf("the prompt section does not say %q", phrase)
		}
	}
}

// prose returns every multi-word string literal this package can put in front
// of a person.
func prose(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	var out []string
	for _, pkg := range pkgs {
		ast.Inspect(pkg, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(value, " ") {
				return true
			}
			out = append(out, value)
			return true
		})
	}
	if len(out) == 0 {
		t.Fatal("no copy was found to check; the scan is broken")
	}
	return out
}
