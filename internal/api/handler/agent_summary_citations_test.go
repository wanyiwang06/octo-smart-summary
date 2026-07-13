package handler

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func TestMapToMessage(t *testing.T) {
	m := map[string]interface{}{
		"sender_name": "Alice",
		"content":     "Test message",
		"send_time":   "2024-01-01T10:00:00Z",
		"timestamp":   float64(1704103200),
		"channel_id":  "channel-1",
		"message_seq": float64(100),
	}

	msg := mapToMessage(m, "peek_channel")
	if msg == nil {
		t.Fatal("expected non-nil message")
	}
	if msg.SenderName != "Alice" {
		t.Errorf("expected SenderName=Alice, got %s", msg.SenderName)
	}
	if msg.Content != "Test message" {
		t.Errorf("expected Content='Test message', got %s", msg.Content)
	}
	if msg.MessageSeq != 100 {
		t.Errorf("expected MessageSeq=100, got %d", msg.MessageSeq)
	}
}

func TestMapToMessage_InvalidData(t *testing.T) {
	// Missing content
	m1 := map[string]interface{}{
		"sender_name": "Alice",
	}
	if mapToMessage(m1, "test") != nil {
		t.Error("expected nil for message without content")
	}

	// Empty content
	m2 := map[string]interface{}{
		"content": "",
	}
	if mapToMessage(m2, "test") != nil {
		t.Error("expected nil for message with empty content")
	}
}

func TestMsgKey(t *testing.T) {
	msg := pipeline.Message{
		ChannelID:  "channel-1",
		MessageSeq: 12345,
	}
	key := msgKey(msg)
	expected := "channel-1:12345"
	if key != expected {
		t.Errorf("expected key=%s, got %s", expected, key)
	}
}

func TestMsgKey_EmptyChannel(t *testing.T) {
	msg := pipeline.Message{
		ChannelID:  "",
		MessageSeq: 100,
	}
	key := msgKey(msg)
	expected := ":100"
	if key != expected {
		t.Errorf("expected key=%s, got %s", expected, key)
	}
}
