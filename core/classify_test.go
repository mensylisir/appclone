package core

import (
	"os"
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

// 公共目录清单来自 ~/.config/user-dirs.dirs，键名是 `XDG_DOWNLOAD_DIR`。
// 解析时少剥一层前缀，中文系统的 `~/下载` 就永远不被认成公共目录——
// 而"公共目录出现在 paths 里即档案非法"这条红线全靠这份清单。
func TestPublicDirsParsesRealUserDirsFile(t *testing.T) {
	r := classifyEnv(t)
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "user-dirs.dirs")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# written by xdg-user-dirs-update\n" +
		"XDG_DOWNLOAD_DIR=\"$HOME/下载\"\n" +
		"XDG_DESKTOP_DIR=\"$HOME/桌面\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range r.PublicDirs() {
		got[p] = true
	}
	for _, want := range []string{
		filepath.Join(r.Home, "下载"),
		filepath.Join(r.Home, "桌面"),
	} {
		if !got[want] {
			t.Fatalf("公共目录应包含 %q，实际 %v", want, got)
		}
	}
	// 配置里没写的类别仍要有默认值，否则 Documents 之类的隔离检查会漏。
	if !got[filepath.Join(r.Home, "Documents")] {
		t.Fatalf("未配置的类别应回退到默认名，实际 %v", got)
	}
	if class, checked, _, _ := r.Classify(filepath.Join(r.Home, "下载", "a.txt")); class != ClassPublic || checked {
		t.Fatalf("中文公共目录之下应强制排除，得到 %s/%v", class, checked)
	}
}

// bookmarks 是文件级硬排除；被归并进 `~/.config/gtk-3.0` 后如果还按 config
// 勾选，隔离生效、书签消失——正是 §5 优先级 2 说的那种坑。
func TestMergeUpperEscalatesSharedWriteDirs(t *testing.T) {
	r := classifyEnv(t)
	cands, _ := r.MergeUpper([]UpperEntry{
		{Path: ".config/gtk-3.0/bookmarks", Kind: EntryFile},
		{Path: ".config/gtk-3.0/gtk.css", Kind: EntryFile},
	})
	if len(cands) != 1 {
		t.Fatalf("应归并成 1 条候选，得到 %+v", cands)
	}
	if cands[0].Class != ClassShared || cands[0].Checked {
		t.Fatalf("含 bookmarks 的目录必须走 shared_writes（默认不勾），得到 %s/%v",
			cands[0].Class, cands[0].Checked)
	}
	// 落档案时它必须进 shared_writes，不能进 paths。
	p := r.ProfileFromCandidates("x", "overlay", cands)
	if len(p.Paths) != 0 || len(p.SharedWrites) != 1 {
		t.Fatalf("落点错：paths=%v shared=%v", p.Paths, p.SharedWrites)
	}
}

// XDG 根的祖先（~/.local、~/.var）在 upper 里只因下级有写入而存在，
// 绝不是"应用目录"：勾中 ~/.var 会把所有 Flatpak 应用的数据一起隔离。
func TestMergeUpperDropsXdgRootAncestors(t *testing.T) {
	r := classifyEnv(t)
	cands, _ := r.MergeUpper([]UpperEntry{
		{Path: ".local", Kind: EntryDir},
		{Path: ".local/share/DingTalk/x", Kind: EntryFile},
		{Path: ".var", Kind: EntryDir},
		{Path: ".var/app/org.x.App/data", Kind: EntryFile},
	})
	want := map[string]bool{
		filepath.Join(r.Home, ".local", "share", "DingTalk"): true,
		filepath.Join(r.Home, ".var", "app", "org.x.App"):    true,
	}
	for _, c := range cands {
		if !want[c.Path] {
			t.Fatalf("不该出现候选 %s（XDG 根的祖先必须剔除）", c.Path)
		}
		delete(want, c.Path)
	}
	if len(want) != 0 {
		t.Fatalf("少了候选：%v", want)
	}
}

// Observed 是"写入条目数"：目录条目只是被顺带造出来的，不能算进去，
// 否则屏3 的"N 个写入条目"会偏大（文案红线）。
func TestMergeUpperCountsOnlyNonDirEntries(t *testing.T) {
	r := classifyEnv(t)
	cands, _ := r.MergeUpper([]UpperEntry{
		{Path: ".config/App", Kind: EntryDir},
		{Path: ".config/App/a.json", Kind: EntryFile},
		{Path: ".config/App/b.json", Kind: EntryFile},
	})
	if len(cands) != 1 || cands[0].Observed != 2 {
		t.Fatalf("应记 2 条写入条目，得到 %+v", cands)
	}
}

// 软链的 XDG 根是常态（`~/.config -> ~/cfg`）。upper 里记录的是**真实**路径，
// 只比词法根会认不出来，候选凭空消失——用户看到的是"没有可勾的配置目录"。
func TestClassifyFollowsSymlinkedXdgRoot(t *testing.T) {
	r := classifyEnv(t)
	realCfg := filepath.Join(r.Home, "cfg")
	if err := os.MkdirAll(filepath.Join(realCfg, "App"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realCfg, filepath.Join(r.Home, ".config")); err != nil {
		t.Skipf("这个环境不支持软链：%v", err)
	}
	// 真实形态（upper 里就是这种）
	realPath := filepath.Join(realCfg, "App")
	if class, checked, _, _ := r.Classify(realPath); class != ClassConfig || !checked {
		t.Fatalf("软链目标的配置目录应判 config 且默认勾选，得到 %s/%v", class, checked)
	}
	// 词法形态（用户/文档里写的 `~/.config/App`）
	lexPath := filepath.Join(r.Home, ".config", "App")
	if class, checked, _, _ := r.Classify(lexPath); class != ClassConfig || !checked {
		t.Fatalf("软链路径本身也应判 config，得到 %s/%v", class, checked)
	}
	// 归并：upper 里给真实路径，候选不能丢
	cands, _ := r.MergeUpper([]UpperEntry{{Path: "cfg/App/x.json", Kind: EntryFile}})
	if len(cands) != 1 {
		t.Fatalf("软链 XDG 根下的写入应归并出 1 条候选，得到 %+v", cands)
	}
	if cands[0].Class != ClassConfig || !cands[0].Checked {
		t.Fatalf("候选应可隔离，得到 %s/%v", cands[0].Class, cands[0].Checked)
	}
}

// 同一条路径只能落在一个字段里：同时进 paths 与 shared_writes 会让 bwrap 先隔离
// 再共享，结果取决于参数顺序——那是"看起来设置过、实际不确定"的状态。
func TestProfileFromCandidatesDropsCrossFieldDuplicates(t *testing.T) {
	r := classifyEnv(t)
	same := filepath.Join(r.Home, ".config", "App")
	out := r.ProfileFromCandidates("x", "overlay", []Candidate{
		{Path: same, Class: ClassConfig, Checked: true},
		{Path: same, Class: ClassShared},
	})
	total := len(out.Paths) + len(out.SharedWrites)
	if total != 1 {
		t.Fatalf("同一路径只该出现一次：paths=%v shared=%v", out.Paths, out.SharedWrites)
	}
	if len(out.Unsupported) != 0 {
		t.Fatalf("这条不该产生 unsupported：%+v", out.Unsupported)
	}
}

// 收紧后的精确路径不能误伤正常应用：应用自己数据目录里的同名文件
// 不是"GTK 最近文件"，把它标成 shared 会把整个应用目录升级成共享。
func TestClassifyNestedXbelIsNotShared(t *testing.T) {
	r := classifyEnv(t)
	nested := filepath.Join(r.Home, ".config", "App", "recently-used.xbel")
	if r.isSharedWrite(nested) {
		t.Fatal("嵌套的 recently-used.xbel 不该被当成共享文件")
	}
	if class, _, _, _ := r.Classify(nested); class == ClassShared {
		t.Fatalf("嵌套文件分类不能是 shared，得到 %v", class)
	}
}

// HOME 是软链时，upper 里是真实路径、档案里是词法路径；精确路径判定必须
// 两种形态都能认出来，否则书签/最近文件会被隔离（用户看到"书签全没了"）。
func TestClassifySharedXbelWithSymlinkedHome(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "realhome")
	if err := os.MkdirAll(realHome, 0o700); err != nil {
		t.Fatal(err)
	}
	homeLink := filepath.Join(root, "home")
	if err := os.Symlink(realHome, homeLink); err != nil {
		t.Fatal(err)
	}
	r := newResolver(homeLink, realHome, 1000, func(string) string { return "" })
	for _, name := range []string{"recently-used.xbel", "user-places.xbel"} {
		abs := filepath.Join(realHome, ".local", "share", name)
		if !r.isSharedWrite(abs) {
			t.Fatalf("%s 在软链 HOME 下必须仍判共享", name)
		}
		if !r.isSharedWrite(filepath.Join(homeLink, ".local", "share", name)) {
			t.Fatalf("%s 的词法路径也必须判共享", name)
		}
	}
	if !r.isSharedWrite(filepath.Join(realHome, ".gtk-bookmarks")) {
		t.Fatal("~/.gtk-bookmarks 在软链 HOME 下必须仍判共享")
	}
}
