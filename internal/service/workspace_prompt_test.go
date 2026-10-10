package service

import (
	"strings"
	"testing"

	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

func TestPickOptionsDropsUnknownIDsAndKeepsOrder(t *testing.T) {
	available := map[string]*agenteamv1.ToolOption{
		"web-search":      {Id: "web-search", Name: "网页搜索"},
		"mcp_tool_alive1": {Id: "mcp_tool_alive1", Name: "新闻源", IsCustom: true},
	}
	// mcp_tool_stale 模拟"mcp_tools 表里已被删除、但 agents.mcp_tools 里还残留"的失效 id。
	got := pickOptions([]string{"mcp_tool_alive1", "mcp_tool_stale", "web-search"}, available)

	if len(got) != 2 {
		t.Fatalf("want 2 options (stale id dropped), got %d", len(got))
	}
	if got[0].Id != "mcp_tool_alive1" || got[1].Id != "web-search" {
		t.Fatalf("order not preserved: %v, %v", got[0].Id, got[1].Id)
	}
}

func TestComposeSystemPromptShowsNamesNotIDs(t *testing.T) {
	agent := &agenteamv1.Agent{Prompt: "你是主 Agent。"}
	mcpTools := []*agenteamv1.ToolOption{
		{Id: "mcp_tool_alive1", Name: "新闻源", IsCustom: true},
		{Id: "web-search", Name: "网页搜索", Description: "检索互联网最新信息"},
	}

	prompt := composeSystemPrompt(agent, mcpTools, "")

	for _, want := range []string{"- 新闻源\n", "- 网页搜索：检索互联网最新信息\n", "不要提及或展示任何内部 ID"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, leaked := range []string{"mcp_tool_alive1", "web-search"} {
		if strings.Contains(prompt, leaked) {
			t.Errorf("prompt must not expose id %q:\n%s", leaked, prompt)
		}
	}
}

func TestComposeSystemPromptWithoutToolsHasNoToolSection(t *testing.T) {
	agent := &agenteamv1.Agent{Prompt: "你是主 Agent。"}

	prompt := composeSystemPrompt(agent, nil, "")

	if prompt != "你是主 Agent。" {
		t.Fatalf("want bare prompt, got %q", prompt)
	}
}
