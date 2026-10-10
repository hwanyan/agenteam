package a2a

import (
	"encoding/json"
	"testing"
)

func parseStreamEvent(t *testing.T, raw string) *streamEvent {
	t.Helper()
	var e streamEvent
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &e
}

func TestToolEventParsedFromFlatStyleStatusUpdate(t *testing.T) {
	e := parseStreamEvent(t, `{"kind":"status-update","status":{"state":"working"},
		"metadata":{"toolEvent":{"phase":"start","name":"get_price","arguments":"{\"market\":\"XAU\"}"}}}`)

	tool := e.toolEvent()
	if tool == nil || tool.Phase != ToolPhaseStart || tool.Name != "get_price" || tool.Arguments != `{"market":"XAU"}` {
		t.Fatalf("unexpected tool event: %+v", tool)
	}
	// 工具事件不带文本，不能被误当成回复正文。
	if e.extractText() != "" {
		t.Fatalf("tool event must not produce reply text, got %q", e.extractText())
	}
	if e.isTerminal() {
		t.Fatal("a working status update must not end the stream")
	}
}

func TestToolEventParsedFromNestedStyleStatusUpdate(t *testing.T) {
	e := parseStreamEvent(t, `{"statusUpdate":{"status":{"state":"working"},
		"metadata":{"toolEvent":{"phase":"success","name":"get_price"}}}}`)

	tool := e.toolEvent()
	if tool == nil || tool.Phase != ToolPhaseSuccess || tool.Name != "get_price" {
		t.Fatalf("unexpected tool event: %+v", tool)
	}
}

func TestToolEventIgnoresMalformedOrUnknown(t *testing.T) {
	cases := map[string]string{
		"no metadata":   `{"kind":"status-update","status":{"state":"working"}}`,
		"no name":       `{"metadata":{"toolEvent":{"phase":"start"}}}`,
		"unknown phase": `{"metadata":{"toolEvent":{"phase":"weird","name":"x"}}}`,
		"wrong shape":   `{"metadata":{"toolEvent":"oops"}}`,
		"other keys":    `{"metadata":{"something":{"phase":"start","name":"x"}}}`,
	}
	for name, raw := range cases {
		if tool := parseStreamEvent(t, raw).toolEvent(); tool != nil {
			t.Errorf("%s: want nil, got %+v", name, tool)
		}
	}
}
