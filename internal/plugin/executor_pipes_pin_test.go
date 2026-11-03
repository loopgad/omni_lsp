package plugin

import (
	"errors"
	"io"
	"testing"
)

// pipesFakeOpener：StdinPipe 成功并交还可观察的假写端；StdoutPipe 注入失败。
// 生产路径上 exec.Cmd 的 StdoutPipe 只在预置 c.Stdout 时失败，cmd 由 launch
// 内部构造，无法自然触发——注入面即 pluginPipes 的 pipeOpener 参数。
type pipesFakeOpener struct {
	stdin     *pipesFakeWriteCloser
	stdoutErr error
}

func (f *pipesFakeOpener) StdinPipe() (io.WriteCloser, error) {
	f.stdin = &pipesFakeWriteCloser{}
	return f.stdin, nil
}

func (f *pipesFakeOpener) StdoutPipe() (io.ReadCloser, error) {
	return nil, f.stdoutErr
}

type pipesFakeWriteCloser struct{ closed bool }

func (w *pipesFakeWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (w *pipesFakeWriteCloser) Close() error                { w.closed = true; return nil }

// TestPluginPipesStdoutPipeFailureClosesStdin pin：StdoutPipe 失败的错误路径
// 必须确定性关闭已到手的 stdin 写端。该分支仅在 fd 压力下可达，若不收口，
// 写端与 cmd 内部读端两条句柄悬挂待 GC 终结器，泄漏要累积到 fd 耗尽才显现。
// （cmd 内部读端由 exec.Cmd 私有持有、无导出收口面，确定性关闭仅覆盖写端；
// 读端仍随 cmd 一起被 GC 终结——与本 pin 固定的写端契约互补。）
func TestPluginPipesStdoutPipeFailureClosesStdin(t *testing.T) {
	opener := &pipesFakeOpener{stdoutErr: errors.New("injected: stdout 管道失败")}
	stdin, stdout, err := pluginPipes(opener)
	if err == nil {
		t.Fatal("注入 StdoutPipe 失败后 pluginPipes 必须报错")
	}
	if stdin != nil || stdout != nil {
		t.Fatalf("错误返回的两个管道端必须为 nil，got stdin=%v stdout=%v", stdin, stdout)
	}
	if opener.stdin == nil {
		t.Fatal("StdinPipe 应已成功并被记录")
	}
	if !opener.stdin.closed {
		t.Fatal("StdoutPipe 失败分支必须关闭已到手的 stdin 写端（防 fd 悬挂）")
	}
}

// TestPluginPipesStdinPipeFailureKeepsNoHandle pin：StdinPipe 先败时不得有
// 任何残留句柄需要清理（对称面：错误返回的两个管道端必须为 nil）。
func TestPluginPipesStdinPipeFailureKeepsNoHandle(t *testing.T) {
	stdinFail := &pipesStdinFailOpener{}
	stdin, stdout, err := pluginPipes(stdinFail)
	if err == nil {
		t.Fatal("注入 StdinPipe 失败后 pluginPipes 必须报错")
	}
	if stdin != nil || stdout != nil {
		t.Fatalf("错误返回的两个管道端必须为 nil，got stdin=%v stdout=%v", stdin, stdout)
	}
}

type pipesStdinFailOpener struct{}

func (pipesStdinFailOpener) StdinPipe() (io.WriteCloser, error) {
	return nil, errors.New("injected: stdin 管道失败")
}
func (pipesStdinFailOpener) StdoutPipe() (io.ReadCloser, error) {
	return nil, errors.New("unreachable: stdin 先败")
}
