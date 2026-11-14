package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/milo-os/assistant/agentcore"
)

// AskChoiceToolName is the built-in tool that puts a pick-one question to the
// person as buttons. It is offered only where the client draws them (see
// [Conversation.Run]); everywhere else the model asks in prose.
const AskChoiceToolName = "ask_choice"

const (
	// minChoiceOptions and maxChoiceOptions bound one question. One option is
	// not a choice, and past four a row of buttons stops being quicker to
	// read than a sentence.
	minChoiceOptions = 2
	maxChoiceOptions = 4
	// maxChoiceQuestionLen and maxChoiceLabelLen keep a question to a line
	// and a label to a button, in runes.
	maxChoiceQuestionLen = 200
	maxChoiceLabelLen    = 60
)

// choiceIconNames are the icon names a client knows how to draw, in a fixed
// order so the tool's schema is byte-identical on every turn (a reordered
// enum would defeat prompt caching). An icon outside this list is dropped
// rather than refused: it is decoration, and a question is still worth asking
// without it.
var choiceIconNames = []string{
	"clock", "container", "database", "domain", "globe",
	"laptop", "location", "network", "server", "shield",
}

func knownChoiceIcon(name string) bool {
	for _, n := range choiceIconNames {
		if n == name {
			return true
		}
	}
	return false
}

// Choices is one question put to the person as buttons.
type Choices struct {
	// ID is the tool call's id, so a client can tell two questions apart.
	ID       string
	Question string
	Options  []ChoiceOption
}

// ChoiceOption is one button. ID is its 1-based position, as a string.
type ChoiceOption struct {
	ID    string
	Label string
	Icon  string
}

// parseChoices reads and checks an ask_choice call's input. The same check
// runs in the tool, which reports a problem back to the model, and in the
// stream, which only shows a question that passed it.
func parseChoices(id string, input json.RawMessage) (Choices, error) {
	var args struct {
		Question string `json:"question"`
		Options  []struct {
			Label string `json:"label"`
			Icon  string `json:"icon"`
		} `json:"options"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return Choices{}, fmt.Errorf("%s: input is not valid JSON: %w", AskChoiceToolName, err)
	}
	question := strings.Join(strings.Fields(args.Question), " ")
	if question == "" {
		return Choices{}, errors.New(AskChoiceToolName + ": question is required")
	}
	if len([]rune(question)) > maxChoiceQuestionLen {
		return Choices{}, fmt.Errorf("%s: question is longer than %d characters; ask it in one short sentence", AskChoiceToolName, maxChoiceQuestionLen)
	}
	if n := len(args.Options); n < minChoiceOptions || n > maxChoiceOptions {
		return Choices{}, fmt.Errorf("%s: give %d to %d options, got %d", AskChoiceToolName, minChoiceOptions, maxChoiceOptions, n)
	}
	out := Choices{ID: id, Question: question, Options: make([]ChoiceOption, 0, len(args.Options))}
	seen := map[string]bool{}
	for i, o := range args.Options {
		label := strings.Join(strings.Fields(o.Label), " ")
		if label == "" {
			return Choices{}, fmt.Errorf("%s: option %d has no label", AskChoiceToolName, i+1)
		}
		if len([]rune(label)) > maxChoiceLabelLen {
			return Choices{}, fmt.Errorf("%s: option %d is longer than %d characters; use a few words", AskChoiceToolName, i+1, maxChoiceLabelLen)
		}
		key := strings.ToLower(label)
		if seen[key] {
			return Choices{}, fmt.Errorf("%s: option %q appears twice", AskChoiceToolName, label)
		}
		seen[key] = true
		icon := o.Icon
		if !knownChoiceIcon(icon) {
			icon = ""
		}
		out.Options = append(out.Options, ChoiceOption{ID: strconv.Itoa(i + 1), Label: label, Icon: icon})
	}
	return out, nil
}

// choicesText renders a question the way conversation memory keeps it. History
// stores prose only, so without this the next turn would see the person's
// pick with no question before it; it is also what a reloaded chat shows.
func choicesText(c Choices) string {
	var b strings.Builder
	b.WriteString(c.Question)
	for _, o := range c.Options {
		b.WriteString("\n- ")
		b.WriteString(o.Label)
	}
	return b.String()
}

// askChoiceTool is the model-facing half of [AskChoiceToolName]. Running it
// only checks the input: the stream is what shows the buttons, and the loop
// ends the turn after it (see [agentcore.LoopOptions].EndsTurn and
// endsTurnOn).
type askChoiceTool struct{}

func (askChoiceTool) Definition() agentcore.ToolDefinition {
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "The question, in one short sentence.",
			},
			"options": map[string]any{
				"type":     "array",
				"minItems": minChoiceOptions,
				"maxItems": maxChoiceOptions,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"label": map[string]any{
							"type":        "string",
							"description": "What the button says, in a few words. It is sent back as the person's reply.",
						},
						"icon": map[string]any{
							"type":        "string",
							"enum":        choiceIconNames,
							"description": "Optional icon for the button.",
						},
					},
					"required": []string{"label"},
				},
			},
		},
		"required": []string{"question", "options"},
	})
	return agentcore.ToolDefinition{
		Name: AskChoiceToolName,
		Description: "Put a pick-one question to the person as buttons. Use it whenever the answer is one of a few " +
			"known options; ask in prose when the answer is open, such as a name, an image or a port. The person " +
			"can still type something else. Calling it ends your reply: say anything that should come before the " +
			"question first, and don't repeat the question or the options in your text. Their pick arrives as " +
			"their next message.",
		InputSchema: schema,
	}
}

func (askChoiceTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	if _, err := parseChoices("", input); err != nil {
		return "", err
	}
	return "Shown to the person as buttons. Their answer will be their next message.", nil
}

// withChoiceTool adds [AskChoiceToolName] to tools on a surface whose client
// draws buttons. Elsewhere it returns tools unchanged. tools is copied, never
// modified: it can be the composed set itself.
func withChoiceTool(tools agentcore.ToolSet, surface *Surface) agentcore.ToolSet {
	if !drawsCards(surface) {
		return tools
	}
	out := make(agentcore.ToolSet, len(tools)+1)
	for name, t := range tools {
		out[name] = t
	}
	out[AskChoiceToolName] = askChoiceTool{}
	return out
}

// drawsCards reports whether the client on surface draws choice buttons and
// plan cards. Only the portal's home page does today.
func drawsCards(surface *Surface) bool {
	return surface != nil && surface.Name == surfaceHomeFirstRun
}
