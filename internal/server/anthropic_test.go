package server

// Anthropic Messages 入站适配测试。
// 端到端用例沿用 newFakeUpstream 模式：假上游回固定 SSE，经真 handler
// （含 chatCore 治理管线）打进来，断言出口的 Anthropic 事件序列/JSON 形态。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuechuya/WorkBuddy2API-Gateway/internal/auth"
	"github.com/yuechuya/WorkBuddy2API-Gateway/internal/upstream"
)

// capturingUpstream 记录出站请求体的假上游（断言协议转换结果用）。
func capturingUpstream(t *testing.T, sse string, capture *string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if capture != nil {
				b, _ := io.ReadAll(r.Body)
				*capture = string(b)
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func anthropicTestHandler(up *upstream.Client) *Handler {
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

// ── 请求转换（纯函数）───────────────────────────────────────────────

func TestAnthropicToOpenAIBasic(t *testing.T) {
	body := []byte(`{
      "model":"claude-sonnet-4-5-20250929",
      "max_tokens":1024,
      "system":[{"type":"text","text":"you are a helper"},{"type":"text","text":"be brief"}],
      "messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":[{"type":"text","text":"again"}]}],
      "tools":[{"name":"get_time","description":"get time","input_schema":{"type":"object","properties":{"tz":{"type":"string"}}}}],
      "tool_choice":{"type":"auto"},
      "temperature":0.5,
      "stop_sequences":["END"],
      "metadata":{"user_id":"u-42"}
    }`)
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	out, err := anthropicToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"].(float64) != 1024 || got["temperature"].(float64) != 0.5 {
		t.Errorf("scalar fields = %v", got)
	}
	if got["user"] != "u-42" {
		t.Errorf("user = %v", got["user"])
	}
	stop, _ := got["stop"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v", got["stop"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4 (system+3): %s", len(msgs), out)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "you are a helperbe brief" {
		t.Errorf("system = %v", sys)
	}
	last, _ := msgs[3].(map[string]any)
	if last["role"] != "user" || last["content"] != "again" {
		t.Errorf("last msg = %v", last)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", got["tools"])
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_time" {
		t.Errorf("tool = %v", tools[0])
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("parameters missing: %v", fn)
	}
	if got["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", got["tool_choice"])
	}
}

func TestAnthropicToOpenAIToolUseAndResult(t *testing.T) {
	body := []byte(`{
      "model":"claude-x","messages":[
        {"role":"user","content":"run ls"},
        {"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"tu_1","name":"exec","input":{"cmd":"ls"}}]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"a.txt"},{"type":"text","text":"thanks"}]}
      ]}`)
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	out, err := anthropicToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4: %s", len(msgs), out)
	}
	// assistant：文本作 content、tool_use 作 tool_calls（合并不拆两条）。
	asst, _ := msgs[1].(map[string]any)
	if asst["role"] != "assistant" || asst["content"] != "ok" {
		t.Errorf("assistant = %v", asst)
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %v", asst)
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "tu_1" {
		t.Errorf("tool_call id = %v", tc)
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "exec" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("function = %v", fn)
	}
	// tool_result → 独立 role:tool 消息，紧挨 assistant 之后。
	tool, _ := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "tu_1" || tool["content"] != "a.txt" {
		t.Errorf("tool msg = %v", tool)
	}
	// 同条 user 里的文本退到 tool 消息之后。
	last, _ := msgs[3].(map[string]any)
	if last["role"] != "user" || last["content"] != "thanks" {
		t.Errorf("tail msg = %v", last)
	}
}

func TestAnthropicToOpenAIImage(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[
      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}},
      {"type":"text","text":"what is this"}]}]}`)
	var req anthropicRequest
	_ = json.Unmarshal(body, &req)
	out, err := anthropicToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "data:image/png;base64,AAA") {
		t.Errorf("image not converted: %s", out)
	}
	if !strings.Contains(string(out), "what is this") {
		t.Errorf("text lost: %s", out)
	}
}

func TestAnthropicToOpenAIRejectsEmpty(t *testing.T) {
	var req anthropicRequest
	_ = json.Unmarshal([]byte(`{"model":"m","messages":[]}`), &req)
	if _, err := anthropicToOpenAI(&req); err == nil {
		t.Error("empty messages should fail")
	}
}

// ── 端到端：流式 ────────────────────────────────────────────────────

func TestAnthropicMessagesStreamEndToEnd(t *testing.T) {
	var captured string
	up := capturingUpstream(t, sseOK, &captured)
	h := anthropicTestHandler(up)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// 出站模型名必须已被别名映射（claude-sonnet-4-5 → glm-5.2）。
	if !strings.Contains(captured, `"model":"glm-5.2"`) {
		t.Errorf("outbound model not aliased: %s", captured)
	}
	// Anthropic 事件序列完整性。
	for _, want := range []string{
		"event: message_start",
		`"type":"message_start"`,
		"event: content_block_start",
		"event: content_block_delta",
		`"type":"text_delta"`,
		`"text":"你好"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("OpenAI [DONE] leaked into Anthropic stream:\n%s", body)
	}
	// message_start 的 usage.input_tokens 来自上游 usage（prompt_tokens=1）。
	if !strings.Contains(body, `"input_tokens":1`) {
		t.Errorf("input_tokens missing:\n%s", body)
	}
}

func TestAnthropicMessagesStreamToolUse(t *testing.T) {
	const sseTools = `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec","arguments":""}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\""}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"ls\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up := capturingUpstream(t, sseTools, nil)
	h := anthropicTestHandler(up)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-opus-4","stream":true,"messages":[{"role":"user","content":"ls"}],"tools":[{"name":"exec","input_schema":{"type":"object"}}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`"type":"tool_use"`,
		`"name":"exec"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"cmd\""`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestAnthropicMessagesStreamUpstreamErrorFrame(t *testing.T) {
	const sseErr = `data: {"error":{"message":"rate limited","code":"rate_limit_exceeded"}}` + "\n\n" + "data: [DONE]\n\n"
	up := capturingUpstream(t, sseErr, nil)
	h := anthropicTestHandler(up)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"type":"rate_limit_error"`) {
		t.Errorf("error event missing:\n%s", body)
	}
}

// ── 端到端：非流式 ──────────────────────────────────────────────────

func TestAnthropicMessagesNonStreamEndToEnd(t *testing.T) {
	const sseSync = `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"你好"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n" +
		"data: [DONE]\n\n"
	up := capturingUpstream(t, sseSync, nil)
	h := anthropicTestHandler(up)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-haiku-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q", ct)
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("envelope = %v", msg)
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v", msg["content"])
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "你好" {
		t.Errorf("block = %v", block)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", msg["stop_reason"])
	}
	usage, _ := msg["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 7 || usage["output_tokens"].(float64) != 3 {
		t.Errorf("usage = %v", usage)
	}
}

func TestAnthropicMessagesErrorPath(t *testing.T) {
	// 全池不可用 → chatCore 末端 503（OpenAI 错误信封）→ 适配层翻成 Anthropic 信封。
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: capturingUpstream(t, sseOK, nil)})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-x","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
	if env["type"] != "error" {
		t.Errorf("envelope = %v", env)
	}
	if _, ok := env["error"].(map[string]any); !ok {
		t.Errorf("error obj = %v", env)
	}
}

func TestAnthropicCountTokensEndpoint(t *testing.T) {
	h := anthropicTestHandler(capturingUpstream(t, sseOK, nil))
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-x","messages":[{"role":"user","content":"你好世界 hello world"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	n, ok := out["input_tokens"].(float64)
	if !ok || n <= 0 {
		t.Errorf("input_tokens = %v", out["input_tokens"])
	}
}

func TestAnthropicMessagesAuthRequired(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: capturingUpstream(t, sseOK, nil),
		APIKey:   "secret-key",
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

// ── 模型别名 ────────────────────────────────────────────────────────

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*haiku*", "claude-haiku-4-5", true},
		{"*haiku*", "claude-sonnet-4-5", false},
		{"*codex*", "gpt-5-codex", true},
		{"glm-*", "glm-5.2", true},
		{"glm-*", "deepseek-v4", false},
		{"glm-?", "glm-5", true},
		{"glm-?", "glm-52", false},
		{"*", "", true},
		{"", "", true},
		{"", "x", false},
		{"GLM-*", "glm-5.2", true}, // 大小写不敏感
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pattern, c.s); got != c.want {
			t.Errorf("wildcardMatch(%q,%q)=%v want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestResolveInboundModelDefaults(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith()})
	cases := map[string]string{
		"claude-sonnet-4-5-20250929": "glm-5.2",
		"claude-haiku-4-5":           "glm-5.3-flash",
		"claude-opus-4-1":            "deepseek-v4-pro",
		"gpt-5-codex":                "kimi-k2.7",
		"totally-unknown-model":      "glm-5.3-flash", // default_model 兜底
		"":                           "glm-5.3-flash",
	}
	for in, want := range cases {
		if got := h.resolveInboundModel(in); got != want {
			t.Errorf("resolveInboundModel(%q)=%q want %q", in, got, want)
		}
	}
}

func TestResolveInboundModelUserConfigWins(t *testing.T) {
	h := NewHandler(Config{
		Pool: testPoolWith(),
		Inbound: &InboundConfig{
			DefaultModel: "deepseek-v4.1-flash",
			Rules:        []ModelAliasRule{{Pattern: "*sonnet*", Model: "kimi-k3-1"}},
		},
	})
	if got := h.resolveInboundModel("claude-sonnet-4-5"); got != "kimi-k3-1" {
		t.Errorf("user rule not honored: %q", got)
	}
	// 用户没配的规则类型回落到内置默认表。
	if got := h.resolveInboundModel("claude-haiku-4-5"); got != "glm-5.3-flash" {
		t.Errorf("builtin fallback rule lost: %q", got)
	}
	// 用户 default_model 覆盖内置默认。
	if got := h.resolveInboundModel("unknown"); got != "deepseek-v4.1-flash" {
		t.Errorf("user default not honored: %q", got)
	}
}

func TestResolveInboundModelCatalogPassthrough(t *testing.T) {
	// 目录缓存里有的模型名原样透传（OpenAI 客户端直呼上游模型名）。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.9-turbo"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.Unlock()
	}()
	h := NewHandler(Config{Pool: testPoolWith()})
	if got := h.resolveInboundModel("glm-5.9-turbo"); got != "glm-5.9-turbo" {
		t.Errorf("catalog passthrough broken: %q", got)
	}
}
