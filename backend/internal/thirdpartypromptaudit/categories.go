package thirdpartypromptaudit

import (
	"regexp"
	"slices"
	"strings"
)

// 违规类别的处置级别：立即停用与仅通知（照常阻断、不累计停用）。
const (
	CategoryDispositionImmediateDisable = "immediate_disable"
	CategoryDispositionNoticeOnly       = "notice_only"
)

// categoryCodePattern 是政策中声明的违规类别编号格式，如 R01。
var categoryCodePattern = regexp.MustCompile(`^R\d{2}$`)

// categoryTitlePattern 从政策正文按行解析类别编号与名称，如 “R01 恶意使用”。
var categoryTitlePattern = regexp.MustCompile(`^R(\d{2}) (.+)$`)

// categoryReasonPrefixes 是 reason 中允许携带类别编号的结论前缀。
var categoryReasonPrefixes = []string{"违规/", "待复核/", "指令风险/"}

// validCategoryCode 判断类别编号是否符合 R01 形式。
func validCategoryCode(code string) bool {
	return categoryCodePattern.MatchString(code)
}

// policyCategoryNames 从当前生效政策中解析类别编号到名称的映射；自定义政策没有该格式时返回空。
func policyCategoryNames(policy string) map[string]string {
	names := make(map[string]string)
	for _, line := range strings.Split(policy, "\n") {
		match := categoryTitlePattern.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		names["R"+match[1]] = strings.TrimSpace(match[2])
	}
	return names
}

// reasonCategories 取 reason 第一个冒号之前的前缀，解析其中声明的类别编号。
// 只保留当前政策声明过的编号；正常结论、无前缀或格式不符时返回空。
func reasonCategories(reason string, declared map[string]string) []string {
	trimmed := strings.TrimSpace(reason)
	index := strings.IndexAny(trimmed, ":：")
	if index <= 0 {
		return nil
	}
	prefix := trimmed[:index]
	rest := ""
	for _, marker := range categoryReasonPrefixes {
		if strings.HasPrefix(prefix, marker) {
			rest = strings.TrimPrefix(prefix, marker)
			break
		}
	}
	if rest == "" {
		return nil
	}
	codes := make([]string, 0, 2)
	for _, field := range strings.FieldsFunc(rest, func(r rune) bool {
		return r == '、' || r == ',' || r == '，' || r == '/' || r == ' '
	}) {
		field = strings.TrimSpace(field)
		if !validCategoryCode(field) {
			continue
		}
		// declared 为 nil 时只校验格式，供运行诊断统计类别识别率。
		if declared != nil {
			if _, ok := declared[field]; !ok {
				continue
			}
		}
		if !slices.Contains(codes, field) {
			codes = append(codes, field)
		}
	}
	if len(codes) == 0 {
		return nil
	}
	return codes
}

// targetCategories 按本任务冻结的阈值计算目标类别：block 取投 block 票的节点，
// review 取达到复核阈值的节点，这些节点的类别取并集；pass 没有类别。
func targetCategories(nodes []NodeResult, config Config, decision Decision) []string {
	if decision == DecisionPass || decision == "" || len(nodes) == 0 {
		return nil
	}
	declared := policyCategoryNames(config.AuditPrompt)
	if len(declared) == 0 {
		return nil
	}
	codes := make([]string, 0, 2)
	for _, node := range nodes {
		if node.Skipped || node.Error != nil || node.Confidence == nil {
			continue
		}
		vote := classifyScore(*node.Confidence, config)
		if decision == DecisionBlock && vote != DecisionBlock {
			continue
		}
		if decision == DecisionReview && vote == DecisionPass {
			continue
		}
		for _, code := range reasonCategories(node.Reason, declared) {
			if !slices.Contains(codes, code) {
				codes = append(codes, code)
			}
		}
	}
	if len(codes) == 0 {
		return nil
	}
	slices.Sort(codes)
	return codes
}

// categoryDisposition 返回类别对应的处置级别；未配置时返回空串，表示按普通规则处理。
func categoryDisposition(config Config, codes []string) string {
	if len(codes) == 0 {
		return ""
	}
	disposition := ""
	for _, code := range codes {
		switch config.CategoryRules[code] {
		case CategoryDispositionImmediateDisable:
			// 涉及多个类别时立即停用优先。
			return CategoryDispositionImmediateDisable
		case CategoryDispositionNoticeOnly:
			disposition = CategoryDispositionNoticeOnly
		}
	}
	return disposition
}
