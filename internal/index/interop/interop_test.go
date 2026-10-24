package interop

import (
	"strings"
	"testing"
)

func TestL14_LossyNotesDocumented(t *testing.T) {
	notes := LossyNotes()
	if len(notes) == 0 {
		t.Fatal("LossyNotes() 为空：§L14 要求有损映射必须文档化")
	}
	for i, n := range notes {
		if !strings.Contains(n, "有损") {
			t.Errorf("LossyNotes()[%d] 缺少「有损」关键词: %q", i, n)
		}
		if len(n) < 20 {
			t.Errorf("LossyNotes()[%d] 过短，不像有效文档: %q", i, n)
		}
	}
}
