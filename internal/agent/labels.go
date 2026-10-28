package agent

// CategoryName returns the Chinese label, or an empty string for unknown IDs.
func CategoryName(category string) string {
	switch category {
	case "style":
		return "代码规范"
	case "reliability":
		return "正确性与可靠性"
	case "maintainability":
		return "可维护性"
	case "performance":
		return "性能效率"
	case "security":
		return "安全风险"
	case "tests":
		return "测试质量"
	case "build":
		return "构建与交付"
	default:
		return ""
	}
}

func SeverityName(severity string) string {
	switch severity {
	case "critical":
		return "严重"
	case "high":
		return "高"
	case "medium":
		return "中"
	case "low":
		return "低"
	default:
		return severity
	}
}
