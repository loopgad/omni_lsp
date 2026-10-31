package security

import "regexp"

// redactPattern 覆盖常见凭据键名的 key=value / key: value 形式（§N11）：
//   - 键名两侧允许可选引号，覆盖 JSON 风格 "password": "hunter2"
//   - 未加引号的值在空白、'&'、';' 处截断，避免吞掉同行的下一个键
//     （如查询串 token=aaa&secret=bbb）
//   - 分隔符之后可选消费 "bearer <tok>" 方案词，使
//     "Authorization: Bearer xyz" 连令牌一起消失，而非只遮住 "Bearer"
//
// 复证圈 5 sweep 发现 1 的两处拓宽（原任务正则 + §S23 偏离之外的新增偏离）：
//   - 键名从「恰为关键词」放宽为「含关键词的标识符」——readApiTokens 一类
//     键（关键词作为子串）此前整体不匹配，多行数组凭据明文穿过脱敏
//   - 值交替新增扁平数组形态 \[[^\[\]]*\]（负类天然跨行）——pretty JSON 的
//     多行数组（用户手写配置与 MarshalIndent 自产的同形态）不再绕过脱敏；
//     嵌套容器不在保证面内（配置凭据数组的现实形态是扁平字符串数组），
//     超出部分属整值替换的上界裁量，保持 fail-closed 方向
//
// 与 goal.md §N11 任务给定的字面正则相比，累计三处偏离（引号结构、Bearer
// 方案词、上述两处）；一次编译，包级复用。
var redactPattern = regexp.MustCompile(
	`(?i)("?[a-z0-9_.-]*(?:token|password|secret|authorization|api[_-]?key|bearer)[a-z0-9_.-]*"?)\s*[=:]\s*(?:bearer\s+)?(?:\[[^\[\]]*\]|"[^"]*"|[^\s&;]+)`)

// RedactString 将 s 中常见凭据（token/password/secret/authorization/api-key/
// bearer，含以其为子串的标识符键名）的值替换为 [REDACTED]，保留键名结构，
// 供日志、追踪与错误路径复用（§N11：默认脱敏）。值形态覆盖单行标量、带引
// 号标量与扁平数组（含多行 pretty JSON），见 redactPattern 的偏离登记。
func RedactString(s string) string {
	return redactPattern.ReplaceAllString(s, "${1}=[REDACTED]")
}
