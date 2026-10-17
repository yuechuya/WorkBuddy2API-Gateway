package server

// 外来模型名映射（inbound model alias）。
//
// 为什么需要：Anthropic / Responses 两类入站协议的客户端只会说自己的模型名
// （claude-sonnet-4-5、gpt-5-codex…），上游 CodeBuddy 账号只认自己的模型名
// （glm-5.2、deepseek-v4.1-flash…）。此前这条映射要靠 CCR 等第三方转换层
// 代劳——多一跳、且规则散落在网关之外。网关原生支持两类协议后，映射收回门内。
//
// 解析顺序（首中即停）：
//  1. 别名规则（pattern 支持 * / ? 通配，大小写不敏感，按配置顺序）；
//  2. 上游动态模型目录（只读缓存，不发起上游调用）命中 → 原样透传
//     （OpenAI 客户端直呼上游真实模型名时不该被改写）；
//  3. 都不命中 → default_model 回落。
//
// 默认表只是兜底：上游目录以 /v1/models 实际返回为准，用户可在 config 的
// inbound.model_alias 段按自己账号的模型清单调整。

import (
	"strings"
	"sync"
)

// InboundConfig 外来协议入站配置（/v1/messages、/v1/responses 共用）。
// 对应 config.json 的 inbound 段。
type InboundConfig struct {
	// DefaultModel 别名规则与上游目录都不命中时的回落模型名。
	DefaultModel string `json:"default_model"`
	// Rules 别名规则列表，按顺序首中即停。
	Rules []ModelAliasRule `json:"rules"`
}

// ModelAliasRule 一条别名规则：Pattern 匹配外来模型名 → Model 为上游模型名。
type ModelAliasRule struct {
	Pattern string `json:"pattern"`
	Model   string `json:"model"`
}

// defaultInboundConfig 内置默认映射（config 未配置 inbound 段时使用）。
//
// 档位选择的依据是「客户端语义 → 上游能力」的粗略对齐，不是精确等价；
// 目标模型名以 /v1/models 实际返回为准（下表经 2026-10-17 真实账号目录核对）：
//   - haiku（快而廉） → glm-5.3-flash（x0.06，最低倍率的可用模型）
//   - codex（编码场景） → kimi-k2.7（Kimi-K2.7-Code，代码特化）
//   - sonnet（主力均衡） → glm-5.2（x0.79，旗舰通用）
//   - opus（最强推理） → deepseek-v4-pro（x0.51，旗舰 + 思维链）
//   - 其余 gpt-* → glm-5.2
// 上游模型清单随账号变动，以 /v1/models 为准；不合适就在 config 里改。
func defaultInboundConfig() *InboundConfig {
	return &InboundConfig{
		DefaultModel: "glm-5.3-flash",
		Rules: []ModelAliasRule{
			{Pattern: "*haiku*", Model: "glm-5.3-flash"},
			{Pattern: "*codex*", Model: "kimi-k2.7"},
			{Pattern: "*sonnet*", Model: "glm-5.2"},
			{Pattern: "*opus*", Model: "deepseek-v4-pro"},
			{Pattern: "*gpt-*", Model: "glm-5.2"},
		},
	}
}

// inboundAlias 返回生效的入站配置：用户规则在前、内置规则兜底（首中即停，
// 所以用户规则天然覆盖同 pattern 的内置规则）；default_model 用户没写则
// 回落到内置默认。语义 = 「显式配置优先，缺口的用内置补」，避免用户只加
// 一条自定义规则就把全部内置映射弄丢。
func inboundAlias(cfg *InboundConfig) *InboundConfig {
	def := defaultInboundConfig()
	if cfg == nil {
		return def
	}
	out := &InboundConfig{DefaultModel: cfg.DefaultModel}
	if out.DefaultModel == "" {
		out.DefaultModel = def.DefaultModel
	}
	out.Rules = append(out.Rules, cfg.Rules...)
	out.Rules = append(out.Rules, def.Rules...)
	return out
}

// aliasLogOnce 同一 (from→to) 组合只记一行日志。
// Claude Code 每轮请求都带同一个模型名，不折叠会把运行日志刷满。
var aliasLogOnce sync.Map

func logAliasOnce(from, to string) {
	key := from + "->" + to
	if _, loaded := aliasLogOnce.LoadOrStore(key, struct{}{}); !loaded {
		// 由调用方带日志前缀，这里只做去重。
		aliasLogHook(key)
	}
}

// aliasLogHook 便于测试注入观测；生产环境由 server 包初始化。
var aliasLogHook = func(string) {}

// resolveInboundModel 把外来协议的模型名解析为上游模型名。
// 解析顺序见文件头注释；返回空串表示无法解析（调用方应报错）。
func (h *Handler) resolveInboundModel(model string) string {
	cfg := inboundAlias(h.cfg.Inbound)
	if model == "" {
		return cfg.DefaultModel
	}
	// 1) 别名规则首中。
	for _, rule := range cfg.Rules {
		if rule.Model != "" && wildcardMatch(rule.Pattern, model) {
			logAliasOnce(model, rule.Model)
			return rule.Model
		}
	}
	// 2) 上游动态目录命中（大小写不敏感）→ 透传。
	for _, mi := range cachedModelsSnapshot() {
		if strings.EqualFold(mi.ID, model) {
			return model
		}
	}
	// 3) 回落默认。
	if cfg.DefaultModel != "" {
		logAliasOnce(model, cfg.DefaultModel)
		return cfg.DefaultModel
	}
	// 未配置默认（显式清空）：原样透传，让上游给出真实错误（信息量最大）。
	return model
}

// wildcardMatch 大小写不敏感的通配匹配：* 匹配任意串（含空），? 匹配单字节。
// 双指针 + 单回溯点实现（经典 glob 匹配），对 ASCII 模型名足够且零分配。
func wildcardMatch(pattern, s string) bool {
	p := []byte(strings.ToLower(pattern))
	t := []byte(strings.ToLower(s))
	var pi, ti, star, mark int
	star = -1
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]):
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star = pi
			pi++
			mark = ti
		case star != -1:
			// 回溯：* 多吞一个字符重试。
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
