package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/hwanyan/agenteam/internal/a2a"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// 工具调用展示信息（agenteamv1.ToolCallInfo）的取值约定，前端按同样的字符串判断。
const (
	toolStatusRunning = "running"
	toolStatusDone    = "done"
	toolStatusError   = "error"

	toolKindAgent = "agent" // 团队主 Agent 把请求委派给了某个子 Agent
	toolKindTool  = "tool"  // Agent（如接入的 A2A Agent）在处理过程中调用的工具

	// maxToolArgsRunes 是工具入参展示文本的最大长度，超出部分截断，避免过长入参撑大消息体。
	maxToolArgsRunes = 300
)

// streamSender 是 recorder 向客户端转发流式响应所需的最小接口
// （agenteamv1.WorkspaceService_SendMessageStreamServer 天然满足；单独声明便于测试替身）。
type streamSender interface {
	Send(*agenteamv1.SendMessageStreamResponse) error
}

// streamRecorder 汇总一次流式回复过程中产生的全部内容——正文、思考过程、工具调用，
// 在向客户端实时转发的同时把它们记录下来，流结束后随这条回复一起落库，
// 这样历史消息里也能看到当时的思考过程与工具调用，而不是只在流式过程中一闪而过。
//
// 非并发安全：一次流式回复只在单个 goroutine 里顺序推进。
type streamRecorder struct {
	out       streamSender
	content   strings.Builder
	reasoning strings.Builder
	toolCalls []*agenteamv1.ToolCallInfo
}

func newStreamRecorder(out streamSender) *streamRecorder {
	return &streamRecorder{out: out}
}

// sendDelta 转发并记录一段正文增量；空文本直接忽略。
func (r *streamRecorder) sendDelta(text string) error {
	if text == "" {
		return nil
	}
	r.content.WriteString(text)
	return r.out.Send(&agenteamv1.SendMessageStreamResponse{Delta: text})
}

// sendReasoning 转发并记录一段思考过程增量；空文本直接忽略。
func (r *streamRecorder) sendReasoning(text string) error {
	if text == "" {
		return nil
	}
	r.reasoning.WriteString(text)
	return r.out.Send(&agenteamv1.SendMessageStreamResponse{ReasoningDelta: text})
}

// startTool 记录并转发一次工具调用的开始（status=running），返回该调用的 id，
// 供后续 finishTool 精确结束它。
func (r *streamRecorder) startTool(name, kind, args string) (string, error) {
	call := &agenteamv1.ToolCallInfo{
		Id:        fmt.Sprintf("call-%d", len(r.toolCalls)+1),
		Name:      name,
		Kind:      kind,
		Arguments: truncateRunes(args, maxToolArgsRunes),
		Status:    toolStatusRunning,
	}
	r.toolCalls = append(r.toolCalls, call)
	return call.Id, r.sendTool(call)
}

// finishTool 把指定 id 的调用（若仍在运行）结束为 status，并转发更新后的状态。
func (r *streamRecorder) finishTool(id, status string) error {
	for _, c := range r.toolCalls {
		if c.Id == id && c.Status == toolStatusRunning {
			c.Status = status
			return r.sendTool(c)
		}
	}
	return nil
}

// finishToolByName 把最近一个同名且仍在运行的调用结束为 status。
// 对端 A2A Agent 的工具事件只带工具名、不带调用 id，且按顺序逐个调用，按"最近一个同名"匹配即可。
func (r *streamRecorder) finishToolByName(name, status string) error {
	for i := len(r.toolCalls) - 1; i >= 0; i-- {
		c := r.toolCalls[i]
		if c.Name == name && c.Status == toolStatusRunning {
			c.Status = status
			return r.sendTool(c)
		}
	}
	return nil
}

// onA2ATool 处理对端 A2A Agent 在处理消息过程中产生的工具调用过程事件。
func (r *streamRecorder) onA2ATool(ev *a2a.ToolEvent) error {
	switch ev.Phase {
	case a2a.ToolPhaseStart:
		_, err := r.startTool(ev.Name, toolKindTool, ev.Arguments)
		return err
	case a2a.ToolPhaseSuccess:
		return r.finishToolByName(ev.Name, toolStatusDone)
	case a2a.ToolPhaseError:
		return r.finishToolByName(ev.Name, toolStatusError)
	}
	return nil
}

// settleTools 把仍处于 running 的调用收尾为 status，保证落库的记录里不会残留"进行中"的调用
// （例如对端没有回传结束事件、或流中途失败）。只改本地记录，不再转发——
// 前端以最后一条 done 响应里的完整消息为准。
func (r *streamRecorder) settleTools(status string) {
	for _, c := range r.toolCalls {
		if c.Status == toolStatusRunning {
			c.Status = status
		}
	}
}

func (r *streamRecorder) sendTool(call *agenteamv1.ToolCallInfo) error {
	// 发送克隆：recorder 后续还会修改 call 的状态，不能让已发出的响应跟着变。
	return r.out.Send(&agenteamv1.SendMessageStreamResponse{ToolCall: proto.Clone(call).(*agenteamv1.ToolCallInfo)})
}

func (r *streamRecorder) contentText() string   { return r.content.String() }
func (r *streamRecorder) reasoningText() string { return r.reasoning.String() }

// hasContent 表示是否已经流出过正文。
func (r *streamRecorder) hasContent() bool { return r.content.Len() > 0 }

// toolCallsSnapshot 返回当前全部工具调用的拷贝，供落库。
func (r *streamRecorder) toolCallsSnapshot() []*agenteamv1.ToolCallInfo {
	if len(r.toolCalls) == 0 {
		return nil
	}
	out := make([]*agenteamv1.ToolCallInfo, 0, len(r.toolCalls))
	for _, c := range r.toolCalls {
		out = append(out, proto.Clone(c).(*agenteamv1.ToolCallInfo))
	}
	return out
}

// typewrite 把已经拿到的完整文本按 size 个字符一段、间隔 interval 地推送给 send，
// 模拟流式打字机效果；用于"内容已通过一次非流式调用生成完毕，只需要以流式 UI 呈现"的场景。
func typewrite(ctx context.Context, text string, size int, interval time.Duration, send func(string) error) error {
	runes := []rune(text)
	for i := 0; i < len(runes); i += size {
		end := min(i+size, len(runes))
		if err := send(string(runes[i:end])); err != nil {
			return err
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// truncateRunes 按字符数（而非字节数）截断，超出时以省略号结尾，避免截断到多字节字符中间。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
