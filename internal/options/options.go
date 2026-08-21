// Package options 提供 Agent 可配置项（LLM 模型 / MCP 工具 / Skill）的可选清单。
//
// 当前为内置静态清单，后续可替换为从配置中心或数据库动态加载，
// 对外暴露的 List* 方法签名保持不变即可无缝切换。
package options

import agenteamv1 "github.com/hwanyan/agenteam/pb/gen"

// Models 返回当前平台支持的 LLM 模型选项。
// 模型 id 会作为 Agent.Model 字段的取值，并原样传给 OpenAI 兼容的 Chat Completions 接口。
func Models() []*agenteamv1.ModelOption {
	return []*agenteamv1.ModelOption{
		{Id: "gpt-4o-mini", Name: "GPT-4o mini", Provider: "openai", Description: "轻量、低成本，适合日常对话与工具调用"},
		{Id: "gpt-4o", Name: "GPT-4o", Provider: "openai", Description: "综合能力更强，适合复杂推理任务"},
		{Id: "deepseek-chat", Name: "DeepSeek Chat", Provider: "deepseek", Description: "性价比较高的中文对话模型"},
		{Id: "qwen-plus", Name: "Qwen Plus", Provider: "qwen", Description: "阿里通义千问，中文场景表现良好"},
		{Id: "glm-4", Name: "GLM-4", Provider: "zhipu", Description: "智谱 GLM-4，支持长文本与工具调用"},
	}
}

// MCPTools 返回当前平台内置的 MCP 工具选项。
func MCPTools() []*agenteamv1.ToolOption {
	return []*agenteamv1.ToolOption{
		{Id: "web-search", Name: "网页搜索", Description: "检索互联网最新信息"},
		{Id: "code-interpreter", Name: "代码执行", Description: "在沙箱环境中执行代码片段"},
		{Id: "filesystem", Name: "文件系统", Description: "读写受限目录下的文件"},
		{Id: "http-request", Name: "HTTP 请求", Description: "调用外部 HTTP/REST 接口"},
		{Id: "database-query", Name: "数据库查询", Description: "对接的数据库执行只读查询"},
	}
}

// Skills 返回当前平台内置的 Skill 选项。
func Skills() []*agenteamv1.ToolOption {
	return []*agenteamv1.ToolOption{
		{Id: "text-summarization", Name: "文本摘要", Description: "对长文本进行提炼总结"},
		{Id: "translation", Name: "多语言翻译", Description: "在中英日等语言间互译"},
		{Id: "data-analysis", Name: "数据分析", Description: "对结构化数据进行统计分析"},
		{Id: "doc-writer", Name: "文档撰写", Description: "生成结构化的文档/报告"},
		{Id: "code-review", Name: "代码审查", Description: "对代码变更给出审查意见"},
	}
}

func modelIDs() map[string]bool {
	m := make(map[string]bool)
	for _, opt := range Models() {
		m[opt.Id] = true
	}
	return m
}

func toolIDs() map[string]bool {
	m := make(map[string]bool)
	for _, opt := range MCPTools() {
		m[opt.Id] = true
	}
	return m
}

func skillIDs() map[string]bool {
	m := make(map[string]bool)
	for _, opt := range Skills() {
		m[opt.Id] = true
	}
	return m
}

// IsValidModel 校验模型 id 是否在可选清单内。
func IsValidModel(id string) bool { return modelIDs()[id] }

// IsValidTool 校验 MCP 工具 id 是否在可选清单内。
func IsValidTool(id string) bool { return toolIDs()[id] }

// IsValidSkill 校验 Skill id 是否在可选清单内。
func IsValidSkill(id string) bool { return skillIDs()[id] }

// DefaultModel 是新建 Agent 时的默认模型。
const DefaultModel = "gpt-4o-mini"
