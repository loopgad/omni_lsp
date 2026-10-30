package jsonrpc

// 审计批 2 发现 1 守卫（帧跳过原语）：配额收紧的帧上限拒绝的是单帧内存
// 分配，不是整条流。MaxMessageSize 拒绝超限帧时，SkipOversizedFrames=true
// 让 codec 按 Content-Length 排空并丢弃该帧 body，返回 *FrameSkippedError，
// 同一条流上的后续帧照常可读。缺省 false：§C1 静态上限的 fatal 语义零变化
// （X10），codec_test.go / codec_framing_test.go 的既有钉住不受影响。

import (
	"bufio"
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestReadMessageSkipsOversizedFrameKeepsStream(t *testing.T) {
	c := &Codec{MaxMessageSize: 64, SkipOversizedFrames: true}

	// 100 字节超限帧 + 合法帧，同一条流顺序到达。
	valid := `{"jsonrpc":"2.0","method":"ping","params":{}}`
	stream := "Content-Length: 100\r\n\r\n" + strings.Repeat("x", 100) +
		"Content-Length: " + strconv.Itoa(len(valid)) + "\r\n\r\n" + valid
	r := bufio.NewReader(strings.NewReader(stream))

	_, err := c.ReadMessage(r)
	if err == nil {
		t.Fatal("oversized frame must be reported, not silently swallowed")
	}
	var skipped *FrameSkippedError
	if !errors.As(err, &skipped) {
		t.Fatalf("err = %T (%v), want *FrameSkippedError", err, err)
	}
	if skipped.Declared != 100 || skipped.Cap != 64 {
		t.Fatalf("skipped frame = declared %d cap %d, want 100/64", skipped.Declared, skipped.Cap)
	}

	// 同一条流：超限帧之后的合法帧必须照常可读（这才是本守卫的核心）。
	msg, err := c.ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage after skipped frame: %v (stream must stay usable)", err)
	}
	if msg.Method != "ping" {
		t.Fatalf("method = %q, want ping", msg.Method)
	}
}

func TestReadMessageOversizeStaysFatalWithoutSkip(t *testing.T) {
	// 缺省（SkipOversizedFrames=false）：§C1 fatal 语义零变化。
	c := &Codec{MaxMessageSize: 64}
	stream := "Content-Length: 100\r\n\r\n" + strings.Repeat("x", 100) +
		"Content-Length: 2\r\n\r\n{}"
	r := bufio.NewReader(strings.NewReader(stream))

	_, err := c.ReadMessage(r)
	if err == nil {
		t.Fatal("oversized frame must be refused without skip enabled")
	}
	var skipped *FrameSkippedError
	if errors.As(err, &skipped) {
		t.Fatal("default codec must not report FrameSkippedError (X10: default semantics unchanged)")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("message too large")) {
		t.Fatalf("err = %v, want the §C1 message-too-large refusal", err)
	}
	// 流已断（fatal）：本实现按 fatal 处理，下一帧读取不再保证同步。
}

func TestSkippedFrameBodyIsDrainedExactly(t *testing.T) {
	// 排空必须精确按声明长度消费，后续帧头不得被当 body 吞掉。
	c := &Codec{MaxMessageSize: 40, SkipOversizedFrames: true}
	valid := `{"id":7,"result":true}`
	stream := "Content-Length: 64\r\n\r\n" + strings.Repeat("y", 64) +
		"Content-Length: " + strconv.Itoa(len(valid)) + "\r\n\r\n" + valid
	r := bufio.NewReader(strings.NewReader(stream))

	if _, err := c.ReadMessage(r); err == nil {
		t.Fatal("oversized frame must be skipped with an error report")
	}
	msg, err := c.ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage after skip: %v", err)
	}
	if msg.ID == nil || msg.ID.Num != 7 {
		t.Fatalf("response id = %+v, want 7 (declared-length drain must not eat the next frame)", msg.ID)
	}
}
