package thirdpartypromptaudit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// 审核目标类型：单个稳定指令块，或当前回合合并后的用户提示词。
const (
	PromptKindInstruction = "instruction"
	PromptKindUser        = "user"
)

// ErrUnsupportedProtocol 表示入口无法按已知协议识别审核目标。
var ErrUnsupportedProtocol = errors.New("未支持的审核输入协议")

// ErrNotAuditable 表示该消息不是生成请求（例如 WebSocket 的会话更新），不需要审核。
var ErrNotAuditable = errors.New("不是可审核的生成请求")

// PromptText 是内容寻址的审核文本；Hash 同时覆盖目标类型和规范化后的正文。
type PromptText struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	Hash string `json:"hash"`
}

// ParsedRequest 是入口一次解析得到的全部审核信息，不携带请求原文。
type ParsedRequest struct {
	Protocol           string
	Model              string
	ClientRules        string
	Instructions       []PromptText
	UserPrompt         *PromptText
	Continuation       bool
	PreviousResponseID string
	TurnFingerprint    string
	// StrippedSegments 记录按严格模板剔除的易变片段名称，只用于测试和排障日志。
	StrippedSegments []string
}

// Targets 按展示顺序返回本请求的全部审核目标：指令块在前，用户提示词在后。
func (p *ParsedRequest) Targets() []PromptText {
	targets := append([]PromptText(nil), p.Instructions...)
	if p.UserPrompt != nil {
		targets = append(targets, *p.UserPrompt)
	}
	return targets
}

func newPromptText(kind, text string) PromptText {
	// 入库与送审前统一掩码高置信密钥形态：既不落库，也不发给第三方审核模型。
	text = maskSecrets(text)
	digest := sha256.Sum256([]byte(kind + "\x00" + text))
	return PromptText{Kind: kind, Text: text, Hash: hex.EncodeToString(digest[:])}
}

// secretPatterns 覆盖常见凭据形态；只替换密钥本体，保留可读上下文。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{16,}`),
	regexp.MustCompile(`(?i)(postgres(?:ql)?|mysql|redis|mongodb)://[^\s"']{8,}`),
	regexp.MustCompile(`(?i)password\s*[=:]\s*[^\s"',]{6,}`),
}

// maskSecrets 把疑似凭据替换为固定占位符，长度信息通过占位符数量体现。
func maskSecrets(text string) string {
	for _, pattern := range secretPatterns {
		text = pattern.ReplaceAllStringFunc(text, func(match string) string {
			if strings.Contains(strings.ToLower(match), "://") {
				if index := strings.Index(match, "://"); index >= 0 {
					return match[:index+3] + "[REDACTED]"
				}
			}
			if index := strings.IndexAny(match, "=:"); index >= 0 && strings.HasPrefix(strings.ToLower(match), "password") {
				return match[:index+1] + " [REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	return text
}

// normalizePromptText 统一换行并去掉首尾空白，保证同一内容得到同一哈希；
// NUL 字符无法写入 PostgreSQL TEXT，统一替换为 U+FFFD，使哈希与入库正文一致。
func normalizePromptText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.ReplaceAll(text, "\x00", "\uFFFD")
	return strings.TrimSpace(strings.ToValidUTF8(text, "\uFFFD"))
}

// turnFingerprint 由有序指令块哈希和用户提示词哈希组成，回合键据此去重和绑定续跑。
func turnFingerprint(instructions []PromptText, user *PromptText) string {
	hashes := make([]string, 0, len(instructions))
	for _, item := range instructions {
		hashes = append(hashes, item.Hash)
	}
	userHash := ""
	if user != nil {
		userHash = user.Hash
	}
	raw, _ := json.Marshal(struct {
		Instructions []string `json:"instructions"`
		User         string   `json:"user"`
	}{hashes, userHash})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// 规则集名称；客户端指纹只用于选择规则集，安全边界由严格模板保证。
const (
	ClientClaudeCode = "claude_code"
	ClientCodex      = "codex"
	ClientGeminiCLI  = "gemini_cli"
	ClientGeneric    = "generic"
)

// DetectClient 按请求头选择易变片段规则集，无法识别时使用通用规则集（不剔除任何内容）。
func DetectClient(header http.Header) string {
	agent := header.Get("User-Agent")
	switch {
	case strings.HasPrefix(agent, "claude-cli/"):
		return ClientClaudeCode
	case strings.Contains(agent, "codex_cli_rs") || strings.HasPrefix(agent, "codex_") || header.Get("Originator") == "codex_cli_rs":
		return ClientCodex
	case strings.HasPrefix(agent, "GeminiCLI/"):
		return ClientGeminiCLI
	default:
		return ClientGeneric
	}
}

type itemRole int

const (
	roleInstruction itemRole = iota + 1
	roleUser
	roleAssistant
	roleTool
)

// promptPart 是一个协议内容块中的文本；media 表示该块是图片、音频、文件等非文本内容。
type promptPart struct {
	text  string
	media bool
}

// promptItem 是按协议顺序展开后的条目；同一条消息中的工具结果与文本会拆成相邻条目。
type promptItem struct {
	role  itemRole
	parts []promptPart
	// origin 标明指令文本的协议位置：system、developer、instructions 或 message。
	origin string
	// message 是条目所属会话消息的序号，用于“首条 user 消息”这类位置约束。
	message int
}

// ParsePrompt 从已解压的请求正文按协议类型化解码，不构造 map[string]any 树。
func ParsePrompt(protocol string, body io.Reader, client string) (*ParsedRequest, error) {
	decoder := json.NewDecoder(bufio.NewReaderSize(body, 64<<10))
	var items []promptItem
	var model, previous string
	var err error
	switch normalizeProtocol(protocol) {
	case "responses":
		items, model, previous, err = decodeResponses(decoder, protocol == "responses_websocket")
	case "messages":
		items, model, err = decodeMessages(decoder)
	case "gemini":
		items, model, err = decodeGemini(decoder)
	case "embeddings":
		items, model, err = decodeEmbeddings(decoder)
	case "media":
		items, model, err = decodeMedia(decoder)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProtocol, protocol)
	}
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON 对象后存在额外数据")
	}
	return buildParsedRequest(protocol, client, model, previous, items), nil
}

// ParsePromptTexts 处理 multipart 媒体上传中的文本字段，所有文本都属于当前回合用户提示词。
func ParsePromptTexts(protocol string, texts []string, client string) *ParsedRequest {
	parts := make([]promptPart, 0, len(texts))
	for _, text := range texts {
		parts = append(parts, promptPart{text: text})
	}
	return buildParsedRequest(protocol, client, "", "", []promptItem{{role: roleUser, parts: parts, origin: "message"}})
}

func normalizeProtocol(protocol string) string {
	switch strings.ToLower(protocol) {
	case "responses", "responses_websocket", "openai_responses":
		return "responses"
	case "chat_completions", "openai_chat_completions", "openai_chat", "anthropic_messages", "claude_messages", "messages":
		return "messages"
	case "gemini", "gemini_generate_content":
		return "gemini"
	case "openai_embeddings":
		return "embeddings"
	case "openai_alpha_search", "openai_images", "grok_media", "media", "images":
		return "media"
	default:
		return ""
	}
}

// buildParsedRequest 先按严格模板处理文本，再判定首次/续跑并计算回合指纹。
func buildParsedRequest(protocol, client, model, previous string, items []promptItem) *ParsedRequest {
	result := &ParsedRequest{Protocol: protocol, Model: model, ClientRules: client, PreviousResponseID: previous}
	rules := templateRulesFor(client)
	firstUser, contextEnd := -1, len(items)
	for i, item := range items {
		if item.role == roleUser && firstUser < 0 {
			firstUser = item.message
		}
		if (item.role == roleAssistant || item.role == roleTool) && contextEnd == len(items) {
			contextEnd = i
		}
	}
	type effectiveItem struct {
		role  itemRole
		texts []string
	}
	effective := make([]effectiveItem, 0, len(items))
	seen := make(map[string]bool)
	addInstruction := func(text string) {
		text = normalizePromptText(text)
		if text == "" {
			return
		}
		block := newPromptText(PromptKindInstruction, text)
		if !seen[block.Hash] {
			seen[block.Hash] = true
			result.Instructions = append(result.Instructions, block)
		}
	}
	for index, item := range items {
		switch item.role {
		case roleInstruction:
			for _, part := range item.parts {
				if part.media {
					continue
				}
				text, stripped, drop := applySystemTemplates(rules, part.text)
				result.StrippedSegments = append(result.StrippedSegments, stripped...)
				if !drop {
					addInstruction(text)
				}
			}
		case roleUser:
			position := templatePosition{firstUserMessage: item.message == firstUser, contextPrefix: index < contextEnd}
			var texts []string
			hasContent := false
			for _, part := range item.parts {
				if part.media {
					hasContent = true
					continue
				}
				outcome := applyUserTemplates(rules, part.text, position)
				result.StrippedSegments = append(result.StrippedSegments, outcome.stripped...)
				switch {
				case outcome.instruction:
					addInstruction(outcome.text)
				case outcome.drop:
				default:
					if text := normalizePromptText(outcome.text); text != "" {
						texts = append(texts, text)
						hasContent = true
					} else if strings.TrimSpace(part.text) != "" {
						// 剔除后为空说明整块是易变片段；原本为空的文本块不影响判定。
						continue
					}
				}
			}
			if hasContent {
				effective = append(effective, effectiveItem{role: roleUser, texts: texts})
			}
		default:
			effective = append(effective, effectiveItem{role: item.role})
		}
	}
	// 跳过末尾的 assistant 预填，再看最后一条有效条目。
	for len(effective) > 0 && effective[len(effective)-1].role == roleAssistant {
		effective = effective[:len(effective)-1]
	}
	end := len(effective) - 1
	if end >= 0 && effective[end].role == roleTool {
		result.Continuation = true
		end = -1
		for i := len(effective) - 1; i >= 0; i-- {
			if effective[i].role == roleUser {
				end = i
				break
			}
		}
	}
	if end >= 0 && effective[end].role == roleUser {
		var texts []string
		start := end
		for start > 0 && effective[start-1].role == roleUser {
			start--
		}
		for _, item := range effective[start : end+1] {
			texts = append(texts, item.texts...)
		}
		if len(texts) > 0 {
			user := newPromptText(PromptKindUser, strings.Join(texts, "\n\n"))
			result.UserPrompt = &user
		}
	}
	result.TurnFingerprint = turnFingerprint(result.Instructions, result.UserPrompt)
	return result
}

// discardValue 跳过不需要的字段；结构不匹配的类型错误只表示已跳过该值。
func discardValue(decoder *json.Decoder) error {
	var discard struct{}
	err := decoder.Decode(&discard)
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return nil
	}
	return err
}

// readObject 逐个读取顶层对象字段，只解码需要的字段，其余字段直接跳过。
func readObject(decoder *json.Decoder, field func(key string) error) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return errors.New("请求必须是结构化 JSON 对象")
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("JSON 对象键无效")
		}
		if err := field(key); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// readArray 逐个读取数组元素，每次只缓冲一个元素。
func readArray(decoder *json.Decoder, element func(index int) error) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return errors.New("字段必须是数组")
	}
	for index := 0; decoder.More(); index++ {
		if err := element(index); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// present 只记录字段存在，不复制字段内容（例如 base64 图片）。
type present bool

func (p *present) UnmarshalJSON([]byte) error { *p = true; return nil }

// contentPiece 是一个内容块的类型和文本；工具结果单独标记，便于拆分条目。
type contentPiece struct {
	text  string
	media bool
	tool  bool
	other bool
}

// contentValue 在解码时直接展开内容，不保留原始字节，适用于字符串或内容块数组。
type contentValue struct {
	pieces []contentPiece
	set    bool
}

type contentBlock struct {
	Type             string  `json:"type"`
	Text             *string `json:"text"`
	Source           present `json:"source"`
	ImageURL         present `json:"image_url"`
	InputAudio       present `json:"input_audio"`
	File             present `json:"file"`
	FunctionResponse present `json:"functionResponse"`
	FunctionCall     present `json:"functionCall"`
	InlineData       present `json:"inlineData"`
	InlineData2      present `json:"inline_data"`
	FileData         present `json:"fileData"`
	FileData2        present `json:"file_data"`
}

func (c *contentValue) UnmarshalJSON(raw []byte) error {
	c.set = true
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	switch first := raw[0]; {
	case first == '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		c.pieces = append(c.pieces, contentPiece{text: text})
		return nil
	case first == '[':
		// 每个内容块在解码回调中直接展开，不保留块的原始字节（例如 base64 图片）。
		var blocks []pieceValue
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return err
		}
		for _, block := range blocks {
			c.pieces = append(c.pieces, contentPiece(block))
		}
		return nil
	case first == '{':
		piece, err := decodeContentBlock(raw)
		if err != nil {
			return err
		}
		c.pieces = append(c.pieces, piece)
		return nil
	default:
		return errors.New("内容字段类型无效")
	}
}

// pieceValue 让数组元素在解码时直接转换为 contentPiece。
type pieceValue contentPiece

func (p *pieceValue) UnmarshalJSON(raw []byte) error {
	piece, err := decodeContentBlock(raw)
	*p = pieceValue(piece)
	return err
}

// decodeContentBlock 只解出块类型和文本；document 的文本来源需要二次读取 source。
func decodeContentBlock(raw []byte) (contentPiece, error) {
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return contentPiece{}, err
		}
		return contentPiece{text: text}, nil
	}
	var block contentBlock
	if err := json.Unmarshal(raw, &block); err != nil {
		return contentPiece{}, err
	}
	switch block.Type {
	case "tool_result", "function_call_output", "custom_tool_call_output":
		return contentPiece{tool: true}, nil
	case "tool_use", "thinking", "redacted_thinking", "server_tool_use", "web_search_tool_result":
		return contentPiece{other: true}, nil
	case "text", "input_text", "output_text":
		if block.Text != nil {
			return contentPiece{text: *block.Text}, nil
		}
		return contentPiece{other: true}, nil
	case "document":
		var document struct {
			Source struct {
				Type string `json:"type"`
			} `json:"source"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			return contentPiece{}, err
		}
		if document.Source.Type == "text" {
			var text struct {
				Source struct {
					Data string `json:"data"`
				} `json:"source"`
			}
			if err := json.Unmarshal(raw, &text); err != nil {
				return contentPiece{}, err
			}
			return contentPiece{text: text.Source.Data}, nil
		}
		return contentPiece{media: true}, nil
	case "image", "image_url", "input_image", "input_audio", "audio", "video", "input_video", "file", "input_file":
		return contentPiece{media: true}, nil
	}
	switch {
	case bool(block.FunctionResponse):
		return contentPiece{tool: true}, nil
	case bool(block.FunctionCall):
		return contentPiece{other: true}, nil
	case block.Text != nil:
		return contentPiece{text: *block.Text}, nil
	case bool(block.InlineData) || bool(block.InlineData2) || bool(block.FileData) || bool(block.FileData2) || bool(block.ImageURL) || bool(block.InputAudio) || bool(block.File) || bool(block.Source):
		return contentPiece{media: true}, nil
	}
	return contentPiece{other: true}, nil
}

// splitContent 把一条消息的内容按工具结果拆成相邻条目，保持协议顺序。
func splitContent(role itemRole, message int, origin string, pieces []contentPiece) []promptItem {
	items := make([]promptItem, 0, 1)
	var pending []promptPart
	flush := func() {
		if len(pending) > 0 {
			items = append(items, promptItem{role: role, parts: pending, origin: origin, message: message})
			pending = nil
		}
	}
	for _, piece := range pieces {
		switch {
		case piece.tool:
			flush()
			items = append(items, promptItem{role: roleTool, message: message})
		case piece.other:
		case piece.media:
			pending = append(pending, promptPart{media: true})
		default:
			pending = append(pending, promptPart{text: piece.text})
		}
	}
	flush()
	if len(items) == 0 && role != roleUser && role != roleInstruction {
		// assistant 条目即使没有文本也要保留位置，用于判定回合边界。
		items = append(items, promptItem{role: role, message: message})
	}
	return items
}

func roleOf(role string) (itemRole, string, bool) {
	switch role {
	case "system":
		return roleInstruction, "system", true
	case "developer":
		return roleInstruction, "developer", true
	case "user":
		return roleUser, "message", true
	case "assistant", "model":
		return roleAssistant, "", true
	case "tool", "function":
		return roleTool, "", true
	default:
		return 0, "", false
	}
}

// decodeMessages 覆盖 Chat Completions 与 Anthropic Messages：顶层 system 加 messages 数组。
func decodeMessages(decoder *json.Decoder) ([]promptItem, string, error) {
	var system, conversation []promptItem
	var model string
	seenMessages := false
	err := readObject(decoder, func(key string) error {
		switch key {
		case "model":
			return decodeOptionalString(decoder, &model)
		case "system":
			var content contentValue
			if err := decoder.Decode(&content); err != nil {
				return fmt.Errorf("system 字段无效: %w", err)
			}
			system = splitContent(roleInstruction, -1, "system", content.pieces)
			return nil
		case "messages":
			seenMessages = true
			return readArray(decoder, func(index int) error {
				var message struct {
					Role    string       `json:"role"`
					Content contentValue `json:"content"`
				}
				if err := decoder.Decode(&message); err != nil {
					return fmt.Errorf("消息对象无效: %w", err)
				}
				role, origin, ok := roleOf(message.Role)
				if !ok {
					return fmt.Errorf("协议角色无法识别: %q", message.Role)
				}
				if role == roleTool {
					conversation = append(conversation, promptItem{role: roleTool, message: index})
					return nil
				}
				conversation = append(conversation, splitContent(role, index, origin, message.Content.pieces)...)
				return nil
			})
		default:
			return discardValue(decoder)
		}
	})
	if err != nil {
		return nil, "", err
	}
	if !seenMessages {
		return nil, "", errors.New("messages 必须是消息数组")
	}
	return append(system, conversation...), model, nil
}

func decodeOptionalString(decoder *json.Decoder, target *string) error {
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if text, ok := value.(string); ok {
		*target = text
	}
	return nil
}

type responsesInput struct {
	items []promptItem
	set   bool
}

// UnmarshalJSON 逐项解出 Responses input 条目的类型、角色和文本。
func (r *responsesInput) UnmarshalJSON(raw []byte) error {
	r.set = true
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		r.items = append(r.items, promptItem{role: roleUser, parts: []promptPart{{text: text}}, origin: "message"})
		return nil
	}
	var entries []responsesEntry
	if raw[0] == '{' {
		var entry responsesEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return err
		}
		entries = []responsesEntry{entry}
	} else if raw[0] != '[' {
		return errors.New("Responses input 类型无效")
	} else if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	for index, entry := range entries {
		for _, item := range entry.items {
			item.message = index
			r.items = append(r.items, item)
		}
	}
	return nil
}

// responsesEntry 在解码时直接把一个 input 条目转换为条目列表，不保留原始字节。
type responsesEntry struct{ items []promptItem }

func (r *responsesEntry) UnmarshalJSON(entry []byte) error {
	if len(entry) > 0 && entry[0] == '"' {
		var text string
		if err := json.Unmarshal(entry, &text); err != nil {
			return err
		}
		r.items = []promptItem{{role: roleUser, parts: []promptPart{{text: text}}, origin: "message"}}
		return nil
	}
	var item struct {
		Type    string       `json:"type"`
		Role    string       `json:"role"`
		Content contentValue `json:"content"`
	}
	if err := json.Unmarshal(entry, &item); err != nil {
		return fmt.Errorf("Responses input 条目无效: %w", err)
	}
	switch {
	case item.Type == "additional_tools" || item.Type == "item_reference":
		return nil
	case strings.HasSuffix(item.Type, "_output") || item.Type == "tool_result":
		r.items = []promptItem{{role: roleTool}}
		return nil
	case strings.HasSuffix(item.Type, "_call") || slices.Contains([]string{"reasoning", "compaction", "agent_message"}, item.Type):
		r.items = []promptItem{{role: roleAssistant}}
		return nil
	case item.Type != "" && item.Type != "message" && item.Role == "":
		return nil
	}
	role, origin, ok := roleOf(defaultString(item.Role, "user"))
	if !ok {
		return fmt.Errorf("协议角色无法识别: %q", item.Role)
	}
	r.items = splitContent(role, 0, origin, item.Content.pieces)
	return nil
}

// decodeResponses 覆盖 Responses HTTP 与 WebSocket 的 response.create 事件。
func decodeResponses(decoder *json.Decoder, websocket bool) ([]promptItem, string, string, error) {
	var instructions []promptItem
	var input responsesInput
	var nested *json.RawMessage
	var model, previous, kind string
	err := readObject(decoder, func(key string) error {
		switch key {
		case "type":
			return decodeOptionalString(decoder, &kind)
		case "model":
			return decodeOptionalString(decoder, &model)
		case "previous_response_id":
			return decodeOptionalString(decoder, &previous)
		case "instructions":
			var content contentValue
			if err := decoder.Decode(&content); err != nil {
				return fmt.Errorf("instructions 字段无效: %w", err)
			}
			instructions = splitContent(roleInstruction, -1, "instructions", content.pieces)
			return nil
		case "input":
			return decoder.Decode(&input)
		case "response":
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				return err
			}
			nested = &raw
			return nil
		default:
			return discardValue(decoder)
		}
	})
	if err != nil {
		return nil, "", "", err
	}
	if websocket || kind != "" {
		if kind != "response.create" {
			return nil, "", "", ErrNotAuditable
		}
		if !input.set && nested != nil {
			inner := json.NewDecoder(strings.NewReader(string(*nested)))
			items, innerModel, innerPrevious, err := decodeResponses(inner, false)
			if err != nil {
				return nil, "", "", err
			}
			if innerModel != "" {
				model = innerModel
			}
			if innerPrevious != "" {
				previous = innerPrevious
			}
			return items, model, previous, nil
		}
	}
	return append(instructions, input.items...), model, previous, nil
}

// decodeGemini 覆盖 generateContent、streamGenerateContent、Imagen predict 与批处理子请求。
func decodeGemini(decoder *json.Decoder) ([]promptItem, string, error) {
	var system, conversation []promptItem
	var model string
	err := readObject(decoder, func(key string) error {
		switch key {
		case "model":
			return decodeOptionalString(decoder, &model)
		case "systemInstruction", "system_instruction":
			var instruction struct {
				Parts contentValue `json:"parts"`
			}
			if err := decoder.Decode(&instruction); err != nil {
				return fmt.Errorf("systemInstruction 字段无效: %w", err)
			}
			system = append(system, splitContent(roleInstruction, -1, "system", instruction.Parts.pieces)...)
			return nil
		case "contents", "content":
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			decodeContent := func(index int) error {
				var content struct {
					Role  string       `json:"role"`
					Parts contentValue `json:"parts"`
				}
				if err := decoder.Decode(&content); err != nil {
					return fmt.Errorf("Gemini content 对象无效: %w", err)
				}
				roleName := content.Role
				if roleName == "" {
					roleName = "user"
				}
				role, origin, ok := roleOf(roleName)
				if !ok {
					return fmt.Errorf("协议角色无法识别: %q", content.Role)
				}
				conversation = append(conversation, splitContent(role, index, origin, content.Parts.pieces)...)
				return nil
			}
			switch token {
			case json.Delim('['):
				for index := 0; decoder.More(); index++ {
					if err := decodeContent(index); err != nil {
						return err
					}
				}
				_, err = decoder.Token()
				return err
			case json.Delim('{'):
				// 单个 content 对象：重新组装为数组元素读取。
				var content struct {
					Role  string       `json:"role"`
					Parts contentValue `json:"parts"`
				}
				raw, err := readRemainingObject(decoder)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(raw, &content); err != nil {
					return fmt.Errorf("Gemini content 对象无效: %w", err)
				}
				role, origin, ok := roleOf(defaultString(content.Role, "user"))
				if !ok {
					return fmt.Errorf("协议角色无法识别: %q", content.Role)
				}
				conversation = append(conversation, splitContent(role, 0, origin, content.Parts.pieces)...)
				return nil
			case nil:
				return nil
			default:
				return errors.New("Gemini contents 类型无效")
			}
		case "instances":
			return readArray(decoder, func(index int) error {
				var instance struct {
					Prompt *string `json:"prompt"`
				}
				if err := decoder.Decode(&instance); err != nil {
					return err
				}
				if instance.Prompt != nil {
					conversation = append(conversation, promptItem{role: roleUser, parts: []promptPart{{text: *instance.Prompt}}, origin: "message", message: index})
				}
				return nil
			})
		case "requests":
			return readArray(decoder, func(int) error {
				var raw json.RawMessage
				if err := decoder.Decode(&raw); err != nil {
					return err
				}
				items, _, err := decodeGemini(json.NewDecoder(strings.NewReader(string(raw))))
				if err != nil {
					return err
				}
				for _, item := range items {
					if item.role == roleInstruction {
						system = append(system, item)
					} else {
						conversation = append(conversation, item)
					}
				}
				return nil
			})
		default:
			return discardValue(decoder)
		}
	})
	if err != nil {
		return nil, "", err
	}
	return append(system, conversation...), model, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// readRemainingObject 在已读取 '{' 后把对象剩余部分重新组装为完整 JSON。
func readRemainingObject(decoder *json.Decoder) ([]byte, error) {
	var builder strings.Builder
	builder.WriteByte('{')
	first := true
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("JSON 对象键无效")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if !first {
			builder.WriteByte(',')
		}
		first = false
		encoded, _ := json.Marshal(key)
		builder.Write(encoded)
		builder.WriteByte(':')
		builder.Write(value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	builder.WriteByte('}')
	return []byte(builder.String()), nil
}

// decodeEmbeddings 只取 input 中的字符串，token 数组不含可审文本。
func decodeEmbeddings(decoder *json.Decoder) ([]promptItem, string, error) {
	var items []promptItem
	var model string
	err := readObject(decoder, func(key string) error {
		switch key {
		case "model":
			return decodeOptionalString(decoder, &model)
		case "input":
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				return err
			}
			var text string
			if json.Unmarshal(raw, &text) == nil {
				items = append(items, promptItem{role: roleUser, parts: []promptPart{{text: text}}, origin: "message"})
				return nil
			}
			var values []json.RawMessage
			if json.Unmarshal(raw, &values) == nil {
				parts := make([]promptPart, 0, len(values))
				for _, value := range values {
					if json.Unmarshal(value, &text) == nil {
						parts = append(parts, promptPart{text: text})
					}
				}
				if len(parts) > 0 {
					items = append(items, promptItem{role: roleUser, parts: parts, origin: "message"})
				}
			}
			return nil
		default:
			return discardValue(decoder)
		}
	})
	return items, model, err
}

// mediaPromptKeys 是媒体与搜索协议中承载用户文本的已知字段（忽略大小写、下划线和连字符）。
var mediaPromptKeys = []string{"prompt", "inputprompt", "textprompt", "description", "query", "lyrics", "negativeprompt", "positiveprompt", "gptdescriptionprompt", "prompten", "finalprompt", "finalzhprompt", "origprompt", "actualprompt", "imageprompt", "input", "text"}

// decodeMedia 沿用媒体协议的文本字段规则；alpha search 带 messages 或 input 时按对应协议解析。
func decodeMedia(decoder *json.Decoder) ([]promptItem, string, error) {
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, "", err
	}
	var probe struct {
		Model    string  `json:"model"`
		Messages present `json:"messages"`
		Input    present `json:"input"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, "", errors.New("请求必须是结构化 JSON 对象")
	}
	if probe.Messages {
		items, model, err := decodeMessages(json.NewDecoder(strings.NewReader(string(raw))))
		return items, model, err
	}
	var parts []promptPart
	collectMediaPrompt(raw, &parts, 0)
	if len(parts) == 0 {
		return nil, probe.Model, nil
	}
	return []promptItem{{role: roleUser, parts: parts, origin: "message"}}, probe.Model, nil
}

func collectMediaPrompt(raw json.RawMessage, parts *[]promptPart, depth int) {
	if depth > 32 || len(raw) == 0 {
		return
	}
	switch raw[0] {
	case '[':
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) == nil {
			for _, value := range values {
				collectMediaPrompt(value, parts, depth+1)
			}
		}
	case '{':
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		// JSON 对象没有顺序，按稳定键顺序提取；数组顺序仍保留。
		slices.Sort(keys)
		for _, key := range keys {
			normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
			var text string
			if slices.Contains(mediaPromptKeys, normalized) && json.Unmarshal(object[key], &text) == nil {
				if !strings.HasPrefix(text, "data:") {
					*parts = append(*parts, promptPart{text: text})
				}
				continue
			}
			collectMediaPrompt(object[key], parts, depth+1)
		}
	}
}

// CheckDuplicateKeys 以流式扫描拒绝任意层级的重复 JSON 键，防止网关、审核器和上游对同一请求解释不同。
// 扫描只保存对象键，不展开字符串值。
func CheckDuplicateKeys(body io.Reader) error {
	reader := bufio.NewReaderSize(body, 64<<10)
	type frame struct {
		object bool
		keys   map[string]struct{}
		expect bool // 对象中下一个字符串是键
	}
	var stack []frame
	readString := func(keep bool) (string, error) {
		var builder []byte
		if keep {
			builder = append(builder, '"')
		}
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return "", errors.New("JSON 字符串不完整")
			}
			if keep {
				builder = append(builder, b)
			}
			if b == '\\' {
				next, err := reader.ReadByte()
				if err != nil {
					return "", errors.New("JSON 字符串不完整")
				}
				if keep {
					builder = append(builder, next)
				}
				continue
			}
			if b == '"' {
				break
			}
		}
		if !keep {
			return "", nil
		}
		var key string
		if err := json.Unmarshal(builder, &key); err != nil {
			return "", errors.New("JSON 对象键无效")
		}
		return key, nil
	}
	for {
		b, err := reader.ReadByte()
		if errors.Is(err, io.EOF) {
			if len(stack) != 0 {
				return errors.New("JSON 结构不完整")
			}
			return nil
		}
		if err != nil {
			return err
		}
		switch b {
		case '{':
			stack = append(stack, frame{object: true, keys: map[string]struct{}{}, expect: true})
		case '[':
			stack = append(stack, frame{})
		case '}', ']':
			if len(stack) == 0 {
				return errors.New("JSON 结构无效")
			}
			stack = stack[:len(stack)-1]
		case ',':
			if len(stack) > 0 && stack[len(stack)-1].object {
				stack[len(stack)-1].expect = true
			}
		case '"':
			top := len(stack) - 1
			if top >= 0 && stack[top].object && stack[top].expect {
				key, err := readString(true)
				if err != nil {
					return err
				}
				if _, exists := stack[top].keys[key]; exists {
					return fmt.Errorf("重复的 JSON 键 %q", key)
				}
				stack[top].keys[key] = struct{}{}
				stack[top].expect = false
			} else if _, err := readString(false); err != nil {
				return err
			}
		}
	}
}

// ValidUTF8Text 判断 multipart 文本字段能否作为审核文本。
func ValidUTF8Text(raw []byte) bool { return utf8.Valid(raw) }
