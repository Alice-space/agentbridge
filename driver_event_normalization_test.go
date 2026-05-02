package agentbridge

import (
	"encoding/json"
	"testing"
)

func TestCodexNotificationSuppressesAgentMessageDeltas(t *testing.T) {
	if event, ok := parseCodexNotification(testRPCNotification(t, "item/agentMessage/delta", map[string]any{
		"threadId": "thread",
		"turnId":   "turn",
		"delta":    "hel",
	})); ok {
		t.Fatalf("delta event = %#v, want suppressed", event)
	}

	event, ok := parseCodexNotification(testRPCNotification(t, "item/completed", map[string]any{
		"threadId": "thread",
		"turnId":   "turn",
		"item": map[string]any{
			"type": "agentMessage",
			"text": "hello",
		},
	}))
	if !ok {
		t.Fatal("completed agentMessage was suppressed")
	}
	if event.Kind != TurnEventAssistantText || event.Text != "hello" {
		t.Fatalf("event = %#v, want assistant_text hello", event)
	}
}

func TestKimiNotificationCoalescesContentParts(t *testing.T) {
	driver := newKimiWireDriver(KimiConfig{})
	driver.threadID = "thread"
	driver.activeID = "turn"

	started := driver.parseKimiNotification(testKimiEvent(t, "TurnBegin", map[string]any{
		"user_input": "go",
	}))
	if len(started) != 1 || started[0].Kind != TurnEventStarted {
		t.Fatalf("started events = %#v, want one turn_started", started)
	}

	if events := driver.parseKimiNotification(testKimiEvent(t, "ContentPart", map[string]any{"text": "hel"})); len(events) != 0 {
		t.Fatalf("first content events = %#v, want suppressed fragment", events)
	}
	if events := driver.parseKimiNotification(testKimiEvent(t, "ContentPart", map[string]any{"text": "lo"})); len(events) != 0 {
		t.Fatalf("second content events = %#v, want suppressed fragment", events)
	}

	ended := driver.parseKimiNotification(testKimiEvent(t, "TurnEnd", map[string]any{"status": "completed"}))
	if len(ended) != 2 {
		t.Fatalf("ended events = %#v, want assistant text and completion", ended)
	}
	if ended[0].Kind != TurnEventAssistantText || ended[0].Text != "hello" {
		t.Fatalf("assistant event = %#v, want coalesced hello", ended[0])
	}
	if ended[1].Kind != TurnEventCompleted {
		t.Fatalf("completion event = %#v, want turn_completed", ended[1])
	}
}

func testKimiEvent(t *testing.T, eventType string, payload map[string]any) rpcNotification {
	t.Helper()
	return testRPCNotification(t, "event", map[string]any{
		"type":    eventType,
		"payload": payload,
	})
}

func testRPCNotification(t *testing.T, method string, params any) rpcNotification {
	t.Helper()
	rawParams, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"method": method,
		"params": params,
	})
	if err != nil {
		t.Fatalf("marshal raw notification: %v", err)
	}
	return rpcNotification{Method: method, Params: rawParams, Raw: string(raw)}
}
