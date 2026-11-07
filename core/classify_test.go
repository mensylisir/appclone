package core

import (
	"path/filepath"
	"testing"
)

// classifyEnv 造一个只有 home/xdg 布局的解析器（不碰真实主目录）。
func classifyEnv(t *testing.T) *PathResolver {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	return newResolver(home, home, 1000, func(string) string { return "" })
}

// §5 的规则表是"先命中先定"，所以每条断言都在钉**优先级**，不只是分类。
func TestClassifyPriorityOrder(t *testing.T) {
	r := classifyEnv(t)
	home := r.Home
	cases := []struct {
		path    string
		class   PathClass
		checked bool
		warning bool
		name    string
	}{
		// 1 公共目录：连其下任意路径都强制排除，任何规则不得翻案
		{filepath.Join(home, "Downloads"), ClassPublic, false, false, "公共目录本身"},
		{filepath.Join(home, "Downloads", "work", "a.txt"), ClassPublic, false, false, "公共目录之下"},
		// 公共目录里的 cache 目录仍是公共目录（规则 1 压过规则 4）
		{filepath.Join(home, "Pictures", "cache"), ClassPublic, false, false, "公共目录之下的 cache"},

		// 3 共享可写压过 `~/.config/*` 的默认勾选
		{filepath.Join(home, ".config", "fcitx5"), ClassShared, false, false, "输入法配置是硬排除"},
		{filepath.Join(home, ".config", "fcitx5", "profile"), ClassShared, false, false, "输入法配置之下"},
		{filepath.Join(home, ".config", "gtk-3.0", "bookmarks"), ClassShared, false, false, "GTK 书签"},
		{filepath.Join(home, ".local", "share", "recently-used.xbel"), ClassShared, false, false, "最近文件"},

		// 2 凭证
		{filepath.Join(home, ".local", "share", "keyrings", "login.keyring"), ClassKeyring, false, false, "keyring"},

		// 4 缓存
		{filepath.Join(home, ".cache", "DingTalk"), ClassCache, false, false, "缓存目录"},
		{filepath.Join(home, ".config", "App", "Cache"), ClassCache, false, false, "大写 Cache"},
		{filepath.Join(home, ".local", "share", "App", "cache"), ClassCache, false, false, "内层 cache"},

		// 5 XDG 配置/数据：默认勾选
		{filepath.Join(home, ".config", "DingTalk"), ClassConfig, true, false, "配置目录"},
		{filepath.Join(home, ".local", "share", "DingTalk"), ClassConfig, true, false, "数据目录"},
		{filepath.Join(home, ".local", "state", "App"), ClassConfig, true, false, "state 目录"},
		{filepath.Join(home, ".var", "app", "org.telegram.desktop"), ClassConfig, true, false, "Flatpak 应用目录"},

		// 7 顶层点目录：灰区（设计稿把它画成 config 且勾选，是演示简化）
		{filepath.Join(home, ".dingtalk"), ClassDotdir, false, true, "顶层点目录"},
		{filepath.Join(home, ".apprc"), ClassDotdir, false, true, "顶层点文件"},

		// 6 系统级
		{"/etc/DingTalk", ClassSystem, false, true, "/etc"},
		{"/opt/App", ClassSystem, false, true, "/opt"},

		// 8 数据库文件
		{filepath.Join(home, "notes.db"), ClassData, true, false, "顶层 db 文件"},
		{filepath.Join(home, "proj", "x.sqlite3"), ClassData, true, false, "sqlite"},
	}
	for _, c := range cases {
		class, checked, why, warn := r.Classify(c.path)
		if class != c.class || checked != c.checked || warn != c.warning {
			t.Fatalf("%s（%s）：得到 class=%s checked=%v warning=%v，期望 %s/%v/%v",
				c.name, c.path, class, checked, warn, c.class, c.checked, c.warning)
		}
		if why == "" {
			t.Fatalf("%s 没有 i18n 说明键：屏3 会显示空白的 why", c.name)
		}
	}
}

// 规则 4 的边界：`CacheManager` 里的 "cache" 只是名字的一部分，
// 用子串匹配会把整个应用目录错判成缓存 → 默认不勾 → 该隔离的没隔离。
func TestClassifyCacheNeedsPathSegment(t *testing.T) {
	r := classifyEnv(t)
	p := filepath.Join(r.Home, ".config", "App", "CacheManager", "settings.ini")
	if class, checked, _, _ := r.Classify(p); class != ClassConfig || !checked {
		t.Fatalf("CacheManager 不该被当成缓存目录，得到 %s/%v", class, checked)
	}
}

func TestMergeUpperTakesAppDirOwner(t *testing.T) {
	r := classifyEnv(t)
	entries := []UpperEntry{
		{Path: ".config/DingTalk/config.json", Kind: EntryFile},
		{Path: ".config/DingTalk/Cache/data_0", Kind: EntryFile},
		{Path: ".local/share/DingTalk/db/main.db", Kind: EntryFile},
		{Path: ".dingtalk/state", Kind: EntryFile},
		{Path: ".apprc", Kind: EntryFile},
		// XDG 根本身是纯父目录：绝不能成为候选
		{Path: ".config", Kind: EntryDir},
	}
	cands, whiteouts := r.MergeUpper(entries)
	if len(whiteouts) != 0 {
		t.Fatalf("这些条目里没有 whiteout，得到 %v", whiteouts)
	}
	want := map[string]int{ // 路径 → 观测到的写入条目数
		filepath.Join(r.Home, ".config", "DingTalk"):         2,
		filepath.Join(r.Home, ".local", "share", "DingTalk"): 1,
		filepath.Join(r.Home, ".dingtalk"):                   1,
		filepath.Join(r.Home, ".apprc"):                      1,
	}
	if len(cands) != len(want) {
		t.Fatalf("候选数 %d，期望 %d：%+v", len(cands), len(want), cands)
	}
	for _, c := range cands {
		n, ok := want[c.Path]
		if !ok {
			t.Fatalf("多出候选 %s（class=%s）", c.Path, c.Class)
		}
		if c.Observed != n {
			t.Fatalf("%s 的 Observed 应为 %d（写入条目数），得到 %d", c.Path, n, c.Observed)
		}
	}
	// 输出必须稳定：GUI 分组会跳、golden 也无从写起。
	sorted := true
	for i := 1; i < len(cands); i++ {
		if cands[i-1].Path > cands[i].Path {
			sorted = false
		}
	}
	if !sorted {
		t.Fatalf("候选必须按路径排序，得到 %+v", cands)
	}
}

// whiteout 是"删过"的证据，不能当写入候选：把它摆给用户勾选，
// 用户会去隔离一个已经删掉的路径。
func TestMergeUpperSeparatesWhiteouts(t *testing.T) {
	r := classifyEnv(t)
	entries := []UpperEntry{
		{Path: ".config/DingTalk/gone.json", Kind: EntryWhiteout},
		{Path: ".config/DingTalk/kept.json", Kind: EntryFile},
	}
	cands, whiteouts := r.MergeUpper(entries)
	if len(cands) != 1 || cands[0].Observed != 1 {
		t.Fatalf("whiteout 不该计入写入条目数，得到 %+v", cands)
	}
	if len(whiteouts) != 1 || filepath.Base(whiteouts[0]) != "gone.json" {
		t.Fatalf("whiteout 应单列，得到 %v", whiteouts)
	}
}

// 零写入判定看的是归并后的候选数：只写了缓存的应用不该生成"有 300 条写入"的假象。
func TestMergeUpperStillReportsCacheCandidates(t *testing.T) {
	r := classifyEnv(t)
	var entries []UpperEntry
	for i := 0; i < 300; i++ {
		entries = append(entries, UpperEntry{
			Path: filepath.Join(".cache", "App", "f"+string(rune('a'+i%26))), Kind: EntryFile,
		})
	}
	cands, _ := r.MergeUpper(entries)
	if len(cands) != 1 {
		t.Fatalf("300 个缓存条目应归并成 1 条候选，得到 %d", len(cands))
	}
	if cands[0].Class != ClassCache || cands[0].Checked {
		t.Fatalf("缓存候选必须默认不勾，得到 %s/%v", cands[0].Class, cands[0].Checked)
	}
}

// 档案落点：勾选的 config/data → paths，shared → shared_writes，
// keyring → unsupported（**不进 shared_writes**，否则 verify 的对账口径失真）。
func TestProfileFromCandidatesSplitsFields(t *testing.T) {
	r := classifyEnv(t)
	cands := []Candidate{
		{Path: filepath.Join(r.Home, ".config", "DingTalk"), Class: ClassConfig, Checked: true},
		{Path: filepath.Join(r.Home, ".dingtalk"), Class: ClassDotdir, Checked: false},
		{Path: filepath.Join(r.Home, ".cache", "DingTalk"), Class: ClassCache, Checked: false},
		{Path: filepath.Join(r.Home, ".config", "fcitx5"), Class: ClassShared},
		{Path: filepath.Join(r.Home, ".local", "share", "keyrings", "login.keyring"), Class: ClassKeyring},
	}
	p := r.ProfileFromCandidates("dingtalk", "overlay", cands)
	if len(p.Paths) != 1 || p.Paths[0] != "~/.config/DingTalk" {
		t.Fatalf("paths 只该有勾选的那条，得到 %v", p.Paths)
	}
	if len(p.SharedWrites) != 1 || p.SharedWrites[0] != "~/.config/fcitx5" {
		t.Fatalf("shared_writes 口径错：%v", p.SharedWrites)
	}
	if len(p.Unsupported) != 1 || p.Unsupported[0].Kind != "keyring" {
		t.Fatalf("keyring 必须进 unsupported：%v", p.Unsupported)
	}
	for _, s := range p.SharedWrites {
		if s == p.Unsupported[0].Path {
			t.Fatal("keyring 不得出现在 shared_writes 里（verify 的预期共享白名单）")
		}
	}
	if p.ProbeMethod != "overlay" || p.Version != ProfileVersion {
		t.Fatalf("档案元信息没写对：%+v", p)
	}
}
