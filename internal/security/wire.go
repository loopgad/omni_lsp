package security

import (
	"fmt"
	"io"
)

// FprintfRedacted 是 §N11 默认脱敏的 internal 侧统一接线口（复证圈 5 sweep
// 发现 4）：格式化结果先过 RedactString 再落 w——操作日志/诊断的 stderr 行
// 统一经此口，杜绝「index 子命令脱敏、兄弟通道裸写」的结构缺口。internal
// 各包（plugin 车道除外——其包文档声明仅标准库依赖）的 stderr 写点一律走
// 本 helper； RedactString 本身已登记「本任务不强制接线全部调用点」的旧
// 自认，本 helper 是该待办的闭合面。
func FprintfRedacted(w io.Writer, format string, args ...any) (int, error) {
	return fmt.Fprint(w, RedactString(fmt.Sprintf(format, args...)))
}
