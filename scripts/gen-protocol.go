//go:build ignore

// gen-protocol 写入或校验 internal/protocol/lsp/protocol.manifest——
// 手写 LSP 类型文件 types.go 的规范声明指纹（§Y5-4 可复现检查，零第三方依赖）。
//
//	go run scripts/gen-protocol.go          # 重新写入 manifest
//	go run scripts/gen-protocol.go -check   # 仅校验；有漂移则以退出码 1 结束
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/omnilsp/omni/internal/protocol/lsp"
)

func main() {
	check := flag.Bool("check", false, "校验磁盘上的 manifest 而非重写")
	flag.Parse()

	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *check {
		if err := lsp.CheckManifest(root); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("protocol.manifest 已是最新")
		return
	}
	content, err := lsp.BuildManifest()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path := filepath.Join(root, filepath.FromSlash(lsp.ManifestRelPath))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("已写入 %s（%d 行）\n", path, strings.Count(content, "\n"))
}

// moduleRoot 从当前目录向上定位 go.mod 所在的模块根。
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("在 %s 及其上层目录未找到 go.mod", dir)
		}
		dir = parent
	}
}
