package thirdpartypromptaudit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

type InputSnapshot struct {
	Protocol string         `json:"protocol"`
	Fields   map[string]any `json:"fields,omitempty"`
	NonText  []NonTextInput `json:"non_text"`
	RawBody  []byte         `json:"raw_body_base64,omitempty"`
}

// UnmarshalJSON 防止从数据库恢复时把工具参数中的大整数转成 float64。
func (s *InputSnapshot) UnmarshalJSON(raw []byte) error {
	type plain InputSnapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode((*plain)(s))
}

type NonTextInput struct {
	SourcePath string `json:"source_path"`
	Type       string `json:"type"`
}

type SegmentMeta struct {
	Order           int    `json:"order"`
	SourcePath      string `json:"source_path"`
	SourceRole      string `json:"source_role"`
	PolicyRole      string `json:"policy_role"`
	TurnScope       string `json:"turn_scope"`
	Selected        bool   `json:"selected"`
	SelectionKind   string `json:"selection_kind,omitempty"`
	SelectionReason string `json:"selection_reason,omitempty"`
}

type TextBlock struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	SourcePath string `json:"source_path"`
}

type Segment struct {
	SegmentMeta
	Content []TextBlock `json:"content"`
}

type protocolMessage struct {
	role, path, contextPath string
	content                 any
	toolData                any
	blockOffset             int
}

// CaptureInput 只在协议确定的内容块位置处理二进制，业务工具对象不参与媒体猜测。
func CaptureInput(protocol string, body []byte) (*InputSnapshot, error) {
	snapshot := &InputSnapshot{Protocol: protocol, NonText: []NonTextInput{}}
	if !utf8.Valid(body) {
		snapshot.RawBody = append([]byte(nil), body...)
		return snapshot, errors.New("请求不是有效 UTF-8，已保留原始字节")
	}
	value, err := decodeUniqueJSON(body)
	if err != nil {
		snapshot.RawBody = append([]byte(nil), body...)
		return snapshot, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		snapshot.RawBody = append([]byte(nil), body...)
		return snapshot, errors.New("请求必须是结构化 JSON 对象")
	}
	snapshot.Fields = object
	messages, err := protocolMessages(protocol, object, "$")
	if err != nil {
		return snapshot, err
	}
	for _, message := range messages {
		// 工具输出的内容数组属于协议；字符串与普通工具对象仍完整保留。
		if output, ok := message.toolData.(map[string]any); ok {
			kind, _ := output["type"].(string)
			if kind == "function_call_output" || kind == "tool_result" {
				if blocks, ok := output["output"].([]any); ok {
					scrubContentBinary(blocks, message.path+".output", &snapshot.NonText)
				}
			}
		}
		if blocks, ok := message.content.([]any); ok {
			for i, block := range blocks {
				scrubContentBinary(block, fmt.Sprintf("%s[%d]", message.path, message.blockOffset+i), &snapshot.NonText)
			}
		} else {
			scrubContentBinary(message.content, message.path, &snapshot.NonText)
		}
	}
	if slices.Contains([]string{"openai_images", "grok_media", "media", "images"}, strings.ToLower(protocol)) {
		for _, key := range []string{"image", "images", "mask", "audio", "video"} {
			if value, exists := object[key]; exists {
				snapshot.NonText = append(snapshot.NonText, NonTextInput{SourcePath: "$." + key, Type: key})
				object[key] = scrubMediaField(value)
			}
		}
	}
	return snapshot, nil
}

// scrubMediaField 仅处理媒体协议的已知载荷字段，保留 URL、格式及引用信息。
func scrubMediaField(value any) any {
	switch node := value.(type) {
	case string:
		if strings.HasPrefix(node, "https://") || strings.HasPrefix(node, "http://") {
			return node
		}
		return nil
	case []any:
		for i, item := range node {
			node[i] = scrubMediaField(item)
		}
	case map[string]any:
		for _, key := range []string{"data", "base64", "b64_json", "bytesBase64Encoded"} {
			if _, exists := node[key]; exists {
				node[key] = nil
			}
		}
		for _, key := range []string{"url", "image_url"} {
			if location, ok := node[key].(string); ok && strings.HasPrefix(location, "data:") {
				node[key] = nil
			}
		}
	}
	return value
}

func scrubContentBinary(value any, path string, nonText *[]NonTextInput) {
	switch content := value.(type) {
	case []any:
		for i, block := range content {
			scrubContentBinary(block, fmt.Sprintf("%s[%d]", path, i), nonText)
		}
	case map[string]any:
		kind, _ := content["type"].(string)
		if kind == "compaction" || kind == "encrypted_content" {
			*nonText = append(*nonText, NonTextInput{SourcePath: path, Type: kind})
			content["encrypted_content"] = nil
			return
		}
		if kind == "reasoning" {
			if _, exists := content["encrypted_content"]; exists {
				*nonText = append(*nonText, NonTextInput{SourcePath: path + ".encrypted_content", Type: "encrypted_content"})
				content["encrypted_content"] = nil
			}
			scrubContentBinary(content["summary"], path+".summary", nonText)
			return
		}
		if kind == "tool_result" {
			scrubContentBinary(content["content"], path+".content", nonText)
			return
		}
		if kind == "text" || kind == "input_text" || kind == "output_text" || kind == "tool_use" {
			return
		}
		media := slices.Contains([]string{"image", "image_url", "input_image", "input_audio", "audio", "video", "input_video", "file", "input_file", "document"}, kind)
		if media {
			source, _ := content["source"].(map[string]any)
			sourceType, _ := source["type"].(string)
			// 文本型 document 仍是完整文本，不能按 document 一概省略。
			if kind == "document" && sourceType == "text" {
				return
			}
			*nonText = append(*nonText, NonTextInput{SourcePath: path, Type: kind})
			if sourceType == "base64" {
				source["data"] = nil
			}
			for _, key := range []string{"image_url", "video_url"} {
				switch location := content[key].(type) {
				case string:
					if strings.HasPrefix(location, "data:") {
						content[key] = nil
					}
				case map[string]any:
					if url, ok := location["url"].(string); ok && strings.HasPrefix(url, "data:") {
						location["url"] = nil
					}
				}
			}
			if audio, ok := content["input_audio"].(map[string]any); ok {
				audio["data"] = nil
			}
			if _, exists := content["file_data"]; exists {
				content["file_data"] = nil
			}
			return
		}
		for _, key := range []string{"inlineData", "inline_data"} {
			if inline, ok := content[key].(map[string]any); ok {
				*nonText = append(*nonText, NonTextInput{SourcePath: path + "." + key, Type: "inline_data"})
				inline["data"] = nil
			}
		}
		for _, key := range []string{"fileData", "file_data"} {
			if _, exists := content[key]; exists {
				*nonText = append(*nonText, NonTextInput{SourcePath: path + "." + key, Type: "file_data"})
			}
		}
		// systemInstruction 的 parts 是协议内容块；函数参数和任意嵌套对象不向下扫描。
		if parts, ok := content["parts"]; ok {
			scrubContentBinary(parts, path+".parts", nonText)
		}
	}
}

// protocolMessages 统一捕获和审核的协议边界，来源角色只由实际协议字段决定。
func protocolMessages(protocol string, root map[string]any, prefix string) ([]protocolMessage, error) {
	protocol = strings.ToLower(protocol)
	messages := make([]protocolMessage, 0)
	switch protocol {
	case "responses_websocket", "openai_responses", "responses":
		kind, _ := root["type"].(string)
		if kind != "" || protocol == "responses_websocket" {
			if kind != "response.create" {
				return nil, errors.New("不是可审核的 response.create 输入")
			}
			if _, exists := root["input"]; !exists {
				if nested, ok := root["response"].(map[string]any); ok {
					root, prefix = nested, prefix+".response"
				}
			}
		}
		if value, exists := root["instructions"]; exists {
			messages = append(messages, protocolMessage{role: "system", path: prefix + ".instructions", contextPath: prefix, content: value})
		}
		input := root["input"]
		if text, ok := input.(string); ok {
			return append(messages, protocolMessage{role: "user", path: prefix + ".input", contextPath: prefix, content: text}), nil
		}
		var items []any
		switch value := input.(type) {
		case nil:
		case []any:
			items = value
		case map[string]any:
			items = []any{value}
		default:
			return nil, errors.New("Responses input 类型无效")
		}
		for i, item := range items {
			path := fmt.Sprintf("%s.input[%d]", prefix, i)
			if _, single := input.(map[string]any); single {
				path = prefix + ".input"
			}
			if text, ok := item.(string); ok {
				messages = append(messages, protocolMessage{role: "user", path: path, contextPath: prefix, content: text})
				continue
			}
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("消息对象无效: %s", path)
			}
			role, _ := entry["role"].(string)
			kind, _ := entry["type"].(string)
			content := entry["content"]
			contentPath := path + ".content"
			var toolData any
			switch kind {
			case "additional_tools":
				if _, ok := entry["tools"].([]any); !ok {
					return nil, fmt.Errorf("Responses additional_tools 缺少工具数组: %s", path)
				}
				continue
			case "compaction":
				role, content = "assistant", entry
				contentPath = path
			case "agent_message":
				role = "assistant"
			case "function_call_output", "tool_result", "custom_tool_call_output":
				role, content, toolData = "tool", nil, entry
				contentPath = path
			case "function_call", "tool_call", "custom_tool_call":
				role, content, toolData = "assistant", nil, entry
				contentPath = path
			case "reasoning":
				role, content = "assistant", entry
				contentPath = path
			default:
				if role == "" {
					role = "user"
				}
				if content == nil {
					content = entry
					contentPath = path
				}
			}
			messages = append(messages, protocolMessage{role: role, path: contentPath, contextPath: prefix, content: content, toolData: toolData})
		}
	case "openai_chat_completions", "openai_chat", "chat_completions", "anthropic_messages", "claude_messages", "messages":
		if value, exists := root["system"]; exists {
			messages = append(messages, protocolMessage{role: "system", path: prefix + ".system", contextPath: prefix, content: value})
		}
		items, ok := root["messages"].([]any)
		if !ok {
			return nil, errors.New("messages 必须是消息数组")
		}
		for i, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("消息对象无效")
			}
			path := fmt.Sprintf("%s.messages[%d].content", prefix, i)
			role, _ := entry["role"].(string)
			content := entry["content"]
			// 工具外层载荷的字段属于业务数据，不能按媒体 type 改写。
			if role == "tool" {
				messages = append(messages, protocolMessage{role: role, path: path, contextPath: prefix, toolData: entry})
				continue
			}
			if blocks, ok := content.([]any); ok {
				pending := make([]any, 0)
				pendingStart := 0
				flush := func() {
					if len(pending) > 0 {
						messages = append(messages, protocolMessage{role: role, path: path, contextPath: prefix, content: pending, blockOffset: pendingStart})
						pending = nil
					}
				}
				for j, block := range blocks {
					object, _ := block.(map[string]any)
					kind, _ := object["type"].(string)
					if kind == "tool_result" {
						flush()
						messages = append(messages, protocolMessage{role: "tool", path: fmt.Sprintf("%s[%d]", path, j), contextPath: prefix, content: block})
					} else {
						if len(pending) == 0 {
							pendingStart = j
						}
						pending = append(pending, block)
					}
				}
				flush()
			} else {
				messages = append(messages, protocolMessage{role: role, path: path, contextPath: prefix, content: content})
			}
			for _, key := range []string{"tool_calls", "function_call"} {
				if value, exists := entry[key]; exists {
					messages = append(messages, protocolMessage{role: role, path: fmt.Sprintf("%s.messages[%d].%s", prefix, i, key), contextPath: prefix, toolData: value})
				}
			}
		}
	case "gemini", "gemini_generate_content":
		for _, key := range []string{"systemInstruction", "system_instruction"} {
			if value, exists := root[key]; exists {
				messages = append(messages, protocolMessage{role: "system", path: prefix + "." + key, contextPath: prefix, content: value})
			}
		}
		for _, key := range []string{"contents", "content"} {
			var items []any
			switch value := root[key].(type) {
			case []any:
				items = value
			case map[string]any:
				items = []any{value}
			case nil:
			default:
				return nil, errors.New("Gemini contents 类型无效")
			}
			for i, item := range items {
				entry, ok := item.(map[string]any)
				if !ok {
					return nil, errors.New("Gemini content 对象无效")
				}
				role, _ := entry["role"].(string)
				if role == "" {
					role = "user"
				}
				parts, ok := entry["parts"].([]any)
				if !ok {
					return nil, errors.New("Gemini parts 必须是数组")
				}
				pending := make([]any, 0)
				pendingStart := 0
				path := fmt.Sprintf("%s.%s[%d].parts", prefix, key, i)
				if _, single := root[key].(map[string]any); single {
					path = prefix + "." + key + ".parts"
				}
				flush := func() {
					if len(pending) > 0 {
						messages = append(messages, protocolMessage{role: role, path: path, contextPath: prefix, content: pending, blockOffset: pendingStart})
						pending = nil
					}
				}
				for j, part := range parts {
					object, _ := part.(map[string]any)
					if _, exists := object["functionResponse"]; exists {
						flush()
						messages = append(messages, protocolMessage{role: "tool", path: fmt.Sprintf("%s[%d]", path, j), contextPath: prefix, toolData: part})
					} else {
						if len(pending) == 0 {
							pendingStart = j
						}
						pending = append(pending, part)
					}
				}
				flush()
			}
		}
		if instances, ok := root["instances"].([]any); ok {
			for i, item := range instances {
				if instance, ok := item.(map[string]any); ok {
					if value, exists := instance["prompt"]; exists {
						messages = append(messages, protocolMessage{role: "user", path: fmt.Sprintf("%s.instances[%d].prompt", prefix, i), contextPath: fmt.Sprintf("%s.instances[%d]", prefix, i), content: value})
					}
				}
			}
		}
		if requests, ok := root["requests"].([]any); ok {
			for i, item := range requests {
				request, ok := item.(map[string]any)
				if !ok {
					return nil, errors.New("Gemini 批处理子请求无效")
				}
				nested, err := protocolMessages(protocol, request, fmt.Sprintf("%s.requests[%d]", prefix, i))
				if err != nil {
					return nil, err
				}
				messages = append(messages, nested...)
			}
		}
	case "openai_embeddings":
		switch input := root["input"].(type) {
		case string:
			messages = append(messages, protocolMessage{role: "user", path: prefix + ".input", contextPath: prefix, content: input})
		case []any:
			for i, item := range input {
				if text, ok := item.(string); ok {
					messages = append(messages, protocolMessage{role: "user", path: fmt.Sprintf("%s.input[%d]", prefix, i), contextPath: prefix, content: text})
				}
			}
		}
	case "openai_alpha_search":
		if _, exists := root["messages"]; exists {
			return protocolMessages("openai_chat_completions", root, prefix)
		}
		if _, exists := root["input"]; exists {
			return protocolMessages("openai_responses", root, prefix)
		}
		collectMediaText(root, prefix, &messages)
	case "openai_images", "grok_media", "media", "images":
		collectMediaText(root, prefix, &messages)
	default:
		return nil, fmt.Errorf("未支持的审核输入协议: %s", protocol)
	}
	return messages, nil
}

func collectMediaText(value any, path string, messages *[]protocolMessage) {
	switch node := value.(type) {
	case []any:
		for i, item := range node {
			collectMediaText(item, fmt.Sprintf("%s[%d]", path, i), messages)
		}
	case map[string]any:
		// JSON 对象没有消息顺序，按稳定键顺序提取；数组顺序仍保留。
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
			if slices.Contains([]string{"prompt", "inputprompt", "textprompt", "description", "query", "lyrics", "negativeprompt", "positiveprompt", "gptdescriptionprompt", "prompten", "finalprompt", "finalzhprompt", "origprompt", "actualprompt", "imageprompt", "input", "text"}, normalized) {
				if text, ok := node[key].(string); ok && !strings.HasPrefix(text, "data:") {
					*messages = append(*messages, protocolMessage{role: "user", path: path + "." + key, contextPath: "$", content: text})
					continue
				}
			}
			collectMediaText(node[key], path+"."+key, messages)
		}
	}
}

// ExtractSegments 保留工具结果来源，当前轮次由真实用户文本而非工具包装决定。
func ExtractSegments(snapshot *InputSnapshot, scope string) ([]Segment, error) {
	if snapshot == nil || snapshot.Fields == nil || len(snapshot.RawBody) != 0 {
		return nil, errors.New("完整结构化输入不可用")
	}
	if scope != "full_request" && scope != "current_turn" {
		return nil, errors.New("审核范围无效")
	}
	messages, err := protocolMessages(snapshot.Protocol, snapshot.Fields, "$")
	if err != nil {
		return nil, err
	}
	segments := make([]Segment, 0)
	contexts := make([]string, 0)
	messageIDs := make([]string, 0)
	for _, message := range messages {
		if !slices.Contains([]string{"system", "developer", "user", "assistant", "model", "tool"}, message.role) {
			return nil, fmt.Errorf("协议角色无法识别: %q", message.role)
		}
		var blocks []TextBlock
		var err error
		if content, ok := message.content.([]any); ok {
			for i, item := range content {
				var next []TextBlock
				next, err = extractTextBlocks(item, fmt.Sprintf("%s[%d]", message.path, message.blockOffset+i))
				if err != nil {
					break
				}
				blocks = append(blocks, next...)
			}
		} else {
			blocks, err = extractTextBlocks(message.content, message.path)
		}
		if err != nil {
			return nil, err
		}
		if message.toolData != nil {
			raw, err := json.Marshal(message.toolData)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, TextBlock{Type: "tool_data", Text: string(raw), SourcePath: message.path})
		}
		if len(blocks) == 0 {
			continue
		}
		policyRole := message.role
		if policyRole == "model" {
			policyRole = "assistant"
		}
		segments = append(segments, Segment{SegmentMeta: SegmentMeta{Order: len(segments) + 1, SourcePath: message.path, SourceRole: message.role, PolicyRole: policyRole}, Content: blocks})
		contexts = append(contexts, message.contextPath)
		identity := message.path
		for _, field := range []string{".messages[", ".contents[", ".input["} {
			if start := strings.LastIndex(message.path, field); start >= 0 {
				if end := strings.IndexByte(message.path[start:], ']'); end >= 0 {
					identity = message.path[:start+end+1]
					break
				}
			}
		}
		messageIDs = append(messageIDs, identity)
	}
	starts := make(map[string]int)
	for i, segment := range segments {
		if segment.SourceRole == "user" {
			starts[contexts[i]] = i
		}
	}
	for group, start := range starts {
		// 一条用户消息内部可交错携带工具结果，当前任务必须覆盖同一消息的全部角色块。
		identity := messageIDs[start]
		for start > 0 && contexts[start-1] == group && messageIDs[start-1] == identity {
			start--
		}
		for start > 0 && contexts[start-1] == group && segments[start-1].SourceRole == "user" {
			start--
		}
		starts[group] = start
	}
	for i := range segments {
		segment := &segments[i]
		start, hasUser := starts[contexts[i]]
		switch {
		case segment.SourceRole == "system" || segment.SourceRole == "developer":
			segment.TurnScope = "active"
		case !hasUser || i >= start:
			segment.TurnScope = "current"
		default:
			segment.TurnScope = "historical"
		}
		segment.Selected = scope == "full_request" || segment.TurnScope != "historical"
	}
	return segments, nil
}

func extractTextBlocks(value any, path string) ([]TextBlock, error) {
	blocks := make([]TextBlock, 0)
	switch content := value.(type) {
	case nil:
		return blocks, nil
	case string:
		return []TextBlock{{Type: "text", Text: content, SourcePath: path}}, nil
	case []any:
		for i, item := range content {
			nested, err := extractTextBlocks(item, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, nested...)
		}
		return blocks, nil
	case map[string]any:
		kind, _ := content["type"].(string)
		if kind == "tool_result" || kind == "tool_use" {
			raw, err := json.Marshal(content)
			if err != nil {
				return nil, err
			}
			return []TextBlock{{Type: kind, Text: string(raw), SourcePath: path}}, nil
		}
		if kind == "document" {
			source, _ := content["source"].(map[string]any)
			if sourceType, _ := source["type"].(string); sourceType == "text" {
				return extractTextBlocks(source["data"], path+".source.data")
			}
			return blocks, nil
		}
		if slices.Contains([]string{"image", "image_url", "input_image", "input_audio", "audio", "video", "input_video", "file", "input_file", "redacted_thinking", "item_reference", "compaction", "encrypted_content"}, kind) {
			return blocks, nil
		}
		for _, key := range []string{"text", "thinking", "refusal", "content", "parts", "summary"} {
			if child, exists := content[key]; exists {
				return extractTextBlocks(child, path+"."+key)
			}
		}
		for _, key := range []string{"functionCall", "functionResponse", "arguments", "input", "output"} {
			if _, exists := content[key]; exists {
				raw, err := json.Marshal(content)
				if err != nil {
					return nil, err
				}
				return []TextBlock{{Type: "tool_data", Text: string(raw), SourcePath: path}}, nil
			}
		}
		for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
			if _, exists := content[key]; exists {
				return blocks, nil
			}
		}
		return nil, fmt.Errorf("未支持的文本内容结构: %s", path)
	default:
		return nil, fmt.Errorf("文本内容类型无效: %s", path)
	}
}
