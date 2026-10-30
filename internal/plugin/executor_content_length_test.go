package plugin

import (
	"bufio"
	"strings"
	"testing"
)

// 复证圈 2 审计发现 4 的守卫：Content-Length 头值必须严格解析。旧实现用
// fmt.Sscanf("%d") 宽松解析——"12junk" 被静默截断为 12 并接受，与同函数
// 其余 fail-fast 纪律（重复 Content-Length、帧头块意外行均拒绝）不一致；
// 畸形值必须按帧结构损坏拒绝，与宿主侧 codec readHeader 的 strconv 严格
// 语义对齐。
func TestReadFrame_ContentLengthStrictlyParsed(t *testing.T) {
	cases := []struct {
		name  string
		wire  string
		legal bool // 旧实现对非法样本也返回 nil error 时测试红
	}{
		{"尾随垃圾", "Content-Length: 12junk\r\n\r\n0123456789AB", false},
		{"空值", "Content-Length: \r\n\r\n", false},
		{"非数字", "Content-Length: abc\r\n\r\n", false},
		{"十六进制形态", "Content-Length: 0x10\r\n\r\n", false},
		{"合法十进制", "Content-Length: 4\r\n\r\nabcd", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readFrame(bufio.NewReader(strings.NewReader(tc.wire)))
			if tc.legal && err != nil {
				t.Fatalf("合法帧被误拒: %v", err)
			}
			if !tc.legal && err == nil {
				t.Fatal("畸形 Content-Length 被静默接受（应按帧结构损坏拒绝）")
			}
		})
	}
}
