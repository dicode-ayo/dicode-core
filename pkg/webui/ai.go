// Package webui — AI chat handler.
// Uses the OpenAI-compatible Chat Completions API (works with OpenAI, Claude via
// api.anthropic.com/v1, Ollama, and any other OpenAI-compatible endpoint).
// Streams text deltas live to the browser and writes task files the moment
// each write_file tool call finishes streaming.
package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dicode/dicode/pkg/agent"
	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"go.uber.org/zap"
)

// aiEvent is one SSE frame sent to the browser.
type aiEvent struct {
	Type     string `json:"type"`
	Content  string `json:"content,omitempty"`  // type=text
	Filename string `json:"filename,omitempty"` // type=file
	Message  string `json:"message,omitempty"`  // type=error
}

// handleAIStream handles POST /api/tasks/{id}/ai/stream.
// It calls the configured OpenAI-compatible LLM, streams text to the browser via
// SSE, and writes task files on disk as soon as each write_file call completes.
func (s *Server) handleAIStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	id := taskIDParam(r)
	spec, found := s.registry.Get(id)
	if !found {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	prompt := r.FormValue("prompt")
	if prompt == "" {
		http.Error(w, "prompt required", http.StatusBadRequest)
		return
	}

	// Resolve API key: direct value in config takes precedence over env var.
	apiKey := s.cfg.AI.APIKey
	if apiKey == "" {
		apiKeyEnv := s.cfg.AI.APIKeyEnv
		if apiKeyEnv == "" {
			apiKeyEnv = "OPENAI_API_KEY"
		}
		apiKey = os.Getenv(apiKeyEnv)
	}

	model := s.cfg.AI.Model
	if model == "" {
		model = "gpt-4o"
	}

	// Build the OpenAI-compatible client; optionally override the base URL.
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if base := s.cfg.AI.BaseURL; base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	client := openai.NewClient(opts...)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func(ev aiEvent) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	readFile := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(spec.TaskDir, name))
		return string(b)
	}

	// Seed the conversation: system context + user prompt.
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(buildAISystem(id, readFile("task.yaml"), readFile("task.js"), readFile("task.test.js"))),
		openai.UserMessage(prompt),
	}

	tools := aiTools()
	ctx := r.Context()

	// Agentic loop — first turn streams, follow-up turns are synchronous.
	for turn := 0; turn < 6; turn++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var next []openai.ChatCompletionMessageParamUnion
		var err error

		if turn == 0 {
			next, err = s.aiStreamTurn(ctx, &client, model, messages, tools, spec.TaskDir, id, send)
		} else {
			next, err = s.aiSyncTurn(ctx, &client, model, messages, tools, spec.TaskDir, id, send)
		}
		if err != nil {
			send(aiEvent{Type: "error", Message: err.Error()})
			return
		}
		if next == nil {
			break // no tool calls — model is done
		}
		messages = next
	}

	send(aiEvent{Type: "done"})
}

// aiStreamTurn runs one streaming turn.
// It forwards text deltas to the browser and executes write_file tool calls the
// instant each one finishes accumulating. Returns the updated message list
// (including the assistant turn + tool results) if tool calls were made, or nil
// if the model finished without calling any tools.
func (s *Server) aiStreamTurn(
	ctx context.Context,
	client *openai.Client,
	model string,
	messages []openai.ChatCompletionMessageParamUnion,
	tools []openai.ChatCompletionToolParam,
	taskDir, taskID string,
	send func(aiEvent),
) ([]openai.ChatCompletionMessageParamUnion, error) {

	stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Model:             model,
		Messages:          messages,
		Tools:             tools,
		ParallelToolCalls: openai.Bool(false), // sequential so JustFinishedToolCall is reliable
		MaxTokens:         openai.Int(4096),
	})

	acc := openai.ChatCompletionAccumulator{}
	type toolResult struct {
		id     string
		result string
	}
	var results []toolResult

	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)

		// Forward text delta to browser.
		if len(chunk.Choices) > 0 {
			if delta := chunk.Choices[0].Delta.Content; delta != "" {
				send(aiEvent{Type: "text", Content: delta})
			}
		}

		// Execute the tool call as soon as its arguments have fully streamed.
		if tc, ok := acc.JustFinishedToolCall(); ok {
			result := s.execWriteFile(tc.Name, tc.Arguments, taskDir, taskID, send)
			results = append(results, toolResult{id: tc.ID, result: result})
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}

	if len(results) == 0 {
		return nil, nil
	}

	// Build the next conversation turn: assistant message + all tool results.
	next := append(messages, acc.Choices[0].Message.ToParam())
	for _, tr := range results {
		next = append(next, openai.ToolMessage(tr.result, tr.id))
	}
	return next, nil
}

// aiSyncTurn runs one non-streaming follow-up turn (typically short acknowledgements).
// Returns nil if the model finished without calling more tools.
func (s *Server) aiSyncTurn(
	ctx context.Context,
	client *openai.Client,
	model string,
	messages []openai.ChatCompletionMessageParamUnion,
	tools []openai.ChatCompletionToolParam,
	taskDir, taskID string,
	send func(aiEvent),
) ([]openai.ChatCompletionMessageParamUnion, error) {

	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:             model,
		Messages:          messages,
		Tools:             tools,
		ParallelToolCalls: openai.Bool(false),
		MaxTokens:         openai.Int(1024),
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, nil
	}

	msg := resp.Choices[0].Message

	// Forward any text to the browser.
	if msg.Content != "" {
		send(aiEvent{Type: "text", Content: msg.Content})
	}

	if len(msg.ToolCalls) == 0 {
		return nil, nil
	}

	next := append(messages, msg.ToParam())
	for _, tc := range msg.ToolCalls {
		var inp map[string]string
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &inp)
		result := s.execWriteFile(tc.Function.Name, tc.Function.Arguments, taskDir, taskID, send)
		next = append(next, openai.ToolMessage(result, tc.ID))
	}
	return next, nil
}

// execWriteFile executes the write_file tool: parses arguments, writes the file,
// sends a file SSE event. Returns the result string for the tool result message.
func (s *Server) execWriteFile(toolName, argsJSON, taskDir, taskID string, send func(aiEvent)) string {
	if toolName != "write_file" {
		return "error: unknown tool " + toolName
	}
	var inp struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &inp); err != nil {
		return "error: bad arguments: " + err.Error()
	}
	allowed := map[string]bool{"task.js": true, "task.yaml": true, "task.test.js": true}
	if !allowed[inp.Filename] {
		return "error: file not allowed"
	}
	if err := os.WriteFile(filepath.Join(taskDir, inp.Filename), []byte(inp.Content), 0644); err != nil {
		return "error: " + err.Error()
	}
	send(aiEvent{Type: "file", Filename: inp.Filename, Content: inp.Content})
	s.log.Info("AI wrote file", zap.String("task", taskID), zap.String("file", inp.Filename))
	return "ok"
}

// aiTools returns the tool definitions for the chat completion request.
func aiTools() []openai.ChatCompletionToolParam {
	return []openai.ChatCompletionToolParam{{
		Function: openai.FunctionDefinitionParam{
			Name:        "write_file",
			Description: openai.String("Write or overwrite a task file on disk. Call once per file. Use this to save task.js, task.yaml, and task.test.js."),
			Parameters: openai.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"filename": map[string]any{
						"type":        "string",
						"enum":        []string{"task.js", "task.yaml", "task.test.js"},
						"description": "Which file to write",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Complete file contents — never truncated",
					},
				},
				"required": []string{"filename", "content"},
			},
		},
	}}
}

// buildAISystem constructs the system prompt: skill.md reference + current file state.
func buildAISystem(taskID, taskYAML, taskJS, testJS string) string {
	var sb strings.Builder
	sb.WriteString(agent.Skill)
	sb.WriteString("\n\n---\n\n## Current task files\n\n")
	sb.WriteString("**Task ID:** `" + taskID + "`\n\n")
	if taskYAML != "" {
		sb.WriteString("**task.yaml:**\n```yaml\n" + taskYAML + "\n```\n\n")
	}
	if taskJS != "" {
		sb.WriteString("**task.js:**\n```javascript\n" + taskJS + "\n```\n\n")
	}
	if testJS != "" {
		sb.WriteString("**task.test.js:**\n```javascript\n" + testJS + "\n```\n\n")
	}
	sb.WriteString("Use `write_file` for every file you create or modify. Always write the complete file contents.\n")
	return sb.String()
}
