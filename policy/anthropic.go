package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jwarykowski/drover/loop"
)

// NewAnthropicReasoner builds a reasoner with a configured client. A zero-value
// anthropic.Client has no base URL and every call errors, so construct through
// NewClient (which reads ANTHROPIC_API_KEY / an ant-login profile) rather than
// composing the struct literal directly.
func NewAnthropicReasoner(opts ...option.RequestOption) AnthropicReasoner {
	return AnthropicReasoner{Client: anthropic.NewClient(opts...)}
}

// AnthropicReasoner is the real Reasoner: it asks Claude to propose tasks for an
// event, constrained to a single tool whose schema mirrors shepherd's item
// grammar. This is the only file in drover that imports the Anthropic SDK.
//
// Output is a fixed vocabulary (the propose_tasks tool), never free-form text —
// the model cannot return anything the loop can't validate. A zero-value Client
// reads ANTHROPIC_API_KEY (or an `ant auth login` profile) from the environment.
type AnthropicReasoner struct {
	Client anthropic.Client
	Model  anthropic.Model // defaults to Claude Opus 4.8
}

const proposeToolName = "propose_tasks"

func (a AnthropicReasoner) model() anthropic.Model {
	if a.Model == "" {
		return anthropic.ModelClaudeOpus4_8
	}
	return a.Model
}

// proposal mirrors the propose_tasks tool input; the outer object carries the
// task list so tool_choice can force exactly one structured call.
type proposal struct {
	Tasks []struct {
		Text     string `json:"text"`
		Category string `json:"category,omitempty"`
		Priority string `json:"priority,omitempty"`
		Due      string `json:"due,omitempty"`
		Link     string `json:"link,omitempty"`
		Note     string `json:"note,omitempty"`
	} `json:"tasks"`
}

// Propose runs one constrained tool call and returns the proposed specs. The
// caller (LLMReasoner) validates each against the vocabulary before acting.
func (a AnthropicReasoner) Propose(ctx context.Context, event loop.Event, board []loop.Item, schema []byte) ([]loop.Spec, error) {
	tool := anthropic.ToolParam{
		Name:        proposeToolName,
		Description: anthropic.String("Propose zero or more todo items for the event. Only use fields and values valid per the shepherd item schema."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"tasks": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text":     map[string]any{"type": "string"},
							"category": map[string]any{"type": "string"},
							"priority": map[string]any{"enum": []string{"H", "M", "L"}},
							"due":      map[string]any{"type": "string", "description": "YYYY-MM-DD"},
							"link":     map[string]any{"type": "string"},
							"note":     map[string]any{"type": "string"},
						},
						"required":             []string{"text"},
						"additionalProperties": false,
					},
				},
			},
			Required:    []string{"tasks"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}

	resp, err := a.Client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     a.model(),
		MaxTokens: 2048,
		System: []anthropic.TextBlockParam{{
			Text: buildSystemPrompt(schema),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(buildUserPrompt(event, board))),
		},
		Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceParamOfTool(proposeToolName),
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}

	for _, block := range resp.Content {
		if block.Type != "tool_use" || block.Name != proposeToolName {
			continue
		}
		var p proposal
		if err := json.Unmarshal([]byte(block.JSON.Input.Raw()), &p); err != nil {
			return nil, fmt.Errorf("anthropic: parse tool input: %w", err)
		}
		specs := make([]loop.Spec, 0, len(p.Tasks))
		for _, t := range p.Tasks {
			specs = append(specs, loop.Spec{
				Text: t.Text, Category: t.Category, Priority: t.Priority,
				Due: t.Due, Link: t.Link, Note: t.Note,
			})
		}
		return specs, nil
	}
	return nil, nil // model proposed nothing
}
