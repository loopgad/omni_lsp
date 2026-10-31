package uri

// Tests for goal.md §D2/§D3 and §S2 property: parse -> canonical -> parse
// preserves identity; path conversion roundtrips for well-formed inputs.

import (
	"runtime"
	"strings"
	"testing"
)

func TestParseCanonicalIdempotent(t *testing.T) {
	inputs := []string{
		"file:///c:/Users/test/main.go",
		"file:///C:/Users/test/main.go",
		"file:///home/user/main.go",
		"file://server/share/proj/a.cpp",
		"file:///home/user/%E4%B8%AD%E6%96%87.go",
		"file:///home/user/中文.go",
		"https://example.com/x.txt",
	}
	for _, in := range inputs {
		u1, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		u2, err := Parse(u1.Canonical())
		if err != nil {
			t.Fatalf("re-Parse(%q): %v", u1.Canonical(), err)
		}
		if u1.Canonical() != u2.Canonical() {
			t.Errorf("idempotence broken: %q -> %q vs %q", in, u1.Canonical(), u2.Canonical())
		}
	}
}

func TestDriveLetterCaseCanonicalizes(t *testing.T) {
	a, err := Parse("file:///C:/x/y.go")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("file:///c:/x/y.go")
	if err != nil {
		t.Fatal(err)
	}
	if a.Canonical() != b.Canonical() {
		t.Errorf("drive case must not affect identity:\n %q\n %q", a.Canonical(), b.Canonical())
	}
}

func TestEncodedDriveColonCanonicalizesWithoutChangingEscapedPathData(t *testing.T) {
	literal, err := Parse("file:///C:/Users/test/main.go")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Parse("file:///c%3A/Users/test/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if literal.Canonical() != encoded.Canonical() {
		t.Errorf("encoded drive colon must preserve Windows file identity:\n %q\n %q", literal.Canonical(), encoded.Canonical())
	}
	withEscapedUnreserved, err := Parse("file:///c:/Users/A%20B/%7Espace.py")
	if err != nil {
		t.Fatal(err)
	}
	withLiteralUnreserved, err := Parse("file:///C:/Users/A%20B/~space.py")
	if err != nil {
		t.Fatal(err)
	}
	if withEscapedUnreserved.Canonical() != withLiteralUnreserved.Canonical() {
		t.Errorf("encoded unreserved path characters must preserve identity:\n %q\n %q", withEscapedUnreserved.Canonical(), withLiteralUnreserved.Canonical())
	}

	withEscapedSlash, err := Parse("file:///C:/Users/a%2Fb/main.go")
	if err != nil {
		t.Fatal(err)
	}
	withPathSeparator, err := Parse("file:///C:/Users/a/b/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if withEscapedSlash.Canonical() == withPathSeparator.Canonical() {
		t.Errorf("canonicalization must preserve escaped path data: %q", withEscapedSlash.Canonical())
	}
}

func TestDisplaySpellingPreserved(t *testing.T) {
	in := "file:///C:/Users/Test/Main.GO"
	u, err := Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != in {
		t.Errorf("display corrupted: %q", u.String())
	}
}

func TestFromPathRoundTrip(t *testing.T) {
	var paths []string
	if runtimeIsWindows() {
		paths = []string{`C:\x\y\main.go`, `C:\中文\测试.go`, `\\server\share\a.cpp`}
	} else {
		paths = []string{"/x/y/main.go", "/中文/测试.go"}
	}
	for _, p := range paths {
		u := FromPath(p)
		got, err := u.Path()
		if err != nil {
			t.Fatalf("Path(%q): %v", p, err)
		}
		if got != p {
			t.Errorf("path roundtrip: got %q, want %q (uri=%s)", got, p, u.String())
		}
	}
}

func TestNonFileSchemeRejected(t *testing.T) {
	u, err := Parse("untitled:Untitled-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Path(); err == nil {
		t.Error("non-file URI must not produce a filesystem path")
	}
	if u.IsFile() {
		t.Error("IsFile should be false for untitled scheme")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "no-scheme-here", "main.go"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}

func TestWindowsPathSemantics(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only assertions")
	}
	u, err := Parse("file:///d:/proj/main.go")
	if err != nil {
		t.Fatal(err)
	}
	p, err := u.Path()
	if err != nil {
		t.Fatal(err)
	}
	if p != `D:\proj\main.go` {
		t.Errorf("got %q, want D:\\proj\\main.go", p)
	}

	unc, err := Parse("file://SERVER/share/f.h")
	if err != nil {
		t.Fatal(err)
	}
	up, err := unc.Path()
	if err != nil {
		t.Fatal(err)
	}
	want := `\\SERVER\share\f.h`
	if up != want && up != `\\server\share\f.h` {
		t.Errorf("UNC path = %q, want %q family", up, want)
	}
}

// TestCanonicalFoldsFileHostCase（复证圈 2 battery 守卫，FuzzURI crasher
// a074a1c9171fbfa9）：file-URI 的 host 按 RFC 3986 §3.2.2 大小写不敏感，
// Canonical 必须折叠 host 大小写——Path() 已对 UNC host 做 strings.ToLower
// （uri.go Path 的 host := strings.ToLower(...)），两个映射若不一致，
// pathToUri(path) 与 pathToUri(uriToPath(pathToUri(path))) 对含大写 host 的
// UNC 不收敛：crasher 链 file:////A →（uriToPath→"\A"→FromPath UNC 分支）
// file://A →（Path ToLower→"\a"）file://a，第二轮才达不动点，
// uri_fuzz_test.go 的稳定性断言失败。折叠后 back 一步即 "file://a"。
func TestCanonicalFoldsFileHostCase(t *testing.T) {
	u, err := Parse("file://A/x")
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Canonical(); got != "file://a/x" {
		t.Fatalf("Canonical(%q) = %q, want file://a/x（host 大小写必须折叠，与 Path() 的 ToLower 一致）",
			"file://A/x", got)
	}
	// Canonical 幂等（§S2）在折叠下不回退。
	u2, err := Parse(u.Canonical())
	if err != nil {
		t.Fatalf("canonical not parseable: %v", err)
	}
	if u2.Canonical() != u.Canonical() {
		t.Fatalf("idempotence broken after host fold: %q vs %q", u2.Canonical(), u.Canonical())
	}
	// 从大写 UNC 形态 OS 路径出发：FromPath 产出的 Canonical 一步即折叠，
	// 不再依赖 Path() 的二次小写化（crasher 的 lossy 步）。
	uncPath := string([]byte{'\\', '\\', 'A', '\\', 'x'}) // \\A\x
	if got := FromPath(uncPath).Canonical(); got != "file://a/x" {
		t.Fatalf("FromPath(大写 UNC host 路径 %q).Canonical() = %q, want file://a/x", uncPath, got)
	}
	// 大小写不同的 UNC host 折叠到同一 canonical 身份（RFC 3986 §3.2.2）。
	other, err := Parse("file://a/x")
	if err != nil {
		t.Fatal(err)
	}
	if other.Canonical() != u.Canonical() {
		t.Fatalf("host 大小写变体必须同身份：%q vs %q", other.Canonical(), u.Canonical())
	}
}

// TestFromPathNeverEmitsUnparseableURI（复证圈 2 battery 守卫，FuzzURI
// crasher 23b5f660a4b884f0）：FromPath 的 UNC 分支曾对 \\0<空格> 形态路径
// 盲目拼接 "file:" + "//0 " ——空格进入 host，产出连自身 Parse 都拒绝的
// URI（canonical 永不可再解析，uri→path→uri 往返在 uri_fuzz_test.go:58
// 断言死亡）。非控制字符路径经 FromPath 产出的 canonical 必须可再解析
// （控制字符路径仍走 mustFile 的 opaque 驻留豁免面，见 uri_fuzz_test.go
// :51-53 的豁免清单）；Windows 上该路径还需与 Path() 往返一致（UNC 探测
// 失败回落路径拼写后，字节进入 path 分量，%20 转义无损）。
func TestFromPathNeverEmitsUnparseableURI(t *testing.T) {
	// `` = 反斜杠 0 空格——crasher 的 uriToPath 产物（非控制字符路径）。
	badUNC := string([]byte{'\\', '\\', '0', ' '})
	u := FromPath(badUNC)
	cu, err := Parse(u.Canonical())
	if err != nil {
		t.Fatalf("FromPath(%q).Canonical() = %q 不可再解析: %v（FromPath 不得产出自身拒绝的 URI）",
			badUNC, u.Canonical(), err)
	}
	if cu.Canonical() != u.Canonical() {
		t.Fatalf("FromPath 产出的 canonical 不幂等: %q vs %q", cu.Canonical(), u.Canonical())
	}
	if runtime.GOOS == "windows" {
		p, err := u.Path()
		if err != nil {
			t.Fatalf("Path(%q): %v", u.Canonical(), err)
		}
		if p != badUNC {
			t.Fatalf("路径往返断裂: Path() = %q, want %q", p, badUNC)
		}
	}
	// 空 host 的多斜杠形状（crasher ce45fb56088df37c：file:////// →
	// file://// → file: 斜杠逐轮减半）：UNC 分支不得吞吃空 host 路径的前导
	// 斜杠——Path() 往返必须逐字节一致（Windows），canonical 可再解析。
	slashed := string([]byte{'\\', '\\', '\\', '\\'}) // \\\\
	u3 := FromPath(slashed)
	cu3, err := Parse(u3.Canonical())
	if err != nil {
		t.Fatalf("FromPath(%q).Canonical() = %q 不可再解析: %v", slashed, u3.Canonical(), err)
	}
	if cu3.Canonical() != u3.Canonical() {
		t.Fatalf("多斜杠路径 canonical 不幂等: %q vs %q", cu3.Canonical(), u3.Canonical())
	}
	if runtime.GOOS == "windows" {
		p3, err := u3.Path()
		if err != nil {
			t.Fatalf("Path(%q): %v", u3.Canonical(), err)
		}
		if p3 != slashed {
			t.Fatalf("多斜杠路径往返断裂: Path() = %q, want %q（UNC 分支吞吃了空 host 的前导斜杠）", p3, slashed)
		}
	}
}

// TestPathDriveHeuristicRequiresASCIILetter（复证圈 2 battery 守卫，FuzzURI
// crasher 541391ec4a3ad20c）：Path() 的盘符提升只对 "/<ASCII字母>:..." 生效
// ——旧实现只查 path[2]==':'，任何字节落在盘符位都会被 strings.ToUpper 腐
// 蚀（非法 UTF-8 → U+FFFD）并丢前导斜杠，残迹 "\uFFFD:" 再经 FromPath 变成
// host，往返不收敛。钉住：非字母盘符位的字节必须原样保留（无 U+FFFD 腐蚀
// 签名），且 canonical 往返稳定。
func TestPathDriveHeuristicRequiresASCIILetter(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only drive heuristic")
	}
	u, err := Parse("file:/%A6:")
	if err != nil {
		t.Fatal(err)
	}
	p, err := u.Path()
	if err != nil {
		t.Fatal(err)
	}
	// 腐蚀判定必须按字节：raw 字节 0xA6 按 rune 解码本就是 U+FFFD（RuneError），
	// ContainsRune 区分不了「腐蚀」与「原样保留」——腐蚀签名是 ToUpper 产出的
	// 三字节序列 EF BF BD（字符串字面量 "\uFFFD" 即该序列），原样保留则是单
	// 字节 0xA6。
	if strings.Contains(p, "\uFFFD") {
		t.Fatalf("Path() = %q 含 U+FFFD 腐蚀签名（EF BF BD）——盘符启发式把非 ASCII 字节替换了（必须原样保留）", p)
	}
	if strings.IndexByte(p, 0xA6) < 0 {
		t.Fatalf("Path() = %q 丢失了原始字节 0xA6", p)
	}
	// 往返稳定：uriToPath 产物再经 FromPath.Canonical 必须回到同一 canonical。
	back := FromPath(p).Canonical()
	u2, err := Parse(back)
	if err != nil {
		t.Fatalf("FromPath(%q).Canonical() = %q 不可再解析: %v", p, back, err)
	}
	p2, err := u2.Path()
	if err != nil || p2 != p {
		t.Fatalf("路径往返不稳定: %q → %q → %q (err=%v)", p, back, p2, err)
	}
}
