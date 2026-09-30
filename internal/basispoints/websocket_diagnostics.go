package basispoints

import (
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// 仅记录当前 WS 尝试的进度；不保留正文、工具参数或上游自定义标识。
type websocketProgress struct {
	started       time.Time
	lastEventAt   time.Time
	lastEvent     string
	events        int
	receivedBytes int
}

func (p *websocketProgress) record(event string, size int) {
	p.lastEventAt = time.Now()
	p.events++
	p.receivedBytes += size
	// 即使符合 event 命名习惯，自定义 type 也可能携带敏感内容，必须精确匹配。
	switch event {
	case "response.created", "response.in_progress",
		"response.output_item.added", "response.output_item.done",
		"response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done",
		"response.completed", "response.incomplete", "response.failed", "response.cancelled", "error":
		p.lastEvent = event
	default:
		p.lastEvent = "other"
	}
}

func (p *websocketProgress) interrupted(err error, delivery *streamDelivery) error {
	message := "Basis Points WebSocket closed before a terminal response; not replayed over HTTP"
	var closed *websocket.CloseError
	if errors.As(err, &closed) {
		// 关闭原因和底层错误文本可能包含自由文本、地址或凭据，不直接回显。
		message += fmt.Sprintf(" (close_code=%d)", closed.Code)
	}
	now := time.Now()
	message += fmt.Sprintf(" (elapsed_ms=%d; event_idle_ms=%d; events=%d; received_bytes=%d; last_event=%s; delivery_started=%t)",
		now.Sub(p.started).Milliseconds(), now.Sub(p.lastEventAt).Milliseconds(),
		p.events, p.receivedBytes, p.lastEvent, delivery != nil && delivery.committed)
	return fail(502, "upstream_ws_interrupted", message)
}
