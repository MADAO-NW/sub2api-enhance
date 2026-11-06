package thirdpartypromptaudit

import (
	"regexp"
	"slices"
	"strings"
)

// 模板动作：易变片段直接剔除（不审不存），项目指令文件转为独立指令块（照常审核）。
const (
	templateStripBlock   = "strip_block"
	templateStripSegment = "strip_segment"
	templateInstruction  = "instruction"
)

// 模板位置：易变片段只在真实客户端使用的位置识别。
const (
	positionSystemText       = "system_text"
	positionUserText         = "user_text"
	positionFirstUserMessage = "first_user_message"
	positionContextPrefix    = "context_prefix"
)

// templateRule 是易变片段规则表的一行。Fields 和 MaxBytes 同时用于匹配和审阅说明；
// 不匹配任何规则的文本一律保留审核，客户端指纹只决定使用哪一组规则。
type templateRule struct {
	Client   string
	Name     string
	Position string
	Action   string
	MaxBytes int
	Fields   string
	// match 返回需要剔除的区间（strip_segment）或是否整体命中（strip_block、instruction）。
	match func(text string) [][2]int
}

// templatePosition 描述当前文本块在请求中的位置。
type templatePosition struct {
	firstUserMessage bool
	contextPrefix    bool
}

var (
	envLinePattern          = regexp.MustCompile(`^(Working directory|Is directory a git repo|Additional working directories|Platform|OS Version|Today's date): [^\n]{1,512}$`)
	billingHeaderPattern    = regexp.MustCompile(`^x-anthropic-billing-header: [\x20-\x7e]{1,500}$`)
	gitStatusHeader         = "gitStatus: This is the git status at the start of the conversation. Note that this status is a snapshot in time, and will not update during the conversation."
	gitBranchPattern        = regexp.MustCompile(`^Current branch: [^\n]{0,256}$`)
	gitMainBranchPattern    = regexp.MustCompile(`^Main branch \(you will usually use this for PRs\): [^\n]{0,256}$`)
	gitUserPattern          = regexp.MustCompile(`^Git user: [^\n]{0,256}$`)
	gitStatusLinePattern    = regexp.MustCompile(`^(?:[ MADRCUT?!]{2} [^\n]{1,512}|\(clean\)|\.\.\. \(truncated because it exceeds [^\n]{1,200}\))$`)
	gitCommitPattern        = regexp.MustCompile(`^[0-9a-f]{7,40} [^\n]{0,512}$`)
	currentDatePattern      = regexp.MustCompile(`# currentDate\nToday's date is \d{4}[-/]\d{2}[-/]\d{2}\.\n?`)
	environmentLinePattern  = regexp.MustCompile(`^\s*<(cwd|approval_policy|sandbox_mode|network_access|shell|current_date|timezone)>([^<\n]{0,512})</(cwd|approval_policy|sandbox_mode|network_access|shell|current_date|timezone)>$`)
	writableRootPattern     = regexp.MustCompile(`^\s*<root>[^<\n]{1,512}</root>$`)
	geminiDatePattern       = regexp.MustCompile(`^Today's date is [^\n]{1,120}$`)
	geminiOSPattern         = regexp.MustCompile(`^My operating system is: [^\n]{1,64}$`)
	geminiDirectoryPattern  = regexp.MustCompile(`^I'm currently working in the directory: [^\n]{1,512}$`)
	geminiShowingPattern    = regexp.MustCompile(`^Showing up to \d+ items \(files \+ folders\)\.[^\n]{0,300}$`)
	geminiTreeLinePattern   = regexp.MustCompile(`^(?:(?:│   |    )*[├└]───[^\n]{1,256}|/[^\n]{0,511}/|\.\.\.)$`)
	todoChangedPrefix       = "<system-reminder>\nYour todo list has changed. DO NOT mention this explicitly to the user. Here are the latest contents of your todo list:\n\n"
	todoChangedSuffix       = ". Continue on with the tasks at hand if applicable.\n</system-reminder>"
	todoItemPattern         = regexp.MustCompile(`^\{"content":"(?:[^"\\]|\\.){1,500}","status":"(?:pending|in_progress|completed)"(?:,"(?:activeForm|priority|id)":"(?:[^"\\]|\\.){0,500}")*\}$`)
	claudeContextPrefix     = "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n"
	codexAgentsPrefix       = "# AGENTS.md instructions for "
	geminiContextFirstLine  = "This is the Gemini CLI. We are setting up the context for our chat."
	geminiStructureLine     = "Here is the folder structure of the current working directories:"
	todoEmptyReminder       = "<system-reminder>\nThis is a reminder that your todo list is currently empty. DO NOT mention this to the user explicitly because they are already aware. If you are working on tasks that would benefit from a todo list please use the TodoWrite tool to create one. If not, please feel free to ignore. Again do not mention this message to the user.\n</system-reminder>"
	maxTodoChangedReminder  = 8192
	maxEnvironmentContext   = 4096
	maxGeminiSessionContext = 16384
)

// templateRules 是严格模板规则表。字段清单与长度上限需要用真实样例复核后再调整。
var templateRules = []templateRule{
	{Client: ClientClaudeCode, Name: "claude_billing_header", Position: positionSystemText, Action: templateStripBlock, MaxBytes: 512,
		Fields: "整块仅一行 `x-anthropic-billing-header: <可打印 ASCII>`", match: wholeMatch(512, billingHeaderPattern.MatchString)},
	{Client: ClientClaudeCode, Name: "claude_env", Position: positionSystemText, Action: templateStripSegment, MaxBytes: 2048,
		Fields: "`<env>` 与 `</env>` 之间每行必须是 `Working directory`/`Is directory a git repo`/`Additional working directories`/`Platform`/`OS Version`/`Today's date` 之一的单行 `Key: value`（值 ≤512 字节）", match: matchClaudeEnv},
	{Client: ClientClaudeCode, Name: "claude_git_status", Position: positionSystemText, Action: templateStripSegment, MaxBytes: 6144,
		Fields: "固定首行 + `Current branch:` + `Main branch (...):` + 可选 `Git user:` + `Status:`（porcelain 行、`(clean)` 或截断说明）+ `Recent commits:`（1–20 行 `<7–40 位哈希> <标题>`），必须位于文本块末尾", match: matchClaudeGitStatus},
	{Client: ClientClaudeCode, Name: "claude_todo_empty_reminder", Position: positionUserText, Action: templateStripBlock, MaxBytes: len(todoEmptyReminder),
		Fields: "与 Claude Code 待办为空提醒全文逐字一致", match: wholeMatch(len(todoEmptyReminder), func(text string) bool { return text == todoEmptyReminder })},
	{Client: ClientClaudeCode, Name: "claude_todo_changed_reminder", Position: positionUserText, Action: templateStripBlock, MaxBytes: maxTodoChangedReminder,
		Fields: "固定前后缀，中间是待办 JSON 数组；每项只允许 content（≤500）、status（pending/in_progress/completed）、activeForm/priority/id", match: wholeMatch(maxTodoChangedReminder, matchTodoChanged)},
	{Client: ClientClaudeCode, Name: "claude_md_context", Position: positionFirstUserMessage, Action: templateInstruction,
		Fields: "首条 user 消息中以固定 context 提醒开头、以 `</system-reminder>` 结尾的文本块转为指令块；其中 `# currentDate` 日期行按精确正则剔除", match: matchClaudeContext},
	{Client: ClientCodex, Name: "codex_environment_context", Position: positionUserText, Action: templateStripBlock, MaxBytes: maxEnvironmentContext,
		Fields: "`<environment_context>` 内每行一个已知子标签（cwd/approval_policy/sandbox_mode/network_access/shell/current_date/timezone，值单行 ≤512）或 `<writable_roots>` 内的 `<root>` 行", match: wholeMatch(maxEnvironmentContext, matchEnvironmentContext)},
	{Client: ClientCodex, Name: "codex_agents_md", Position: positionContextPrefix, Action: templateInstruction,
		Fields: "首个 assistant/tool 条目之前的 user 文本块：`# AGENTS.md instructions for ` 开头且含 `<INSTRUCTIONS>…</INSTRUCTIONS>`，或 `<user_instructions>…</user_instructions>`，整块转为指令块", match: matchCodexAgents},
	{Client: ClientGeminiCLI, Name: "gemini_session_context", Position: positionFirstUserMessage, Action: templateStripBlock, MaxBytes: maxGeminiSessionContext,
		Fields: "首条 user 消息的固定开场行 + 日期行 + 操作系统行 + 工作目录行 + 固定目录结构说明 + 目录树行（树形前缀或根路径，单行 ≤256）", match: wholeMatch(maxGeminiSessionContext, matchGeminiSession)},
}

func templateRulesFor(client string) []templateRule {
	rules := make([]templateRule, 0)
	for _, rule := range templateRules {
		if rule.Client == client {
			rules = append(rules, rule)
		}
	}
	return rules
}

func wholeMatch(maxBytes int, accept func(string) bool) func(string) [][2]int {
	return func(text string) [][2]int {
		trimmed := strings.TrimSpace(text)
		if len(trimmed) > maxBytes || !accept(trimmed) {
			return nil
		}
		return [][2]int{{0, len(text)}}
	}
}

// applySystemTemplates 处理 system/developer/instructions 文本块；drop 表示整块是易变片段。
func applySystemTemplates(rules []templateRule, text string) (string, []string, bool) {
	var stripped []string
	for _, rule := range rules {
		if rule.Position != positionSystemText {
			continue
		}
		spans := rule.match(text)
		if len(spans) == 0 {
			continue
		}
		stripped = append(stripped, rule.Name)
		if rule.Action == templateStripBlock {
			return "", stripped, true
		}
		text = removeSpans(text, spans)
	}
	return text, stripped, false
}

type userTemplateOutcome struct {
	text        string
	stripped    []string
	drop        bool
	instruction bool
}

// applyUserTemplates 处理 user 文本块：易变片段剔除，已识别的项目指令文件转为指令块，其余保留为用户提示词。
func applyUserTemplates(rules []templateRule, text string, position templatePosition) userTemplateOutcome {
	outcome := userTemplateOutcome{text: text}
	for _, rule := range rules {
		switch rule.Position {
		case positionUserText:
		case positionFirstUserMessage:
			if !position.firstUserMessage {
				continue
			}
		case positionContextPrefix:
			if !position.contextPrefix {
				continue
			}
		default:
			continue
		}
		spans := rule.match(text)
		if len(spans) == 0 {
			continue
		}
		outcome.stripped = append(outcome.stripped, rule.Name)
		switch rule.Action {
		case templateStripBlock:
			outcome.drop = true
			outcome.text = ""
			return outcome
		case templateInstruction:
			outcome.instruction = true
			if rule.Name == "claude_md_context" {
				outcome.text = currentDatePattern.ReplaceAllString(text, "")
			}
			return outcome
		}
	}
	return outcome
}

func removeSpans(text string, spans [][2]int) string {
	slices.SortFunc(spans, func(a, b [2]int) int { return b[0] - a[0] })
	for _, span := range spans {
		text = text[:span[0]] + text[span[1]:]
	}
	return text
}

// matchClaudeEnv 只剔除每行都属于已知字段的 <env> 段。
func matchClaudeEnv(text string) [][2]int {
	var spans [][2]int
	offset := 0
	for {
		start := strings.Index(text[offset:], "<env>\n")
		if start < 0 {
			return spans
		}
		start += offset
		end := strings.Index(text[start:], "\n</env>")
		if end < 0 {
			return spans
		}
		end += start + len("\n</env>")
		body := text[start+len("<env>\n") : end-len("\n</env>")]
		valid := end-start <= 2048 && body != ""
		for _, line := range strings.Split(body, "\n") {
			if !envLinePattern.MatchString(line) {
				valid = false
				break
			}
		}
		if valid {
			spans = append(spans, [2]int{start, end})
		}
		offset = end
	}
}

// matchClaudeGitStatus 只剔除位于文本块末尾、结构完整的 gitStatus 快照。
func matchClaudeGitStatus(text string) [][2]int {
	start := strings.LastIndex(text, gitStatusHeader)
	if start < 0 || (start > 0 && text[start-1] != '\n') {
		return nil
	}
	segment := strings.TrimRight(text[start:], " \n")
	if len(segment) > 6144 {
		return nil
	}
	lines := strings.Split(segment, "\n")
	index := 1
	next := func() (string, bool) {
		if index >= len(lines) {
			return "", false
		}
		line := lines[index]
		index++
		return line, true
	}
	expect := func(accept func(string) bool) bool {
		line, ok := next()
		return ok && accept(line)
	}
	blank := func(line string) bool { return line == "" }
	if !expect(gitBranchPattern.MatchString) || !expect(blank) || !expect(gitMainBranchPattern.MatchString) || !expect(blank) {
		return nil
	}
	line, ok := next()
	if ok && gitUserPattern.MatchString(line) {
		if !expect(blank) {
			return nil
		}
		line, ok = next()
	}
	if !ok || line != "Status:" {
		return nil
	}
	for {
		line, ok = next()
		if !ok {
			return nil
		}
		if line == "" {
			break
		}
		if !gitStatusLinePattern.MatchString(line) {
			return nil
		}
	}
	if !expect(func(line string) bool { return line == "Recent commits:" }) {
		return nil
	}
	commits := 0
	for index < len(lines) {
		line, _ = next()
		if !gitCommitPattern.MatchString(line) {
			return nil
		}
		commits++
	}
	if commits == 0 || commits > 20 {
		return nil
	}
	return [][2]int{{start, len(text)}}
}

func matchTodoChanged(text string) bool {
	if !strings.HasPrefix(text, todoChangedPrefix) || !strings.HasSuffix(text, todoChangedSuffix) {
		return false
	}
	body := text[len(todoChangedPrefix) : len(text)-len(todoChangedSuffix)]
	if !strings.HasPrefix(body, "[") || !strings.HasSuffix(body, "]") {
		return false
	}
	inner := body[1 : len(body)-1]
	if inner == "" {
		return true
	}
	for _, item := range splitTodoItems(inner) {
		if !todoItemPattern.MatchString(item) {
			return false
		}
	}
	return true
}

// splitTodoItems 按对象边界拆分紧凑 JSON 数组；字符串内的括号和逗号不参与拆分。
func splitTodoItems(inner string) []string {
	var items []string
	depth, start, quoted, escaped := 0, 0, false, false
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && quoted:
			escaped = true
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '{':
			depth++
		case c == '}':
			depth--
		case c == ',' && depth == 0:
			items = append(items, inner[start:i])
			start = i + 1
		}
	}
	return append(items, inner[start:])
}

func matchClaudeContext(text string) [][2]int {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, claudeContextPrefix) && strings.HasSuffix(trimmed, "</system-reminder>") {
		return [][2]int{{0, len(text)}}
	}
	return nil
}

func matchEnvironmentContext(text string) bool {
	lines := strings.Split(text, "\n")
	if len(lines) < 2 || lines[0] != "<environment_context>" || lines[len(lines)-1] != "</environment_context>" {
		return false
	}
	inRoots := false
	for _, line := range lines[1 : len(lines)-1] {
		switch trimmed := strings.TrimSpace(line); {
		case trimmed == "<writable_roots>" && !inRoots:
			inRoots = true
		case trimmed == "</writable_roots>" && inRoots:
			inRoots = false
		case inRoots:
			if !writableRootPattern.MatchString(line) {
				return false
			}
		default:
			match := environmentLinePattern.FindStringSubmatch(line)
			if match == nil || match[1] != match[3] {
				return false
			}
		}
	}
	return !inRoots
}

func matchCodexAgents(text string) [][2]int {
	trimmed := strings.TrimSpace(text)
	agents := strings.HasPrefix(trimmed, codexAgentsPrefix) && strings.Contains(trimmed, "\n<INSTRUCTIONS>") && strings.HasSuffix(trimmed, "</INSTRUCTIONS>")
	legacy := strings.HasPrefix(trimmed, "<user_instructions>") && strings.HasSuffix(trimmed, "</user_instructions>")
	if agents || legacy {
		return [][2]int{{0, len(text)}}
	}
	return nil
}

func matchGeminiSession(text string) bool {
	lines := strings.Split(text, "\n")
	if len(lines) < 6 || lines[0] != geminiContextFirstLine || !geminiDatePattern.MatchString(lines[1]) ||
		!geminiOSPattern.MatchString(lines[2]) || !geminiDirectoryPattern.MatchString(lines[3]) || lines[4] != geminiStructureLine {
		return false
	}
	for _, line := range lines[5:] {
		if line == "" || geminiShowingPattern.MatchString(line) || geminiTreeLinePattern.MatchString(line) {
			continue
		}
		return false
	}
	return true
}

// TemplateRuleView 是规则表的审阅视图，用于文档和测试，不包含匹配实现。
type TemplateRuleView struct {
	Client   string `json:"client"`
	Name     string `json:"name"`
	Position string `json:"position"`
	Action   string `json:"action"`
	MaxBytes int    `json:"max_bytes"`
	Fields   string `json:"fields"`
}

// TemplateRules 返回当前严格模板规则表。
func TemplateRules() []TemplateRuleView {
	views := make([]TemplateRuleView, 0, len(templateRules))
	for _, rule := range templateRules {
		views = append(views, TemplateRuleView{Client: rule.Client, Name: rule.Name, Position: rule.Position, Action: rule.Action, MaxBytes: rule.MaxBytes, Fields: rule.Fields})
	}
	return views
}
