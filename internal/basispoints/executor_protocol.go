package basispoints

import "bytes"

// CPA 将 Claude 请求转换为 codex 后，OriginalRequest 仍是原始 Messages 正文。
// 原生 Responses 保留原文路径，避免丢失客户端附加工具和回放元数据。
func executorSource(request ExecutorRequest) (map[string]any, error) {
	body := request.OriginalRequest
	if request.SourceFormat == "codex" || len(body) == 0 {
		body = request.Payload
	}
	return rawObject(body)
}

// CPA 的 codex 非流式转换器接收终态事件，不接收裸 Responses 对象。
func codexTerminalResponse(response map[string]any) []byte {
	event := "response.completed"
	if response["status"] == "incomplete" {
		event = "response.incomplete"
	}
	return jsonBytes(map[string]any{"type": event, "response": response})
}

func executorStreamPayloads(format string, response map[string]any) [][]byte {
	payload := syntheticStream(response)
	if format != "codex" {
		return [][]byte{payload}
	}
	// 宿主每次回调只翻译一个 data 事件；整段 SSE 会被转换器忽略。
	var frames [][]byte
	for _, line := range bytes.Split(payload, []byte{10}) {
		if bytes.HasPrefix(line, []byte("data: ")) && !bytes.Equal(line, []byte("data: [DONE]")) {
			frames = append(frames, bytes.Clone(line))
		}
	}
	return frames
}
