package server

// OpenAI Responses 协议入站适配（POST /v1/responses）。
//
// 为什么需要：Codex CLI 等客户端只说 Responses 协议（input item 数组 +
// 平铺 tools + function_call/function_call_output item），与 chat/completions
// 的消息结构差异大。此前同样要靠第三方转换层。适配策略与 Anthropic 一致：
// 请求段结构转换（responsesToOpenAI），响应段事件翻译（responsesWriter），
// 中段治理走 chatCore。
//
// 事件序列（流式）对齐 OpenAI 官方形态：
//   response.created → response.in_progress →
//   [response.output_item.added → response.content_part.added →
//    response.output_text.delta* → response.output_text.done →
//    response.content_part.done → response.output_item.done]* →
//   response.completed
// 工具调用item：output_item.added(function_call) →
//   function_call_arguments.delta* → function_call_arguments.done →
//   output_item.done。
// 每个事件带递增 sequence_number（官方字段，部分客户端校验）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ── 请求结构 ────────────────────────────────────────────────────────────

type responsesRequest struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions"`
	Input             json.RawMessage `json:"input"` // string | []item
	Tools             []responsesTool `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"` // "auto"|"none"|"required"|{type:function,name}
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	MaxOutputTokens   int             `json:"max_output_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stream            bool            `json:"stream"`
	Reasoning         json.RawMessage `json:"reasoning"` // {effort?,summary?}
	Text              json.RawMessage `json:"text"`      // {format:{type:text|json_object|json_schema,...}}
	Metadata          map[string]any  `json:"metadata"`
	PreviousResponseID string         `json:"previous_response_id"`
	Store             bool            `json:"store"`
}

type responsesTool struct {
	Type        string          `json:"type"` // function | web_search | ...
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// responsesItem input 数组里的一个 item（宽松形态，type 判别）。
type responsesItem struct {
	Type    string          `json:"type"` // message|function_call|function_call_output|reasoning|item_reference
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | []part（message）
	// function_call
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
	ID        string `json:"id"`
	// function_call_output
	Output json.RawMessage `json:"output"` // string | []part
}

// ── 请求转换：Responses → OpenAI chat/completions ──────────────────────

// responsesToOpenAI 把 Responses 请求体转换成 OpenAI chat/completions 请求体。
func responsesToOpenAI(req *responsesRequest) ([]byte, error) {
	out := map[string]any{"model": req.Model, "stream": req.Stream}
	var messages []map[string]any

	if req.Instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.Instructions})
	}
	// text.format（结构化输出约束）→ 追加到 system 指令。上游没有 json_schema
	// 强制能力，用指令方式尽最大努力（Codex 的最终答案格式化依赖它）。
	if hint := responsesTextFormatHint(req.Text); hint != "" {
		if len(messages) > 0 {
			if s, ok := messages[0]["content"].(string); ok {
				messages[0]["content"] = s + "\n\n" + hint
			}
		} else {
			messages = append(messages, map[string]any{"role": "system", "content": hint})
		}
	}

	// input：字符串 → 单条 user 消息；数组 → 逐 item 转换。
	var s string
	if err := json.Unmarshal(req.Input, &s); err == nil {
		if s != "" {
			messages = append(messages, map[string]any{"role": "user", "content": s})
		}
	} else {
		var items []responsesItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, err
		}
		converted, err := convertResponsesItems(items)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("input: at least one message is required")
	}
	out["messages"] = messages

	// tools：只转换 function 型（其余类型上游没有对应能力，跳过）。
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "function" || t.Name == "" {
				continue
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				var params any
				if err := json.Unmarshal(t.Parameters, &params); err == nil {
					fn["parameters"] = params
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if tc := convertResponsesToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.Metadata != nil {
		if uid, ok := req.Metadata["user_id"].(string); ok && uid != "" {
			out["user"] = uid
		}
	}
	// reasoning.effort → reasoning_effort（档位名一致，直接透传语义）。
	if effort := responsesEffort(req.Reasoning); effort != "" {
		out["reasoning_effort"] = effort
	}
	return json.Marshal(out)
}

// convertResponsesItems 逐 item 转换 input 数组。
// 连续性处理：相邻的 function_call item 合并进同一条 assistant 消息
// （OpenAI 的 tool_calls 是消息级数组，平铺发送时逐条开会破坏配对语义）。
func convertResponsesItems(items []responsesItem) ([]map[string]any, error) {
	var out []map[string]any
	var pendingCalls []map[string]any // 待合并的连续 function_call

	flushCalls := func() {
		if len(pendingCalls) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "assistant", "content": nil, "tool_calls": pendingCalls})
		pendingCalls = nil
	}

	for _, it := range items {
		switch it.Type {
		case "message", "":
			flushCalls()
			msg, err := convertResponsesMessage(it)
			if err != nil {
				return nil, err
			}
			if msg != nil {
				out = append(out, msg)
			}
		case "function_call":
			callID := it.CallID
			if callID == "" {
				callID = it.ID
			}
			args := it.Arguments
			if args == "" {
				args = "{}"
			}
			pendingCalls = append(pendingCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      it.Name,
					"arguments": args,
				},
			})
		case "function_call_output":
			flushCalls()
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": it.CallID,
				"content":      flattenResponsesText(it.Output),
			})
		case "reasoning", "item_reference":
			// 思考历史 / 引用 item 不进入上下文（与官方行为一致）。
		default:
			// 未知 item 类型忽略（向前兼容）。
		}
	}
	flushCalls()
	return out, nil
}

// convertResponsesMessage message item → OpenAI 消息。
// content parts：input_text/output_text/refusal → 文本；input_image → image_url。
func convertResponsesMessage(it responsesItem) (map[string]any, error) {
	role := it.Role
	if role == "" {
		role = "user"
	}
	var s string
	if err := json.Unmarshal(it.Content, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return map[string]any{"role": role, "content": s}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(it.Content, &parts); err != nil {
		return nil, nil // 无法解析的 content 跳过（不阻塞整请求）
	}
	var text strings.Builder
	var contentParts []map[string]any
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "refusal", "":
			text.WriteString(p.Text)
		case "input_image":
			if p.ImageURL != "" {
				contentParts = append(contentParts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": p.ImageURL},
				})
			}
		}
	}
	if text.Len() > 0 {
		contentParts = append([]map[string]any{{"type": "text", "text": text.String()}}, contentParts...)
	}
	if len(contentParts) == 0 {
		return nil, nil
	}
	if len(contentParts) == 1 && contentParts[0]["type"] == "text" {
		return map[string]any{"role": role, "content": contentParts[0]["text"]}, nil
	}
	return map[string]any{"role": role, "content": contentParts}, nil
}

// flattenResponsesText string | []part → 纯文本（function_call_output 用）。
func flattenResponsesText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "input_text" || p.Type == "output_text" || p.Type == "" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// convertResponsesToolChoice Responses tool_choice → OpenAI tool_choice。
func convertResponsesToolChoice(raw json.RawMessage) any {
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
	if obj.Type == "function" && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// responsesEffort 取 reasoning.effort。
func responsesEffort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	switch obj.Effort {
	case "low", "medium", "high":
		return obj.Effort
	}
	return ""
}

// responsesTextFormatHint text.format → system 指令补充（结构化输出约束）。
func responsesTextFormatHint(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Format struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict bool            `json:"strict"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	switch obj.Format.Type {
	case "json_object":
		return "Respond with a valid JSON object only, no markdown fences or extra text."
	case "json_schema":
		if len(obj.Format.Schema) == 0 {
			return ""
		}
		return "Respond with a valid JSON object conforming to this JSON Schema, no markdown fences or extra text:\n" + string(obj.Format.Schema)
	}
	return ""
}

// ── 响应翻译：OpenAI SSE / 聚合 JSON → Responses 事件流 / response ─────

// responsesItemState 一个输出 item（message / reasoning / function_call）的
// 流式状态。
type responsesItemState struct {
	kind        string // message | reasoning | function_call
	itemID      string
	outputIndex int
	text        strings.Builder // message：累积全文（done 事件回显用）
	args        strings.Builder // function_call：累积参数
	name        string
	callID      string
	partOpen    bool
}

// responsesWriter 包装真实 ResponseWriter，把 chatCore 写出的 OpenAI 形态
// 响应翻译成 Responses 协议形态（流式事件序列 / 非流式 response 对象）。
type responsesWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	stream  bool

	respID     string
	createdAt  int64
	started    bool
	sawDone    bool
	seq        int
	nextOutIdx int

	textItem  *responsesItemState
	thinkItem *responsesItemState
	toolItems map[int]*responsesItemState // openai tool index → item
	itemOrder []*responsesItemState       // 开item顺序（completed 时 output 数组用）

	inTok  int
	outTok int
	reasonTok int

	frameBuf strings.Builder
	buf      []byte
	finished bool
}

func newResponsesWriter(w http.ResponseWriter, model string, stream bool) *responsesWriter {
	fl, _ := w.(http.Flusher)
	return &responsesWriter{
		w: w, flusher: fl, model: model, stream: stream,
		respID: "resp_" + randHex(16), createdAt: time.Now().Unix(), toolItems: map[int]*responsesItemState{},
	}
}

func (rw *responsesWriter) Header() http.Header    { return rw.w.Header() }
func (rw *responsesWriter) WriteHeader(status int) { rw.w.WriteHeader(status) }

// Flush 实现 http.Flusher（StreamHint 逐帧 flush 依赖该接口）。
func (rw *responsesWriter) Flush() {
	if rw.flusher != nil {
		rw.flusher.Flush()
	}
}
func (rw *responsesWriter) Write(p []byte) (int, error) {
	if rw.stream {
		rw.frameBuf.Write(p)
		rw.drainFrames()
		return len(p), nil
	}
	rw.buf = append(rw.buf, p...)
	return len(p), nil
}

func (rw *responsesWriter) drainFrames() {
	buf := rw.frameBuf.String()
	for {
		idx := strings.Index(buf, "\n\n")
		if idx < 0 {
			break
		}
		frame := buf[:idx]
		buf = buf[idx+2:]
		rw.feedFrame(frame)
	}
	rw.frameBuf.Reset()
	rw.frameBuf.WriteString(buf)
}

func (rw *responsesWriter) feedFrame(frame string) {
	frame = strings.TrimRight(frame, "\r")
	if frame == "" {
		return
	}
	payload := frame
	if strings.HasPrefix(frame, "data:") {
		payload = strings.TrimSpace(frame[len("data:"):])
	}
	if payload == "" || payload == "[DONE]" {
		if payload == "[DONE]" {
			rw.finishStream()
		}
		return
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	if errObj, ok := chunk["error"].(map[string]any); ok {
		rw.emitErrorFrame(errObj)
		rw.finishStream()
		return
	}
	rw.ensureStarted()
	if choices, ok := chunk["choices"].([]any); ok {
		for _, ci := range choices {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if d, ok := c["delta"].(map[string]any); ok {
				rw.feedDelta(d)
			}
		}
	}
	if u, ok := chunk["usage"].(map[string]any); ok {
		rw.inTok = int(jsonFloat(u["prompt_tokens"]))
		rw.outTok = int(jsonFloat(u["completion_tokens"]))
		if det, ok := u["completion_tokens_details"].(map[string]any); ok {
			rw.reasonTok = int(jsonFloat(det["reasoning_tokens"]))
		}
	}
}

// ensureStarted 首帧到达时发 response.created + response.in_progress（幂等）。
// 仅流式路径调用——非流式走 finish() 直接产出 response 对象，发 SSE 事件
// 会把事件写进 JSON 响应体（实测踩过）。
func (rw *responsesWriter) ensureStarted() {
	if rw.started {
		return
	}
	rw.started = true
	resp := rw.responseSkeleton("in_progress")
	rw.emitEvent("response.created", map[string]any{"response": resp})
	rw.emitEvent("response.in_progress", map[string]any{"response": resp})
}

// responseSkeleton 响应对象骨架（created/in_progress/completed 共用字段）。
func (rw *responsesWriter) responseSkeleton(status string) map[string]any {
	resp := map[string]any{
		"id":         rw.respID,
		"object":     "response",
		"created_at": rw.createdAt,
		"status":     status,
		"model":      rw.model,
		"output":     []any{},
	}
	if status == "completed" {
		resp["usage"] = map[string]any{
			"input_tokens":  rw.inTok,
			"output_tokens": rw.outTok,
			"total_tokens":  rw.inTok + rw.outTok,
			"output_tokens_details": map[string]any{
				"reasoning_tokens": rw.reasonTok,
			},
			"input_tokens_details": map[string]any{"cached_tokens": 0},
		}
	}
	return resp
}

// feedDelta OpenAI delta → Responses 事件。
func (rw *responsesWriter) feedDelta(d map[string]any) {
	if rc, ok := d["reasoning_content"].(string); ok && rc != "" {
		rw.openReasoning()
		rw.emitEvent("response.reasoning_text.delta", map[string]any{
			"item_id": rw.thinkItem.itemID, "output_index": rw.thinkItem.outputIndex,
			"content_index": 0, "delta": rc,
		})
	}
	if content, ok := d["content"].(string); ok && content != "" {
		rw.openText()
		rw.textItem.text.WriteString(content)
		rw.emitEvent("response.output_text.delta", map[string]any{
			"item_id": rw.textItem.itemID, "output_index": rw.textItem.outputIndex,
			"content_index": 0, "delta": content,
		})
	}
	if tcs, ok := d["tool_calls"].([]any); ok {
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			oaIdx := int(jsonFloat(tc["index"]))
			item, open := rw.toolItems[oaIdx]
			if !open {
				callID, _ := tc["id"].(string)
				if callID == "" {
					callID = "call_" + randHex(12)
				}
				name := ""
				if fn, ok := tc["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				item = &responsesItemState{
					kind: "function_call", itemID: "fc_" + randHex(12),
					outputIndex: rw.allocOutIdx(), name: name, callID: callID,
				}
				rw.toolItems[oaIdx] = item
				rw.itemOrder = append(rw.itemOrder, item)
				rw.emitEvent("response.output_item.added", map[string]any{
					"output_index": item.outputIndex,
					"item": map[string]any{
						"id": item.itemID, "type": "function_call", "status": "in_progress",
						"name": name, "call_id": callID, "arguments": "",
					},
				})
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				if args, ok := fn["arguments"].(string); ok && args != "" {
					item.args.WriteString(args)
					rw.emitEvent("response.function_call_arguments.delta", map[string]any{
						"item_id": item.itemID, "output_index": item.outputIndex, "delta": args,
					})
				}
			}
		}
	}
}

func (rw *responsesWriter) openText() {
	if rw.textItem != nil {
		return
	}
	rw.textItem = &responsesItemState{
		kind: "message", itemID: "msg_" + randHex(12), outputIndex: rw.allocOutIdx(),
	}
	rw.itemOrder = append(rw.itemOrder, rw.textItem)
	rw.emitEvent("response.output_item.added", map[string]any{
		"output_index": rw.textItem.outputIndex,
		"item": map[string]any{
			"id": rw.textItem.itemID, "type": "message", "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	})
	rw.textItem.partOpen = true
	rw.emitEvent("response.content_part.added", map[string]any{
		"item_id": rw.textItem.itemID, "output_index": rw.textItem.outputIndex,
		"content_index": 0,
		"part":         map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (rw *responsesWriter) openReasoning() {
	if rw.thinkItem != nil {
		return
	}
	rw.thinkItem = &responsesItemState{
		kind: "reasoning", itemID: "rs_" + randHex(12), outputIndex: rw.allocOutIdx(),
	}
	rw.itemOrder = append(rw.itemOrder, rw.thinkItem)
	rw.emitEvent("response.output_item.added", map[string]any{
		"output_index": rw.thinkItem.outputIndex,
		"item": map[string]any{
			"id": rw.thinkItem.itemID, "type": "reasoning", "summary": []any{},
		},
	})
}

func (rw *responsesWriter) allocOutIdx() int {
	idx := rw.nextOutIdx
	rw.nextOutIdx++
	return idx
}

// emitErrorFrame 上游 error 帧 → Responses error 事件。
func (rw *responsesWriter) emitErrorFrame(errObj map[string]any) {
	rw.ensureStarted()
	msg, _ := errObj["message"].(string)
	if msg == "" {
		msg = "upstream error"
	}
	rw.emitEvent("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": msg, "code": nil},
	})
}

// finishStream 流式收尾：逐 item 发 done 事件 → response.completed。幂等。
func (rw *responsesWriter) finishStream() {
	if rw.sawDone {
		return
	}
	rw.sawDone = true
	rw.ensureStarted()
	for _, item := range rw.itemOrder {
		switch item.kind {
		case "message":
			if item.partOpen {
				rw.emitEvent("response.output_text.done", map[string]any{
					"item_id": item.itemID, "output_index": item.outputIndex,
					"content_index": 0, "text": item.text.String(),
				})
				rw.emitEvent("response.content_part.done", map[string]any{
					"item_id": item.itemID, "output_index": item.outputIndex,
					"content_index": 0,
					"part":         map[string]any{"type": "output_text", "text": item.text.String(), "annotations": []any{}},
				})
			}
			rw.emitEvent("response.output_item.done", map[string]any{
				"output_index": item.outputIndex,
				"item": map[string]any{
					"id": item.itemID, "type": "message", "status": "completed",
					"role": "assistant",
					"content": []any{map[string]any{
						"type": "output_text", "text": item.text.String(), "annotations": []any{},
					}},
				},
			})
		case "reasoning":
			rw.emitEvent("response.reasoning_text.done", map[string]any{
				"item_id": item.itemID, "output_index": item.outputIndex, "content_index": 0, "text": "",
			})
			rw.emitEvent("response.output_item.done", map[string]any{
				"output_index": item.outputIndex,
				"item": map[string]any{
					"id": item.itemID, "type": "reasoning", "summary": []any{},
				},
			})
		case "function_call":
			rw.emitEvent("response.function_call_arguments.done", map[string]any{
				"item_id": item.itemID, "output_index": item.outputIndex,
				"arguments": item.args.String(),
			})
			rw.emitEvent("response.output_item.done", map[string]any{
				"output_index": item.outputIndex,
				"item": map[string]any{
					"id": item.itemID, "type": "function_call", "status": "completed",
					"name": item.name, "call_id": item.callID, "arguments": item.args.String(),
				},
			})
		}
	}
	resp := rw.responseSkeleton("completed")
	outputs := make([]any, 0, len(rw.itemOrder))
	for _, item := range rw.itemOrder {
		outputs = append(outputs, rw.itemJSON(item, "completed"))
	}
	resp["output"] = outputs
	rw.emitEvent("response.completed", map[string]any{"response": resp})
	if rw.flusher != nil {
		rw.flusher.Flush()
	}
}

// itemJSON item 的终态 JSON（completed 事件的 output 数组用）。
func (rw *responsesWriter) itemJSON(item *responsesItemState, status string) map[string]any {
	switch item.kind {
	case "message":
		return map[string]any{
			"id": item.itemID, "type": "message", "status": status, "role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": item.text.String(), "annotations": []any{},
			}},
		}
	case "reasoning":
		return map[string]any{
			"id": item.itemID, "type": "reasoning", "summary": []any{},
		}
	case "function_call":
		return map[string]any{
			"id": item.itemID, "type": "function_call", "status": status,
			"name": item.name, "call_id": item.callID, "arguments": item.args.String(),
		}
	}
	return map[string]any{}
}

// finish 非流式收尾：聚合 JSON → response 对象。
func (rw *responsesWriter) finish() {
	if rw.stream || rw.finished {
		return
	}
	rw.finished = true
	if len(rw.buf) == 0 {
		return
	}
	var resp map[string]any
	if err := json.Unmarshal(rw.buf, &resp); err != nil {
		_, _ = rw.w.Write(rw.buf)
		return
	}
	if errObj, ok := resp["error"].(map[string]any); ok {
		// 网关错误 → Responses 错误信封（与 OpenAI 兼容形态一致）。
		typ, _ := errObj["type"].(string)
		if typ == "" {
			typ = "api_error"
		}
		code, _ := errObj["code"].(string)
		msg, _ := errObj["message"].(string)
		_ = json.NewEncoder(rw.w).Encode(map[string]any{
			"error": map[string]any{"type": typ, "code": code, "message": msg},
		})
		return
	}
	// 聚合响应 → output items（非流式不发任何 SSE 事件）。
	var outputs []any
	var text strings.Builder
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if m, ok := c["message"].(map[string]any); ok {
				if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
					outputs = append(outputs, map[string]any{
						"id": "rs_" + randHex(12), "type": "reasoning", "summary": []any{},
					})
				}
				if s, ok := m["content"].(string); ok {
					text.WriteString(s)
				}
				if tcs, ok := m["tool_calls"].([]any); ok {
					for _, tci := range tcs {
						tc, _ := tci.(map[string]any)
						if tc == nil {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						name, args := "", "{}"
						callID, _ := tc["id"].(string)
						if fn != nil {
							name, _ = fn["name"].(string)
							if s, ok := fn["arguments"].(string); ok && s != "" {
								args = s
							}
						}
						outputs = append(outputs, map[string]any{
							"id": "fc_" + randHex(12), "type": "function_call", "status": "completed",
							"name": name, "call_id": callID, "arguments": args,
						})
					}
				}
			}
		}
	}
	if text.Len() > 0 {
		outputs = append([]any{map[string]any{
			"id": "msg_" + randHex(12), "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": text.String(), "annotations": []any{},
			}},
		}}, outputs...)
	}
	if u, ok := resp["usage"].(map[string]any); ok {
		rw.inTok = int(jsonFloat(u["prompt_tokens"]))
		rw.outTok = int(jsonFloat(u["completion_tokens"]))
		if det, ok := u["completion_tokens_details"].(map[string]any); ok {
			rw.reasonTok = int(jsonFloat(det["reasoning_tokens"]))
		}
	}
	out := rw.responseSkeleton("completed")
	out["output"] = outputs
	_ = json.NewEncoder(rw.w).Encode(out)
}

// emitEvent 写出一帧 Responses SSE 事件（带递增 sequence_number）。
func (rw *responsesWriter) emitEvent(event string, payload map[string]any) {
	payload["type"] = event
	payload["sequence_number"] = rw.seq
	rw.seq++
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
	if _, err := io.WriteString(rw.w, sb.String()); err != nil {
		return
	}
	if rw.flusher != nil {
		rw.flusher.Flush()
	}
}

// ── Handler ────────────────────────────────────────────────────────────

func (h *Handler) responsesAPI(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), nil)
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error(), nil)
		return
	}
	// 模型名映射（外来名 → 上游名）在结构转换之前做；原始名进请求跟踪，
	// 面板「请求记录」展示「原始名 → 解析名」（同 anthropicMessages）。
	inboundModel := req.Model
	req.Model = h.resolveInboundModel(req.Model)
	if req.Model == "" {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "model: field required", nil)
		return
	}
	if tr := requestTraceFrom(r); tr != nil && inboundModel != req.Model {
		tr.inboundModel = inboundModel
	}
	openaiBody, err := responsesToOpenAI(&req)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), nil)
		return
	}
	rw := newResponsesWriter(w, req.Model, req.Stream)
	h.chatCore(rw, r, openaiBody)
	rw.finish()
}

// writeResponsesError Responses 协议错误信封。
func writeResponsesError(w http.ResponseWriter, status int, typ, msg string, code any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": typ, "code": code, "message": msg},
	})
}
