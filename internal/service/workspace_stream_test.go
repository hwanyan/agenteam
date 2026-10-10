package service

import (
	"testing"

	"github.com/hwanyan/agenteam/internal/a2a"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// fakeSender 记录 recorder 转发出去的全部流式响应。
type fakeSender struct {
	sent []*agenteamv1.SendMessageStreamResponse
}

func (f *fakeSender) Send(r *agenteamv1.SendMessageStreamResponse) error {
	f.sent = append(f.sent, r)
	return nil
}

func TestRecorderAccumulatesContentAndReasoning(t *testing.T) {
	out := &fakeSender{}
	rec := newStreamRecorder(out)

	_ = rec.sendReasoning("先想")
	_ = rec.sendReasoning("一下")
	_ = rec.sendDelta("答案")
	_ = rec.sendDelta("") // 空文本不应产生任何响应

	if rec.reasoningText() != "先想一下" || rec.contentText() != "答案" {
		t.Fatalf("got reasoning=%q content=%q", rec.reasoningText(), rec.contentText())
	}
	if len(out.sent) != 3 {
		t.Fatalf("want 3 forwarded responses (empty delta ignored), got %d", len(out.sent))
	}
	if out.sent[0].ReasoningDelta != "先想" || out.sent[2].Delta != "答案" {
		t.Fatalf("unexpected forwarded payloads: %+v", out.sent)
	}
}

func TestRecorderToolLifecycle(t *testing.T) {
	out := &fakeSender{}
	rec := newStreamRecorder(out)

	if err := rec.onA2ATool(&a2a.ToolEvent{Phase: a2a.ToolPhaseStart, Name: "get_price", Arguments: `{"market":"XAU"}`}); err != nil {
		t.Fatal(err)
	}
	if err := rec.onA2ATool(&a2a.ToolEvent{Phase: a2a.ToolPhaseSuccess, Name: "get_price"}); err != nil {
		t.Fatal(err)
	}

	// 前端据此按 id 合并：先收到 running，再收到同一个 id 的 done。
	if len(out.sent) != 2 {
		t.Fatalf("want 2 tool events, got %d", len(out.sent))
	}
	first, second := out.sent[0].ToolCall, out.sent[1].ToolCall
	if first.Status != toolStatusRunning || second.Status != toolStatusDone || first.Id != second.Id {
		t.Fatalf("want running then done with same id, got %+v then %+v", first, second)
	}
	// 已发出的 running 响应不能被后续状态变更污染（recorder 发送的是克隆）。
	if first.Status != toolStatusRunning {
		t.Fatalf("already-sent event was mutated: %+v", first)
	}
}

func TestRecorderFinishToolByNameMatchesLatestRunningCall(t *testing.T) {
	rec := newStreamRecorder(&fakeSender{})

	_ = rec.onA2ATool(&a2a.ToolEvent{Phase: a2a.ToolPhaseStart, Name: "search"})
	_ = rec.onA2ATool(&a2a.ToolEvent{Phase: a2a.ToolPhaseSuccess, Name: "search"})
	_ = rec.onA2ATool(&a2a.ToolEvent{Phase: a2a.ToolPhaseStart, Name: "search"})
	_ = rec.onA2ATool(&a2a.ToolEvent{Phase: a2a.ToolPhaseError, Name: "search"})

	calls := rec.toolCallsSnapshot()
	if len(calls) != 2 || calls[0].Status != toolStatusDone || calls[1].Status != toolStatusError {
		t.Fatalf("same-name calls must be resolved one by one in order, got %+v", calls)
	}
}

func TestRecorderIgnoresResultWithoutStart(t *testing.T) {
	out := &fakeSender{}
	rec := newStreamRecorder(out)

	_ = rec.onA2ATool(&a2a.ToolEvent{Phase: a2a.ToolPhaseSuccess, Name: "ghost"})

	if len(out.sent) != 0 || len(rec.toolCallsSnapshot()) != 0 {
		t.Fatal("a result event with no matching running call must be ignored")
	}
}

func TestRecorderSettleToolsLeavesNoRunningCall(t *testing.T) {
	rec := newStreamRecorder(&fakeSender{})
	_, _ = rec.startTool("a", toolKindTool, "")
	id, _ := rec.startTool("b", toolKindTool, "")
	_ = rec.finishTool(id, toolStatusDone)

	rec.settleTools(toolStatusError)

	calls := rec.toolCallsSnapshot()
	if calls[0].Status != toolStatusError || calls[1].Status != toolStatusDone {
		t.Fatalf("settle must only touch running calls, got %+v", calls)
	}
}

func TestRecorderTruncatesLongToolArguments(t *testing.T) {
	rec := newStreamRecorder(&fakeSender{})
	long := ""
	for i := 0; i < maxToolArgsRunes+50; i++ {
		long += "中"
	}

	_, _ = rec.startTool("t", toolKindTool, long)

	got := []rune(rec.toolCallsSnapshot()[0].Arguments)
	if len(got) != maxToolArgsRunes+len("...") {
		t.Fatalf("want %d runes (truncated + ellipsis), got %d", maxToolArgsRunes+3, len(got))
	}
}
