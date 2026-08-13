package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode"

	tiktoken "github.com/tiktoken-go/tokenizer"

	"m365-copilot2api/internal/chathub"
)

const (
	usageSourceTiktoken  = "tiktoken_o200k_base_estimate"
	usageSourceHeuristic = "heuristic_character_estimate"

	// These cover visible request framing not represented by literal message text.
	// They are conservative estimates, not ChatHub billing-token claims.
	requestProtocolTokens    = 4
	messageProtocolTokens    = 4
	toolProtocolTokens       = 6
	toolChoiceProtocolTokens = 2
	replyPrimingTokens       = 3
	outputProtocolTokens     = 3
)

var (
	gptTokenizerOnce sync.Once
	gptTokenizer     tiktoken.Codec
	gptTokenizerErr  error
)

func getGPTTokenizer() (tiktoken.Codec, error) {
	gptTokenizerOnce.Do(func() {
		// The vocabulary is embedded in the binary, so this never needs network or cache state.
		gptTokenizer, gptTokenizerErr = tiktoken.Get(tiktoken.O200kBase)
	})
	return gptTokenizer, gptTokenizerErr
}

func countTokens(model, text string) int64 {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(m, "gpt-") {
		if enc, err := getGPTTokenizer(); err == nil {
			if ids, _, err := enc.Encode(text); err == nil {
				return int64(len(ids))
			}
		}
	}
	return int64(heuristicTokenCount(text))
}

func heuristicTokenCount(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		if r <= 0x7f {
			ascii++
		} else {
			other++
		}
	}
	if ascii == 0 && other == 0 {
		return 0
	}
	return (ascii+3)/4 + other
}

type responsesUsageEstimate struct {
	Values map[string]any
	Source string
}

func tokenEstimator(model string) (func(string) int, string) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-") {
		if enc, err := getGPTTokenizer(); err == nil {
			return func(text string) int {
				ids, _, err := enc.Encode(text)
				if err != nil {
					return heuristicTokenCount(text)
				}
				return len(ids)
			}, usageSourceTiktoken
		}
	}
	return heuristicTokenCount, usageSourceHeuristic
}

func serializedTokenCount(v any, count func(string) int) int {
	if s, ok := v.(string); ok {
		return count(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return count(fmt.Sprint(v))
	}
	return count(string(b))
}

// estimateResponsesUsage is a local Codex context estimate, never billing data.
func estimateResponsesUsage(model string, input []oaiMsg, tools []chathub.Tool, toolChoice any, output string) responsesUsageEstimate {
	count, source := tokenEstimator(model)
	in := requestProtocolTokens + replyPrimingTokens
	var cacheTokens int
	upper := len(input)
	if upper > 0 {
		upper = upper - 1
	}
	for i, message := range input {
		in += messageProtocolTokens
		in += count(message.Role)
		in += serializedTokenCount(message.Content, count)
		in += count(message.Name)
		in += count(message.ToolCallID)
		for _, call := range message.ToolCalls {
			in += serializedTokenCount(call, count)
		}
		if i < upper {
			cacheTokens += count(message.Role)
			cacheTokens += serializedTokenCount(message.Content, count)
			cacheTokens += count(message.Name)
			cacheTokens += count(message.ToolCallID)
			for _, call := range message.ToolCalls {
				cacheTokens += serializedTokenCount(call, count)
			}
		}
	}
	for _, tool := range tools {
		in += toolProtocolTokens + serializedTokenCount(tool, count)
	}
	if toolChoice != nil {
		in += toolChoiceProtocolTokens + serializedTokenCount(toolChoice, count)
	}
	out := count(output)
	if output != "" {
		out += outputProtocolTokens
	}
	values := map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out}
	if cacheTokens > 0 {
		values["cache_read_input_tokens"] = cacheTokens
	}
	return responsesUsageEstimate{Values: values, Source: source}
}

func localUsageMetadata(source string) map[string]any {
	return map[string]any{
		"usage_source":               source,
		"usage_values_are_estimates": true,
		"usage_estimate_scope":       "visible_request_and_completion",
		"usage_includes":             []string{"message_content", "message_framing", "tool_schemas", "tool_choice", "tool_calls", "completion_framing"},
	}
}
