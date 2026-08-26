package security

import "regexp"

// redactPattern 覆盖常见凭据键名的 key=value / key: value 形式（§N11）：
//   - 键名两侧允许可选引号，覆盖 JSON 风格 "password": "hunter2"
//   - 未加引号的值在空白、'&'、';' 处截断，避免吞掉同行的下一个键
//     （如查询串 token=aaa&secret=bbb）
//   - 分隔符之后可选消费 "bearer <tok>" 方案词，使
//     "Authorization: Bearer xyz" 连令牌一起消失，而非只遮住 "Bearer"
//
// 与 goal.md §N11 任务给定的字面正则相比，仅新增上述两点以满足 §S23 审计
// 样例（JSON 引号结构与 Bearer 方案头）；键名交替与 (?i) 保持一致。
// 一次编译，包级复用。
var redactPattern = regexp.MustCompile(
	`(?i)("?(?:token|password|secret|authorization|api[_-]?key|bearer)"?)\s*[=:]\s*(?:bearer\s+)?(?:"[^"]*"|[^\s&;]+)`)

// RedactString 将 s 中常见凭据（token/password/secret/authorization/api-key/
// bearer）的值替换为 [REDACTED]，保留键名结构，供日志、追踪与错误路径复用
// （§N11：默认脱敏）。本任务仅交付 helper 与审计测试，不强制接线到全部调用点。
func RedactString(s string) string {
	return redactPattern.ReplaceAllString(s, "${1}=[REDACTED]")
}
