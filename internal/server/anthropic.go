package server

// Anthropic Messages 协议入站适配（POST /v1/messages、/v1/messages/count_tokens）。
//
// 为什么在网关里原生支持：Claude Code 等客户端只说 Anthropic Messages 协议。
// 此前要经 CCR 等第三方转换层才能打进来——多一跳转换就多一处丢字段（实测
// tools 声明在协议转换中丢失是常态，直接导致上游把工具调用吐成纯文本），
// 且转换层不认识本网关的账号治理（熔断/粘性/降级对它都是黑盒）。
//
// 设计：协议差异只存在于两段——
//   - 请求段：anthropicToOpenAI 把 Anthropic 请求转换成 OpenAI chat/completions
//     形态（system 数组、content block 数组、tool_use/tool_result、input_schema
//     全是 Anthropic 特有结构，在此消化）；
//   - 响应段：anthropicWriter 把上游回吐的 OpenAI SSE 帧 / 聚合 JSON 翻译成
//     Anthropic 事件流 / message 对象。
// 中段（选号、轮转、熔断、粘性、提示词改写、用量记账）全部走 chatCore，
// 与 /v1/chat/completions 逐字节同一条管线。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ── 请求结构 ────────────────────────────────────────────────────────────

type anthropicRequest struct {
	Model          string             `json:"model"`
	MaxTokens      int                `json:"max_tokens"`
	System         json.RawMessage    `json:"system"` // string | []{type:text,text}
	Messages       []anthropicMessage `json:"messages"`
	Tools          []anthropicTool    `json:"tools"`
	ToolChoice     json.RawMessage    `json:"tool_choice"` // {type:auto|any|tool|none,name?}
	Stream         bool               `json:"stream"`
	Temperature    *float64           `json:"temperature"`
	TopP           *float64           `json:"top_p"`
	TopK           *int               `json:"top_k"` // 上游不识别，转换时丢弃
	StopSequences  []string           `json:"stop_sequences"`
	Metadata       map[string]any     `json:"metadata"`
	Thinking       json.RawMessage    `json:"thinking"` // {type:enabled|disabled,budget_tokens?}
	ServiceTier    string             `json:"service_tier"`
	AnthropicBeta  string             `json:"-"` // 仅观测，不参与转换
}

type anthropicMessage struct {
	Role    string          `json:"role"`    // user | assistant
	Content json.RawMessage `json:"content"` // string | []block
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicBlock content block 的宽松形态（type 判别后按需取字段）。
type anthropicBlock struct {
	Type string `json:"type"`
	// text / thinking
	Text string `json:"text"`
	// image
	Source *anthropicImageSource `json:"source"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // string | []block（tool_result 内）
	IsError   bool            `json:"is_error"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// ── 请求转换：Anthropic → OpenAI chat/completions ──────────────────────

// anthropicToOpenAI 把 Anthropic Messages 请求体转换成 OpenAI chat/completions
// 请求体。模型名由调用方先行映射（resolveInboundModel），此处只做结构转换。
func anthropicToOpenAI(req *anthropicRequest) ([]byte, error) {
	out := map[string]any{"model": req.Model, "stream": req.Stream}
	var messages []map[string]any

	// system：字符串或 block 数组 → 单条 system 消息。
	if sys := flattenAnthropicText(req.System); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}

	for _, m := range req.Messages {
		converted, err := convertAnthropicMessage(m)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages: at least one message is required")
	}
	out["messages"] = messages

	// tools：Anthropic 的平铺结构（name/description/input_schema）→ OpenAI 的
	// function 嵌套结构。
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Name == "" {
				continue
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				var params any
				if err := json.Unmarshal(t.InputSchema, &params); err == nil {
					fn["parameters"] = params
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}

	// tool_choice：auto→auto、any→required、tool→指定函数、none→none。
	if tc := convertAnthropicToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}

	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if req.Metadata != nil {
		if uid, ok := req.Metadata["user_id"].(string); ok && uid != "" {
			out["user"] = uid
		}
	}
	// thinking.enabled → reasoning_effort=high（上游思维链开关；上游不识别
	// Anthropic 的 budget_tokens 粒度，只做有能力/无能力两态映射）。
	if thinkingEnabled(req.Thinking) {
		out["reasoning_effort"] = "high"
	}
	return json.Marshal(out)
}

// convertAnthropicMessage 把一条 Anthropic 消息转成零到多条 OpenAI 消息。
// 展开规则：
//   - text / image block → 累积进当前角色的消息内容；
//   - tool_use（assistant）→  assistant 消息挂 tool_calls（与同消息的文本
//     合并为一条，文本作 content、调用作 tool_calls）；
//   - tool_result（user）→ 独立的 role:tool 消息（OpenAI 工具结果必须是
//     独立消息且紧挨对应的 assistant tool_calls 之后）。
func convertAnthropicMessage(m anthropicMessage) ([]map[string]any, error) {
	blocks := parseAnthropicContent(m.Content)
	if len(blocks) == 0 {
		return nil, fmt.Errorf("messages: empty content")
	}
	role := m.Role
	if role == "" {
		role = "user"
	}
	var out []map[string]any
	var textParts []map[string]any  // OpenAI content parts（多模态时）
	var textBuf strings.Builder     // 纯文本时直接给字符串
	var toolCalls []map[string]any  // 本条 assistant 消息的工具调用

	// flushToolCalls / flushText 互相引用（文本后跟工具调用、工具调用后跟
	// 文本都要保持相对顺序），故以前置 var 声明再赋值，不能直接用 :=。
	var flushToolCalls func()
	flushToolCalls = func() {
		if len(toolCalls) == 0 {
			return
		}
		msg := map[string]any{"role": "assistant", "tool_calls": toolCalls}
		if textBuf.Len() > 0 {
			msg["content"] = textBuf.String()
			textBuf.Reset()
		} else {
			msg["content"] = nil
		}
		textParts = nil
		toolCalls = nil
		out = append(out, msg)
	}
	flushText := func() {
		// 有挂起的工具调用时先把它们落成 assistant 消息（保持「文本→调用」
		// 的相对顺序；同消息的文本已并入调用消息，不会到这一步）。
		if len(toolCalls) > 0 {
			flushToolCalls()
		}
		if textBuf.Len() == 0 && len(textParts) == 0 {
			return
		}
		msg := map[string]any{"role": role}
		switch {
		case len(textParts) > 0:
			// 有图片等非文本 part → 用 parts 数组（纯文本也并入）。
			if textBuf.Len() > 0 {
				textParts = append([]map[string]any{{"type": "text", "text": textBuf.String()}}, textParts...)
				textBuf.Reset()
			}
			msg["content"] = textParts
		default:
			msg["content"] = textBuf.String()
			textBuf.Reset()
		}
		textParts = nil
		out = append(out, msg)
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textBuf.WriteString(b.Text)
			}
		case "image":
			url := anthropicImageURL(b.Source)
			if url != "" {
				textParts = append(textParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case "tool_use":
			// 工具调用与同消息的文本合并为一条 assistant 消息（文本作 content、
			// 调用作 tool_calls）——不提前 flushText，否则会拆成两条 assistant
			// 消息，部分上游对连续 assistant 消息的容忍度很差。
			args := "{}"
			if len(b.Input) > 0 && string(b.Input) != "null" {
				args = string(b.Input)
			}
			id := b.ID
			if id == "" {
				id = "toolu_" + randHex(12)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		case "tool_result":
			// 工具结果必须先于同条的后续文本（OpenAI 要求 tool 消息紧跟
			// 发起调用的 assistant 消息）。
			flushText()
			flushToolCalls()
			content := flattenAnthropicText(b.Content)
			if content == "" && b.IsError {
				content = "tool error (no content)"
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      content,
			})
		case "thinking", "redacted_thinking":
			// 历史思考块不入上下文（与官方客户端回传语义一致：思考不重放）。
		default:
			// 未知 block 类型忽略（向前兼容新 block）。
		}
	}
	flushText()
	flushToolCalls()
	if len(out) == 0 {
		return nil, fmt.Errorf("messages: no convertible content")
	}
	return out, nil
}

// parseAnthropicContent 把 string | []block 的 content 统一成 block 数组。
func parseAnthropicContent(raw json.RawMessage) []anthropicBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// flattenAnthropicText 把 string | []block 的 content 拍平为纯文本
// （system 提示词与 tool_result 内容用）。
func flattenAnthropicText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_result":
			sb.WriteString(flattenAnthropicText(b.Content))
		}
	}
	return sb.String()
}

// anthropicImageURL 把 Anthropic image source 转成 OpenAI image_url。
// base64 → data URL；url → 原样。
func anthropicImageURL(src *anthropicImageSource) string {
	if src == nil {
		return ""
	}
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return ""
		}
		media := src.MediaType
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + src.Data
	case "url":
		return src.URL
	}
	return ""
}

// convertAnthropicToolChoice Anthropic tool_choice → OpenAI tool_choice。
func convertAnthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	switch obj.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if obj.Name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
		}
	}
	return nil
}

// thinkingEnabled 判断 Anthropic thinking 是否开启。
func thinkingEnabled(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return obj.Type == "enabled"
}

// ── 响应翻译：OpenAI SSE / 聚合 JSON → Anthropic 事件流 / message ──────

// anthropicWriter 包装真实 ResponseWriter，把 chatCore 写出的 OpenAI 形态
// 响应翻译成 Anthropic 形态：
//   - 流式：逐 SSE 帧翻译成 Anthropic 事件序列（message_start →
//     content_block_* → message_delta → message_stop）；
//   - 非流式：缓冲聚合 JSON，finish() 时整包翻译成 Anthropic message 对象。
//
// 头信息直接委托真实 writer（StreamHint 设置的 text/event-stream、writeJSON
// 设置的 application/json 对 Anthropic 客户端同样正确），不另造一套。
type anthropicWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string // 客户端请求的原始模型名（回显用）
	stream  bool

	// 流式状态
	msgID      string
	started    bool
	sawDone    bool
	nextIndex  int
	textIdx    int // 文本 block 的 anthropic index（-1 = 未开）
	thinkIdx   int // 思考 block 的 anthropic index（-1 = 未开）
	toolIdx    map[int]int // openai tool_calls index → anthropic block index
	stopReason string
	inTok      int
	outTok     int

	// 帧切分缓冲（SSE 帧以空行分隔，跨 Write 到达要拼接）
	frameBuf strings.Builder

	// 非流式缓冲
	buf      []byte
	finished bool
}

func newAnthropicWriter(w http.ResponseWriter, model string, stream bool) *anthropicWriter {
	fl, _ := w.(http.Flusher)
	return &anthropicWriter{
		w: w, flusher: fl, model: model, stream: stream,
		textIdx: -1, thinkIdx: -1, toolIdx: map[int]int{},
	}
}

func (a *anthropicWriter) Header() http.Header { return a.w.Header() }

func (a *anthropicWriter) WriteHeader(status int) { a.w.WriteHeader(status) }

// Flush 实现 http.Flusher：StreamHint 按帧 flush 依赖该接口（不实现则
// 整条流攒到结束才吐，流式客户端看到的是"假流式"）。翻译帧在 emit 里
// 已逐条 flush，这里只做透传。
func (a *anthropicWriter) Flush() {
	if a.flusher != nil {
		a.flusher.Flush()
	}
}

func (a *anthropicWriter) Write(p []byte) (int, error) {
	if a.stream {
		a.frameBuf.Write(p)
		a.drainFrames()
		return len(p), nil
	}
	a.buf = append(a.buf, p...)
	return len(p), nil
}

// drainFrames 从帧缓冲切出完整 SSE 帧（空行分隔）逐个翻译。
func (a *anthropicWriter) drainFrames() {
	buf := a.frameBuf.String()
	for {
		idx := strings.Index(buf, "\n\n")
		if idx < 0 {
			break
		}
		frame := buf[:idx]
		buf = buf[idx+2:]
		a.feedFrame(frame)
	}
	a.frameBuf.Reset()
	a.frameBuf.WriteString(buf)
}

// feedFrame 翻译一帧 OpenAI SSE（已去掉空行，可能含 "data: " 前缀）。
func (a *anthropicWriter) feedFrame(frame string) {
	frame = strings.TrimRight(frame, "\r")
	if frame == "" {
		return
	}
	payload := frame
	if strings.HasPrefix(frame, "data:") {
		payload = strings.TrimSpace(frame[len("data:"):])
	}
	if payload == "" {
		return
	}
	if payload == "[DONE]" {
		a.finishStream()
		return
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return // 非 JSON 帧（注释/keep-alive）忽略
	}
	if errObj, ok := chunk["error"].(map[string]any); ok {
		a.emitErrorFrame(errObj)
		a.finishStream()
		return
	}
	a.ensureStarted(chunk)
	if choices, ok := chunk["choices"].([]any); ok {
		for _, ci := range choices {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if d, ok := c["delta"].(map[string]any); ok {
				a.feedDelta(d)
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				a.stopReason = mapAnthropicStopReason(fr)
			}
		}
	}
	if u, ok := chunk["usage"].(map[string]any); ok {
		a.inTok = int(jsonFloat(u["prompt_tokens"]))
		a.outTok = int(jsonFloat(u["completion_tokens"]))
	}
}

// ensureStarted 首帧到达时发 message_start 骨架（幂等）。
func (a *anthropicWriter) ensureStarted(chunk map[string]any) {
	if a.started {
		return
	}
	a.started = true
	if a.msgID == "" {
		a.msgID = "msg_" + randHex(16)
	}
	inTok := 0
	if chunk != nil {
		if u, ok := chunk["usage"].(map[string]any); ok {
			inTok = int(jsonFloat(u["prompt_tokens"]))
		}
	}
	a.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            a.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         a.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": inTok, "output_tokens": 0},
		},
	})
}

// feedDelta 处理一个 OpenAI delta：文本 / 思考 / 工具调用三类。
func (a *anthropicWriter) feedDelta(d map[string]any) {
	if rc, ok := d["reasoning_content"].(string); ok && rc != "" {
		a.openThinking()
		a.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": a.thinkIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
		})
	}
	if content, ok := d["content"].(string); ok && content != "" {
		a.openText()
		a.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": a.textIdx,
			"delta": map[string]any{"type": "text_delta", "text": content},
		})
	}
	if tcs, ok := d["tool_calls"].([]any); ok {
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			oaIdx := int(jsonFloat(tc["index"]))
			blockIdx, open := a.toolBlock(oaIdx)
			if !open {
				id, _ := tc["id"].(string)
				if id == "" {
					id = "toolu_" + randHex(12)
				}
				name := ""
				if fn, ok := tc["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				blockIdx = a.openBlock()
				a.toolIdx[oaIdx] = blockIdx
				a.emit("content_block_start", map[string]any{
					"type": "content_block_start", "index": blockIdx,
					"content_block": map[string]any{
						"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
					},
				})
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				if args, ok := fn["arguments"].(string); ok && args != "" {
					a.emit("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": blockIdx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
					})
				}
			}
		}
	}
}

// openText / openThinking / openBlock：Anthropic 的 content block 必须按
// index 顺序逐个开合，新 block 开启前先关掉已开的（与官方流行为一致）。
func (a *anthropicWriter) openText() {
	if a.textIdx >= 0 {
		return
	}
	a.closeOpenBlocks()
	a.textIdx = a.openBlock()
	a.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": a.textIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (a *anthropicWriter) openThinking() {
	if a.thinkIdx >= 0 {
		return
	}
	a.closeOpenBlocks()
	a.thinkIdx = a.openBlock()
	a.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": a.thinkIdx,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
}

// openBlock 分配下一个 anthropic block index。
func (a *anthropicWriter) openBlock() int {
	idx := a.nextIndex
	a.nextIndex++
	return idx
}

// toolBlock 返回 openai tool index 对应的 anthropic block index 及是否已开。
func (a *anthropicWriter) toolBlock(oaIdx int) (int, bool) {
	idx, ok := a.toolIdx[oaIdx]
	return idx, ok
}

// closeOpenBlocks 关闭所有已开 block（文本/思考/工具），保持开合有序。
// 工具 block 按 index 升序关（严格客户端按 index 顺序校验开合序列）。
func (a *anthropicWriter) closeOpenBlocks() {
	if a.textIdx >= 0 {
		a.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": a.textIdx})
		a.textIdx = -1
	}
	if a.thinkIdx >= 0 {
		a.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": a.thinkIdx})
		a.thinkIdx = -1
	}
	if len(a.toolIdx) > 0 {
		idxs := make([]int, 0, len(a.toolIdx))
		for _, idx := range a.toolIdx {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		for _, idx := range idxs {
			a.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		}
		a.toolIdx = map[int]int{}
	}
}

// emitErrorFrame 上游 error 帧 → Anthropic error 事件。
func (a *anthropicWriter) emitErrorFrame(errObj map[string]any) {
	a.ensureStarted(nil)
	msg, _ := errObj["message"].(string)
	if msg == "" {
		msg = "upstream error"
	}
	typ := "api_error"
	if code, _ := errObj["code"].(string); code == "rate_limit_exceeded" {
		typ = "rate_limit_error"
	}
	a.emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// finishStream 流式收尾：关 block → message_delta（stop_reason + usage）→
// message_stop。幂等（[DONE] 与 error 帧都可能触发）。
func (a *anthropicWriter) finishStream() {
	if a.sawDone {
		return
	}
	a.sawDone = true
	a.ensureStarted(nil)
	a.closeOpenBlocks()
	if a.stopReason == "" {
		a.stopReason = "end_turn"
	}
	a.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": a.stopReason, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": a.inTok, "output_tokens": a.outTok},
	})
	a.emit("message_stop", map[string]any{"type": "message_stop"})
	if a.flusher != nil {
		a.flusher.Flush()
	}
}

// finish 非流式收尾：把缓冲的 OpenAI 聚合响应翻译成 Anthropic message。
// 流式路径无需调用（帧级已收尾）。
func (a *anthropicWriter) finish() {
	if a.stream || a.finished {
		return
	}
	a.finished = true
	if len(a.buf) == 0 {
		return
	}
	var resp map[string]any
	if err := json.Unmarshal(a.buf, &resp); err != nil {
		// 非 JSON（理论上来不了）：原样透传，不吞字节。
		_, _ = a.w.Write(a.buf)
		return
	}
	if errObj, ok := resp["error"].(map[string]any); ok {
		// 网关错误透出（writeOpenAIError 的 OpenAI 信封 → Anthropic 信封）。
		typ, _ := errObj["type"].(string)
		if typ == "" {
			typ = "api_error"
		}
		msg, _ := errObj["message"].(string)
		_ = json.NewEncoder(a.w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrorType(typ, errObj), "message": msg},
		})
		return
	}
	msg := anthropicMessageFromOpenAI(resp, a.model)
	_ = json.NewEncoder(a.w).Encode(msg)
}

// anthropicMessageFromOpenAI 聚合响应 → Anthropic message 对象。
func anthropicMessageFromOpenAI(resp map[string]any, model string) map[string]any {
	id, _ := resp["id"].(string)
	if id == "" {
		id = "chatcmpl-" + randHex(8)
	}
	var content []map[string]any
	stopReason := "end_turn"
	finish := "stop"
	inTok, outTok := 0, 0
	if u, ok := resp["usage"].(map[string]any); ok {
		inTok = int(jsonFloat(u["prompt_tokens"]))
		outTok = int(jsonFloat(u["completion_tokens"]))
	}
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finish = fr
			}
			if m, ok := c["message"].(map[string]any); ok {
				if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
					content = append(content, map[string]any{"type": "thinking", "thinking": rc})
				}
				if text, ok := m["content"].(string); ok && text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
				if tcs, ok := m["tool_calls"].([]any); ok {
					for _, tci := range tcs {
						tc, _ := tci.(map[string]any)
						if tc == nil {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						name := ""
						args := "{}"
						if fn != nil {
							name, _ = fn["name"].(string)
							if s, ok := fn["arguments"].(string); ok && s != "" {
								args = s
							}
						}
						var input any = map[string]any{}
						_ = json.Unmarshal([]byte(args), &input)
						content = append(content, map[string]any{
							"type": "tool_use", "id": tc["id"], "name": name, "input": input,
						})
					}
				}
			}
		}
	}
	stopReason = mapAnthropicStopReason(finish)
	if model == "" {
		model, _ = resp["model"].(string)
	}
	return map[string]any{
		"id":            "msg_" + randHex(16),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": inTok, "output_tokens": outTok},
	}
}

// mapAnthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func mapAnthropicStopReason(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// anthropicErrorType 网关错误类型 → Anthropic 错误类型。
func anthropicErrorType(typ string, errObj map[string]any) string {
	code, _ := errObj["code"].(string)
	if code == "rate_limit_exceeded" || typ == "rate_limit" {
		return "rate_limit_error"
	}
	switch typ {
	case "invalid_request_error", "invalid_api_key":
		return "invalid_request_error"
	case "authentication_error":
		return "authentication_error"
	case "permission_error":
		return "permission_error"
	case "not_found_error":
		return "not_found_error"
	case "overloaded_error":
		return "overloaded_error"
	}
	return "api_error"
}

// ── Handler ────────────────────────────────────────────────────────────

func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages: field required")
		return
	}
	// 模型名映射（外来名 → 上游名）在结构转换之前做。
	inboundModel := req.Model
	req.Model = h.resolveInboundModel(req.Model)
	if req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model: field required")
		return
	}
	// 原始模型名进请求跟踪：面板「请求记录」据此展示「原始名 → 解析名」，
	// 否则 Anthropic 入站的请求在日志里只剩解析后的上游名，看不出客户端
	// 问的是哪个模型（别名映射是本仓库二开核心，日志必须可观测）。
	if tr := requestTraceFrom(r); tr != nil && inboundModel != req.Model {
		tr.inboundModel = inboundModel
	}
	openaiBody, err := anthropicToOpenAI(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	aw := newAnthropicWriter(w, req.Model, req.Stream)
	h.chatCore(aw, r, openaiBody)
	aw.finish()
}

// anthropicCountTokens Anthropic 的 token 计数端点（/v1/messages/count_tokens）。
// Claude Code 在上下文预警里会调用它。上游没有对应的计数接口，这里给出
// 本地估算（CJK 按 1 字 ≈ 1 token、其余按 4 字符 ≈ 1 token，另加每条消息
// 与工具定义的固定开销）——估算值只影响客户端的剩余上下文提示，不影响请求
// 本身，偏差不致命；不实现该端点 Claude Code 会直接报错。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicRequest
	_ = json.Unmarshal(raw, &req) // 解析失败也给估算（不阻塞客户端）
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateAnthropicTokens(&req)})
}

// estimateAnthropicTokens 粗估请求的 input token 数。
func estimateAnthropicTokens(req *anthropicRequest) int {
	total := 0
	total += estimateTextTokens(flattenAnthropicText(req.System))
	for _, m := range req.Messages {
		total += 3 // 每条消息的角色/边界开销
		total += estimateTextTokens(flattenAnthropicText(m.Content))
	}
	for _, t := range req.Tools {
		total += estimateTextTokens(t.Name + " " + t.Description)
		total += len(t.InputSchema) / 4
		total += 8
	}
	return total
}

// estimateTextTokens 混合中英文本的 token 估算：CJK 字符 1 个 ≈ 1 token，
// 其余每 4 字符 ≈ 1 token。
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk := 0
	other := 0
	for _, r := range s {
		if r >= 0x2E80 && r <= 0x9FFF || r >= 0xF900 && r <= 0xFAFF ||
			r >= 0xFF00 && r <= 0xFF6F || r >= 0x3000 && r <= 0x303F {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// ── 工具 ────────────────────────────────────────────────────────────────

// emit 写出一帧 Anthropic SSE 事件（event + data 双行，空行收尾）。
func (a *anthropicWriter) emit(event string, payload map[string]any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var sb strings.Builder
	sb.WriteString("event: ")
	sb.WriteString(event)
	sb.WriteString("\ndata: ")
	sb.Write(data)
	sb.WriteString("\n\n")
	if _, err := io.WriteString(a.w, sb.String()); err != nil {
		return
	}
	if a.flusher != nil {
		a.flusher.Flush()
	}
}

// writeAnthropicError 以 Anthropic 错误信封写出错误响应。
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// jsonFloat 从 any 取 float64（JSON 数字统一解码为 float64）。
func jsonFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// randHex 生成 n 字节的随机十六进制串（事件/消息 ID 用）。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败（理论极限）→ 时间戳兜底，保证 ID 唯一可辨。
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
