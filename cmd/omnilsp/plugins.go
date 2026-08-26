// plugins.go 实现 `omnilsp plugins` 子命令：进程外插件的静态巡检入口。
// 仅依赖 internal/plugin 与标准库（§U1/§O5 依赖方向约束）。
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/omnilsp/omni/internal/plugin"
)

// cmdPlugins 处理 `plugins --root <dir> list|validate <dir>`。
func cmdPlugins(args []string) error {
	fs := flag.NewFlagSet("plugins", flag.ExitOnError)
	root := fs.String("root", ".", "插件根目录（list 扫描其直接子目录）")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return fmt.Errorf("缺少子命令：list | validate <dir>")
	}

	switch rest[0] {
	case "list":
		return pluginsList(*root)
	case "validate":
		if len(rest) < 2 {
			return fmt.Errorf("validate 需要一个插件目录参数")
		}
		return pluginsValidate(rest[1])
	default:
		return fmt.Errorf("未知子命令 %q（可用：list | validate <dir>）", rest[0])
	}
}

// pluginsList 以表格输出 id/version/state/checksum ok。state 由信任链结果推导：
// 校验通过为 verified，否则 failed（运行态由服务进程内的 Manager 维护）。
func pluginsList(root string) error {
	mgr := plugin.NewManager()
	ds := mgr.Discover(root)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tVERSION\tSTATE\tCHECKSUM")
	for _, d := range ds {
		state, ok := "verified", "ok"
		if d.VerifyErr != nil {
			state, ok = "failed", "FAIL"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Manifest.ID, d.Manifest.Version, state, ok)
	}
	w.Flush()
	if len(ds) == 0 {
		fmt.Printf("(在 %s 下未发现插件)\n", root)
	}
	return nil
}

// pluginsValidate 对单目录输出校验报告：信任链各步结果 + manifest 摘要。
func pluginsValidate(dir string) error {
	m, err := plugin.Verify(dir)
	if err != nil {
		return fmt.Errorf("validate %s: %w", dir, err)
	}
	fmt.Printf("PASS %s\n", dir)
	fmt.Printf("  id          %s\n", m.ID)
	fmt.Printf("  version     %s\n", m.Version)
	fmt.Printf("  apiVersion  %s\n", m.APIVersion)
	fmt.Printf("  entrypoint  %s\n", m.Entrypoint)
	fmt.Printf("  languages   %v\n", m.Languages)
	fmt.Printf("  caps        %v\n", m.Capabilities)
	fmt.Printf("  checksum    %s\n", m.Checksum)
	if m.Warning != "" {
		fmt.Printf("  warning     %s\n", m.Warning)
	}
	return nil
}
