package web

import (
	"m365-copilot2api/internal/chathub"
	"net/http"
	"time"
)

// openAIUsage 用与 WebUI 相同的 EstimateTokens 估算构造 OpenAI 兼容的
// usage 字段。上游 ChatHub 不返回真实 token 计数，故此处仅为估算值。
// cached 为可选的缓存 token 估算（历史消息部分，对应 WebUI CacheTokens），
// 填入 prompt_tokens_details.cached_tokens，并计入 total_tokens。
func openAIUsage(model, prompt, completion string, cached ...int64) map[string]any {
	pt := countTokens(model, prompt)
	ct := countTokens(model, completion)
	var cachedTokens int64
	if len(cached) > 0 {
		cachedTokens = cached[0]
	}
	var details map[string]any
	if cachedTokens > 0 {
		details = map[string]any{"cached_tokens": cachedTokens}
	}
	return map[string]any{
		"prompt_tokens":             pt,
		"completion_tokens":         ct,
		"total_tokens":              pt + ct,
		"prompt_tokens_details":     details,
		"completion_tokens_details": nil,
	}
}

func writeToolResponse(w http.ResponseWriter, id, model, prompt string, stream bool, calls []detectedToolCall, res chathub.Result, cached ...int64) error {
	toolCalls := toolCallMaps(calls)
	msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls}
	if res.Reasoning != "" {
		msg["reasoning_content"] = res.Reasoning
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)
		emit := func(v any) {
			if err := sseDataRaw(w, flusher, mustJSON(v)); err != nil {
				return
			}
		}
		base := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		firstDelta := map[string]any{"role": "assistant", "content": nil}
		if res.Reasoning != "" {
			firstDelta["reasoning_content"] = res.Reasoning
		}
		emit(base(firstDelta, nil))
		for i, tc := range calls {
			typ := tc.Type
			if typ == "" {
				typ = "function"
			}
			emit(base(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tc.ID, "type": typ, "function": map[string]any{"name": tc.Name, "arguments": string(tc.Arguments)}}}}, nil))
		}
		emit(base(map[string]any{}, "tool_calls"))
		emit(map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{}, "usage": openAIUsage(model, prompt, res.Text, cached...)})
		_ = sseSafeRaw(w, flusher, "data: [DONE]\n\n")
		return nil
	}
	jsonOut(w, map[string]any{"id": id, "object": "chat.completion", "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "tool_calls"}}, "m365": compatM365Metadata(res), "usage": openAIUsage(model, prompt, res.Text, cached...)})
	return nil
}
