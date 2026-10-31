package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrChecksumMismatch 表示 entrypoint 实际内容与声明的 sha256 不一致（N14）。
var ErrChecksumMismatch = errors.New("plugin: checksum 不匹配")

// maxManifestBytes 是 plugin.json 的大小上限（复证圈 5 sweep 发现 6）：manifest
// 是插件车道唯一读取前无大小上限的协议输入面（生产链 server.New 每会话装配
// → DiscoverContext → VerifyContext → Load），非敌意损坏（大二进制误拷成
// manifest）或本地低权写者可制造按文件大小的全量内存尖峰——运行期帧界纪律
// （MaxPluginFrameSize 4MiB、帧头行 8KiB）在 manifest 车道的对应物，超限按
// 帧结构损坏类错误拒绝并保留 Discover 的单目录 VerifyErr 可观测性。1 MiB
// 足以容纳任何合法清单（§O3 七必填 + 能力/语言列表）。
const maxManifestBytes = 1 << 20

// Load 读取并反序列化 dir/plugin.json。未知字段一律拒绝，
// 防止敌意 manifest 携带未定义语义。首个 JSON 值之后的首个值之外的尾随
// 非空白字节同样拒绝（复证圈 1 sweep 发现 3）：DisallowUnknownFields 只
// 作用于第一个文档，Decode 成功后若解码器仍有余量，说明清单文件的完整
// 字节不在校验面内——尾随文档/垃圾字节按帧结构损坏拒绝，纯空白尾随
// （JSON 流分帧的空白填充）不受影响。
func Load(dir string) (Manifest, error) {
	path := filepath.Join(dir, "plugin.json")
	// 单句柄消除 check-then-act（复证圈 7 sweep 发现 3）：旧形态 Stat 门尺
	// 寸后 os.ReadFile 全量读、读后无复核——并发写者以「小尺寸过 Stat、读中
	// 高速增长」绕过任何读前门。改为 open → 按句柄 Stat → readManifestBounded
	// （LimitReader cap+1 硬上界 + 读后复核）：读面恒 ≤ maxManifestBytes+1
	// 字节，与文件是否在读期间增长无关。
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin: 读取 plugin.json: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > maxManifestBytes {
		return Manifest{}, fmt.Errorf("plugin: plugin.json %d 字节超出 %d 上限（帧结构损坏类拒绝）", fi.Size(), maxManifestBytes)
	}
	b, err := readManifestBounded(f)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("plugin: 解析 plugin.json: %w", err)
	}
	if dec.More() {
		return Manifest{}, fmt.Errorf("plugin: plugin.json 首个 JSON 值之后存在尾随内容（清单必须是单一完整文档）")
	}
	return m, nil
}

// readManifestBounded 以 maxManifestBytes+1 的硬上界读 r 并复核读后长度
// （复证圈 7 sweep 发现 3）：即使底层文件在 Stat 之后被并发写者增长，读面
// 也恒有界，越界按帧结构损坏类错误拒绝——读后复核是读前门（句柄 Stat）
// 之外的第二条带，两条都在才闭合「按文件大小的全量内存尖峰」。
func readManifestBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("plugin: 读取 plugin.json: %w", err)
	}
	if len(b) > maxManifestBytes {
		return nil, fmt.Errorf("plugin: plugin.json 读入 %d 字节超出 %d 上限（帧结构损坏类拒绝）", len(b), maxManifestBytes)
	}
	return b, nil
}

// Verify 执行完整信任链：Load → Validate → entrypoint 路径安全检查 →
// 存在性检查 → sha256 比对。任一步失败即整体拒绝（N14：未验证元数据不可信）。
func Verify(dir string) (Manifest, error) {
	return VerifyContext(context.Background(), dir)
}

// VerifyContext 是带界变体（复证圈 2 审计发现 6）：sha256 复核逐块响应 ctx
// 取消——装配同步运行在 TCP accept 关键路径上时，大 entrypoint 的全文件
// 哈希不能让 accept 无界等待。
func VerifyContext(ctx context.Context, dir string) (Manifest, error) {
	m, err := Load(dir)
	if err != nil {
		return Manifest{}, err
	}
	if err := m.Validate(); err != nil {
		return m, err
	}
	if err := safeEntrypoint(m.Entrypoint); err != nil {
		return m, err
	}
	p := filepath.Join(dir, m.Entrypoint)
	if _, err := os.Stat(p); err != nil {
		return m, fmt.Errorf("plugin: entrypoint 缺失: %w", err)
	}
	if err := verifyChecksumContext(ctx, p, m.Checksum); err != nil {
		return m, err
	}
	return m, nil
}

// safeEntrypoint 强制 entrypoint 为与 plugin.json 同级的裸文件名，覆盖
// 穿越/绝对路径/盘符/裸点四类敌意样本（ponytail：支持子目录需加相对路径
// 遍历校验）。Verify 与 Launch 共用——一旦目录来源进入执行路径，形态检查
// 必须在启动时刻同样生效，否则 Join(dir, ep) 可被带分隔符的 ep 引出目录。
// 裸点 "." 通过 Base/ ".."/绝对/冒号四项检查（复证圈 3 sweep 发现 1），旧
// 实现只靠下游 openEntryFile 打不开目录而偶然 fail-closed——若未来
// openEntryFile 语义变化（如加 FILE_FLAG_BACKUP_SEMANTICS 支持目录），该
// 形态将滑入执行路径；形态门必须自持。其余点形态（"./" 等）已被 Base
// 不等检查拒绝；".." 含 ".." 同拒。
func safeEntrypoint(ep string) error {
	if ep == "." || ep != filepath.Base(ep) || strings.Contains(ep, "..") ||
		filepath.IsAbs(ep) || strings.ContainsRune(ep, ':') {
		return fmt.Errorf("plugin: 敌意 entrypoint 被拒绝: %q", ep)
	}
	return nil
}

// verifyChecksum 计算 path 处文件的 sha256 并与 manifest 声明值比对（N14）。
func verifyChecksum(path, declared string) error {
	return verifyChecksumContext(context.Background(), path, declared)
}

// verifyChecksumContext 是带界变体（复证圈 2 审计发现 6）：读循环逐块检查
// ctx——旧实现 io.Copy 无界，装配路径上的大 entrypoint 哈希不可取消。
func verifyChecksumContext(ctx context.Context, path, declared string) error {
	f, err := openEntryFile(path)
	if err != nil {
		return fmt.Errorf("plugin: 打开 entrypoint: %w", err)
	}
	defer f.Close()
	return verifyChecksumFromFile(ctx, f, declared)
}

// verifyChecksumFromFile 对「调用方持有的句柄」计算 sha256 并与 manifest
// 声明值比对（审计批 2 发现 3）。与按路径重开的变体的差别在这里是本质的：
// 哈希消费的就是调用方稍后要执行的那一份字节——调用方必须保持句柄存续到
// 执行时刻（Launch 持有到 Start 返回），哈希与执行才钉在同一份文件上；
// 哈希完就关句柄、随后按路径重开的写法留给敌意换包一个可重复的窗口。
func verifyChecksumFromFile(ctx context.Context, f *os.File, declared string) error {
	sum := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("plugin: 计算 sha256 被取消: %w", err)
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := sum.Write(buf[:n]); werr != nil {
				return fmt.Errorf("plugin: 计算 sha256: %w", werr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("plugin: 计算 sha256: %w", rerr)
		}
	}
	actual := "sha256:" + hex.EncodeToString(sum.Sum(nil))
	if actual != declared {
		return fmt.Errorf("%w: 声明 %s 实际 %s", ErrChecksumMismatch, declared, actual)
	}
	return nil
}
