package web

import (
	"encoding/json"
	"testing"
)

// /v1/messages is the endpoint Claude Code talks to, and the Anthropic -> OpenAI
// translation below it had no tests at all. These cover the parts that decide
// whether a request reaches the model at all: the system prompt, every content
// block type, tool schemas and tool_choice.
func TestAnthropicOpenAISystemPromptForms(t *testing.T) {
	cases := []struct {
		name string
		sys  any
		want any
	}{
		{"string", "be terse", "be terse"},
		// A block array is forwarded verbatim: flattenPromptMessages downstream is
		// what extracts attachments from blocks, so the translation must not
		// flatten it here.
		{"block array", []any{map[string]any{"type": "text", "text": "block form"}},
			[]any{map[string]any{"type": "text", "text": "block form"}}},
		{"multi-block", []any{
			map[string]any{"type": "text", "text": "one"},
			map[string]any{"type": "text", "text": "two"},
		}, []any{
			map[string]any{"type": "text", "text": "one"},
			map[string]any{"type": "text", "text": "two"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, err := (anthropicRequest{System: c.sys}).openAI()
			if err != nil {
				t.Fatal(err)
			}
			if len(o.Messages) != 1 || o.Messages[0].Role != "system" {
				t.Fatalf("messages = %#v, want one system message", o.Messages)
			}
			switch want := c.want.(type) {
			case string:
				if got, ok := o.Messages[0].Content.(string); !ok || got != want {
					t.Fatalf("system content = %#v, want %q", o.Messages[0].Content, want)
				}
			case []any:
				got, ok := o.Messages[0].Content.([]any)
				if !ok || len(got) != len(want) {
					t.Fatalf("system content = %#v, want %d block(s)", o.Messages[0].Content, len(want))
				}
			}
		})
	}
}

func TestAnthropicOpenAIMessageContent(t *testing.T) {
	t.Run("plain string", func(t *testing.T) {
		o, err := (anthropicRequest{Messages: []anthropicMessage{
			{Role: "user", Content: "hello"},
		}}).openAI()
		if err != nil {
			t.Fatal(err)
		}
		if len(o.Messages) != 1 || o.Messages[0].Role != "user" || o.Messages[0].Content != "hello" {
			t.Fatalf("messages = %#v", o.Messages)
		}
	})

	t.Run("image base64 becomes an input_image block", func(t *testing.T) {
		o, err := (anthropicRequest{Messages: []anthropicMessage{
			{Role: "user", Content: []any{map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": "aGk=",
				},
			}}},
		}}).openAI()
		if err != nil {
			t.Fatal(err)
		}
		blocks, ok := o.Messages[0].Content.([]any)
		if !ok || len(blocks) != 1 {
			t.Fatalf("content = %#v", o.Messages[0].Content)
		}
		b := blocks[0].(map[string]any)
		if b["type"] != "input_image" || b["image_url"] != "data:image/png;base64,aGk=" {
			t.Fatalf("image block = %#v", b)
		}
	})

	t.Run("image url keeps the remote address", func(t *testing.T) {
		o, err := (anthropicRequest{Messages: []anthropicMessage{
			{Role: "user", Content: []any{map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "url", "url": "https://example.com/x.png"},
			}}},
		}}).openAI()
		if err != nil {
			t.Fatal(err)
		}
		b := o.Messages[0].Content.([]any)[0].(map[string]any)
		if b["image_url"] != "https://example.com/x.png" {
			t.Fatalf("image_url = %#v", b)
		}
	})

	t.Run("tool_result becomes a tool message keyed by tool_use_id", func(t *testing.T) {
		o, err := (anthropicRequest{Messages: []anthropicMessage{
			{Role: "user", Content: []any{map[string]any{
				"type": "tool_result", "tool_use_id": "call_1", "content": "42",
			}}},
		}}).openAI()
		if err != nil {
			t.Fatal(err)
		}
		if len(o.Messages) != 1 || o.Messages[0].Role != "tool" {
			t.Fatalf("messages = %#v", o.Messages)
		}
		if o.Messages[0].ToolCallID != "call_1" || o.Messages[0].Content != "42" {
			t.Fatalf("tool message = %#v", o.Messages[0])
		}
	})

	t.Run("tool_use becomes an assistant tool call", func(t *testing.T) {
		o, err := (anthropicRequest{Messages: []anthropicMessage{
			{Role: "assistant", Content: []any{map[string]any{
				"type": "tool_use", "id": "call_2", "name": "weather", "input": map[string]any{"city": "sf"},
			}}},
		}}).openAI()
		if err != nil {
			t.Fatal(err)
		}
		calls := o.Messages[0].ToolCalls
		if len(calls) != 1 || calls[0]["id"] != "call_2" {
			t.Fatalf("tool calls = %#v", calls)
		}
		fn := calls[0]["function"].(map[string]any)
		if fn["name"] != "weather" {
			t.Fatalf("function = %#v", fn)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
			t.Fatalf("arguments is not JSON: %v (%q)", err, fn["arguments"])
		}
		if args["city"] != "sf" {
			t.Fatalf("arguments = %#v", args)
		}
	})

	t.Run("unknown block types are dropped rather than crashing", func(t *testing.T) {
		o, err := (anthropicRequest{Messages: []anthropicMessage{
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "keep"},
				map[string]any{"type": "thinking", "thinking": "drop"},
				"not even a map",
			}},
		}}).openAI()
		if err != nil {
			t.Fatal(err)
		}
		blocks, ok := o.Messages[0].Content.([]any)
		if !ok || len(blocks) != 1 {
			t.Fatalf("content = %#v, want only the text block", o.Messages[0].Content)
		}
		if b := blocks[0].(map[string]any); b["text"] != "keep" {
			t.Fatalf("text block = %#v", b)
		}
	})

	t.Run("a message whose content is neither string nor array is an error", func(t *testing.T) {
		if _, err := (anthropicRequest{Messages: []anthropicMessage{
			{Role: "user", Content: 3.14},
		}}).openAI(); err == nil {
			t.Fatal("expected an error for a numeric content block")
		}
	})
}

func TestAnthropicOpenAIToolsAndChoice(t *testing.T) {
	req := anthropicRequest{
		Tools: []anthropicTool{
			{Name: "weather", Description: "Get weather", InputSchema: map[string]any{"type": "object"}},
		},
		ToolChoice: map[string]any{"type": "tool", "name": "weather"},
	}
	o, err := req.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Tools) != 1 || o.Tools[0].Type != "function" {
		t.Fatalf("tools = %#v", o.Tools)
	}
	var schema map[string]any
	if err := json.Unmarshal(o.Tools[0].Function, &schema); err != nil {
		t.Fatalf("tool schema is not JSON: %v", err)
	}
	if schema["name"] != "weather" {
		t.Fatalf("tool schema = %#v", schema)
	}
	if schema["parameters"] == nil {
		t.Fatal("tool schema lost input_schema (it must become OpenAI's parameters)")
	}
	// Anthropic's {type:"tool",name:"x"} is OpenAI's forced function call.
	choice := o.ToolChoice.(map[string]any)
	if choice["type"] != "function" {
		t.Fatalf("tool_choice = %#v", o.ToolChoice)
	}
	if got := choice["function"].(map[string]any)["name"]; got != "weather" {
		t.Fatalf("forced function name = %#v", o.ToolChoice)
	}
}

func TestAnthropicOpenAIToolChoiceMapping(t *testing.T) {
	cases := map[string]string{
		"auto": "auto",
		"any":  "required",
		"none": "none",
	}
	for in, want := range cases {
		o, err := (anthropicRequest{ToolChoice: map[string]any{"type": in}}).openAI()
		if err != nil {
			t.Fatal(err)
		}
		if o.ToolChoice != want {
			t.Errorf("tool_choice %q = %#v, want %q", in, o.ToolChoice, want)
		}
	}
}

// max_tokens and stop_sequences are Anthropic-only request fields; they must be
// forwarded or the model's output length and stop behaviour silently change.
func TestAnthropicOpenAICarriesLimitsAndStops(t *testing.T) {
	o, err := (anthropicRequest{MaxTokens: 512, StopSequences: []string{"END", "\n\n"}}).openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.MaxCompletionTokens == nil || *o.MaxCompletionTokens != 512 {
		t.Fatalf("MaxCompletionTokens = %#v", o.MaxCompletionTokens)
	}
	// Stop is typed as any on oaiReq because the OpenAI API accepts both a
	// string and an array; assert the array form the translation produces.
	stops, ok := o.Stop.([]string)
	if !ok || len(stops) != 2 || stops[0] != "END" || stops[1] != "\n\n" {
		t.Fatalf("Stop = %#v", o.Stop)
	}
	// max_tokens is required by the Anthropic API; a request without it must not
	// set a zero pointer that would clamp the model's output to zero.
	o, err = (anthropicRequest{}).openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.MaxCompletionTokens != nil {
		t.Fatalf("MaxCompletionTokens = %#v, want nil", *o.MaxCompletionTokens)
	}
}

// The stream flag decides whether /v1/messages answers with SSE, so a dropped
// flag would turn a streaming client's request into a non-streaming one.
func TestAnthropicOpenAICarriesStreamFlag(t *testing.T) {
	o, err := (anthropicRequest{Stream: true}).openAI()
	if err != nil {
		t.Fatal(err)
	}
	if !o.Stream {
		t.Fatal("stream=true was dropped")
	}
}
