package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/history"
)

func TestParseChoices(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Choices
		wantErr string
	}{
		{
			name:  "valid with icons",
			input: `{"question":"What do you want to deploy?","options":[{"label":"My own image","icon":"container"},{"label":"Try  the nginx demo","icon":"globe"}]}`,
			want: Choices{ID: "c1", Question: "What do you want to deploy?", Options: []ChoiceOption{
				{ID: "1", Label: "My own image", Icon: "container"},
				{ID: "2", Label: "Try the nginx demo", Icon: "globe"},
			}},
		},
		{
			name:  "unknown icon is dropped",
			input: `{"question":"Pick","options":[{"label":"A","icon":"rocket"},{"label":"B"}]}`,
			want:  Choices{ID: "c1", Question: "Pick", Options: []ChoiceOption{{ID: "1", Label: "A"}, {ID: "2", Label: "B"}}},
		},
		{name: "not json", input: `{`, wantErr: "not valid JSON"},
		{name: "no question", input: `{"question":"  ","options":[{"label":"A"},{"label":"B"}]}`, wantErr: "question is required"},
		{name: "one option", input: `{"question":"Q","options":[{"label":"A"}]}`, wantErr: "give 2 to 4 options, got 1"},
		{name: "five options", input: `{"question":"Q","options":[{"label":"A"},{"label":"B"},{"label":"C"},{"label":"D"},{"label":"E"}]}`, wantErr: "got 5"},
		{name: "empty label", input: `{"question":"Q","options":[{"label":"A"},{"label":" "}]}`, wantErr: "option 2 has no label"},
		{name: "duplicate label", input: `{"question":"Q","options":[{"label":"Yes"},{"label":"yes"}]}`, wantErr: "appears twice"},
		{name: "long label", input: `{"question":"Q","options":[{"label":"A"},{"label":"` + strings.Repeat("x", maxChoiceLabelLen+1) + `"}]}`, wantErr: "option 2 is longer"},
		{name: "long question", input: `{"question":"` + strings.Repeat("q", maxChoiceQuestionLen+1) + `","options":[{"label":"A"},{"label":"B"}]}`, wantErr: "question is longer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChoices("c1", json.RawMessage(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != tt.want.ID || got.Question != tt.want.Question || len(got.Options) != len(tt.want.Options) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range got.Options {
				if got.Options[i] != tt.want.Options[i] {
					t.Fatalf("option %d = %+v, want %+v", i, got.Options[i], tt.want.Options[i])
				}
			}
		})
	}
}

func TestAskChoiceToolReportsBadInputToTheModel(t *testing.T) {
	if _, err := (askChoiceTool{}).Execute(context.Background(), json.RawMessage(`{"question":"Q","options":[]}`)); err == nil {
		t.Fatal("a question with no options must come back to the model as an error")
	}
	out, err := (askChoiceTool{}).Execute(context.Background(), json.RawMessage(`{"question":"Q","options":[{"label":"A"},{"label":"B"}]}`))
	if err != nil || out == "" {
		t.Fatalf("valid input: out=%q err=%v", out, err)
	}
}

func TestAskChoiceToolSchemaIsStable(t *testing.T) {
	a, b := (askChoiceTool{}).Definition(), (askChoiceTool{}).Definition()
	if string(a.InputSchema) != string(b.InputSchema) {
		t.Fatal("the schema must be byte-identical on every turn, or prompt caching breaks")
	}
}

func TestWithChoiceToolOnlyOnTheHomePage(t *testing.T) {
	base := agentcore.ToolSet{"resources_list": askChoiceTool{}}
	tests := []struct {
		name    string
		surface *Surface
		want    bool
	}{
		{name: "no surface", surface: nil},
		{name: "other surface", surface: &Surface{Name: "settings"}},
		{name: "home first run", surface: &Surface{Name: surfaceHomeFirstRun}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools := withChoiceTool(base, tt.surface)
			if _, has := tools[AskChoiceToolName]; has != tt.want {
				t.Fatalf("tool offered = %v, want %v", has, tt.want)
			}
			if _, leaked := base[AskChoiceToolName]; leaked {
				t.Fatal("the caller's tool set must not be modified")
			}
		})
	}
}

// choiceAskingModel writes a sentence, then asks a question with ask_choice.
// It fails the test if it is called a second time, since the turn must end
// on the question.
type choiceAskingModel struct {
	t     *testing.T
	calls int
	tools []string
}

func (m *choiceAskingModel) ModelID() string { return "choice-asking" }

func (m *choiceAskingModel) Stream(_ context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	m.calls++
	if m.calls > 1 {
		m.t.Error("the model was asked again after ask_choice; the turn should have ended")
	}
	for _, d := range req.Tools {
		m.tools = append(m.tools, d.Name)
	}
	input := json.RawMessage(`{"question":"What do you want to deploy?","options":[{"label":"My own image","icon":"container"},{"label":"Try the nginx demo"}]}`)
	return &partReader{parts: []agentcore.StreamPart{
		{Kind: agentcore.StreamPartTextDelta, Text: "Let's get you deployed."},
		{Kind: agentcore.StreamPartToolCall, ToolCall: &agentcore.ToolCall{ID: "call-1", Name: AskChoiceToolName, Input: input}},
		{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishToolCalls, Usage: agentcore.Usage{Input: 10, Output: 5}},
	}}, nil
}

func TestTurnEndsOnChoicesAndRemembersThem(t *testing.T) {
	store := history.NewMemoryStore()
	model := &choiceAskingModel{t: t}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(), History: store})
	stream := conv.Run(context.Background(), Params{
		UserText: "I want to run an app", ProjectName: "demo", ContextID: "ctx-1", TaskID: "t1",
		Surface: &Surface{Name: surfaceHomeFirstRun, Network: "default"},
	})
	events := drainEvents(t, stream)

	var choices []*Choices
	for _, e := range events {
		switch e.Kind {
		case EventChoices:
			choices = append(choices, e.Choices)
		case EventToolCall, EventToolResult:
			if e.ToolName == AskChoiceToolName {
				t.Errorf("ask_choice must not show as a tool step, got %s", e.Kind)
			}
		}
	}
	if len(choices) != 1 || choices[0].ID != "call-1" || len(choices[0].Options) != 2 {
		t.Fatalf("choices events = %+v, want one question with two options", choices)
	}
	if !contains(model.tools, AskChoiceToolName) {
		t.Fatalf("the model was not offered %s on the home page: %v", AskChoiceToolName, model.tools)
	}
	if res := stream.Result(); res.State != StateCompleted {
		t.Fatalf("state = %s, want completed", res.State)
	}

	turns, err := store.Turns(context.Background(), "demo", "ctx-1")
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns = %+v, err = %v", turns, err)
	}
	want := "Let's get you deployed.\n\nWhat do you want to deploy?\n- My own image\n- Try the nginx demo"
	if turns[0].AssistantText != want {
		t.Fatalf("stored answer =\n%q\nwant\n%q", turns[0].AssistantText, want)
	}
}

func TestAskChoiceIsNotOfferedOffTheHomePage(t *testing.T) {
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter()})
	drainEvents(t, conv.Run(context.Background(), Params{UserText: "hi", ProjectName: "demo", TaskID: "t1"}))
	for _, d := range model.requests[0].Tools {
		if d.Name == AskChoiceToolName {
			t.Fatal("ask_choice must only be offered where the client draws buttons")
		}
	}
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
