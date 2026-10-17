package server

// OpenAI Responses 入站适配测试。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── 请求转换（纯函数）───────────────────────────────────────────────

func TestResponsesToOpenAIBasic(t *testing.T) {
	body := []byte(`{
      "model":"gpt-5-codex",
      "instructions":"you are codex",
      "input":"write hello world",
      "tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object"}}],
      "tool_choice":"auto",
      "max_output_tokens":2048,
      "reasoning":{"effort":"high"}
    }`)
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	out, err := responsesToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["max_tokens"].(float64) != 2048 {
		t.Errorf("max_tokens = %v", got["max_tokens"])
	}
	if got["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", got["reasoning_effort"])
	}
	if got["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", got["tool_choice"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %s", out)
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "user" {
		t.Errorf("roles wrong: %s", out)
	}
}

func TestResponsesToOpenAIFunctionCallChain(t *testing.T) {
	// Codex 的典型 input：user → assistant(function_call) → function_call_output → user。
	body := []byte(`{"model":"gpt-5-codex","input":[
      {"type":"message","role":"user","content":[{"type":"input_text","text":"ls"}]},
      {"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
      {"type":"function_call_output","call_id":"call_1","output":"a.txt"},
      {"type":"message","role":"user","content":[{"type":"input_text","text":"thanks"}]}
    ]}`)
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	out, err := responsesToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d: %s", len(msgs), out)
	}
	asst, _ := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("asst role = %v", asst)
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %v", asst)
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("call id = %v", tc["id"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "shell" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("function = %v", fn)
	}
	tool, _ := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "a.txt" {
		t.Errorf("tool msg = %v", tool)
	}
}

func TestResponsesToOpenAITextFormatHint(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"}}}}`)
	var req responsesRequest
	_ = json.Unmarshal(body, &req)
	out, _ := responsesToOpenAI(&req)
	if !strings.Contains(string(out), "JSON Schema") {
		t.Errorf("json_schema hint missing: %s", out)
	}
}

func TestResponsesToOpenAIRejectsEmptyInput(t *testing.T) {
	var req responsesRequest
	_ = json.Unmarshal([]byte(`{"model":"m"}`), &req)
	if _, err := responsesToOpenAI(&req); err == nil {
		t.Error("empty input should fail")
	}
}

// ── 端到端：流式 ────────────────────────────────────────────────────

func TestResponsesStreamEndToEnd(t *testing.T) {
	var captured string
	up := capturingUpstream(t, sseOK, &captured)
	h := anthropicTestHandler(up)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"gpt-5-codex","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 模型名映射：gpt-5-codex → kimi-k2.7（内置 codex 规则）。
	if !strings.Contains(captured, `"model":"kimi-k2.7"`) {
		t.Errorf("outbound model not aliased: %s", captured)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		`"delta":"你好"`,
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"status":"completed"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// sequence_number 递增存在。
	if !strings.Contains(body, `"sequence_number":0`) {
		t.Errorf("sequence_number missing:\n%s", body)
	}
	// usage 进 completed。
	if !strings.Contains(body, `"input_tokens":1`) {
		t.Errorf("usage missing in completed:\n%s", body)
	}
}

func TestResponsesStreamToolCall(t *testing.T) {
	const sseTools = `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":""}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up := capturingUpstream(t, sseTools, nil)
	h := anthropicTestHandler(up)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"gpt-5-codex","stream":true,"input":"ls","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`"type":"function_call"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"arguments":"{\"cmd\":\"ls\"}"`,
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestResponsesNonStreamEndToEnd(t *testing.T) {
	const sseSync = `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"done"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":5,"total_tokens":7}}` + "\n\n" +
		"data: [DONE]\n\n"
	up := capturingUpstream(t, sseSync, nil)
	h := anthropicTestHandler(up)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"gpt-5-codex","stream":false,"input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("envelope = %v", resp)
	}
	outputs, _ := resp["output"].([]any)
	if len(outputs) != 1 {
		t.Fatalf("output = %v", resp["output"])
	}
	item, _ := outputs[0].(map[string]any)
	if item["type"] != "message" {
		t.Errorf("item = %v", item)
	}
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "done" {
		t.Errorf("text = %v", part)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 2 || usage["output_tokens"].(float64) != 5 {
		t.Errorf("usage = %v", usage)
	}
}
