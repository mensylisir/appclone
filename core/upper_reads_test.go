package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// testHomeXDG 把 XDG 根放回 $HOME 之下（真实桌面默认形态）。testEnv 故意把
// XDG 放到家目录外，便于隔离写入；但读证据是相对 $HOME 的，只有这种布局
// 才能覆盖 ~/.config、~/.cache 的 owner 归并与噪声清单。
func testHomeXDG(t *testing.T) (*Env, *PathResolver) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	r := newResolver(home, home, 1000, func(string) string { return "" })
	e := NewEnvAt(filepath.Join(home, ".local", "share", "appclone"), r)
	e.TmpRoot = filepath.Join(root, "tmp")
	if err := os.MkdirAll(e.TmpRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return e, r
}

func relHome(t *testing.T, r *PathResolver, abs string) string {
	t.Helper()
	rel, err := filepath.Rel(r.Home, abs)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(rel)
}

func candByPath(cands []Candidate, path string) *Candidate {
	for i := range cands {
		if samePathLoose(cands[i].Path, path) {
			return &cands[i]
		}
	}
	return nil
}

// 强凭据读证据：owner 即使写在 $HOME 顶层点目录（灰区）也必须默认勾选，
// 并且派生出的 credential_paths 是**实际观察到**的凭据文件，不是整个 owner。
func TestMergeUpperWithReadsStrongCredential(t *testing.T) {
	e, r := testHomeXDG(t)
	dot := filepath.Join(r.Home, ".zcode")
	reads := []string{
		".zcode/credentials.json",
		".zcode/settings.json",
		".config/dconf/user",
		".cache/zcode/blob",
		"Downloads/export.txt",
	}
	cands, _, hidden, warns := r.MergeUpperWithReads(nil, reads, nil, e.Base, "zcode")
	c := candByPath(cands, dot)
	if c == nil {
		t.Fatalf("应生成 ~/.zcode 候选：%+v", cands)
	}
	if c.Class != ClassDotdir || !c.Checked || c.Warning {
		t.Fatalf("强凭据应把灰区升级为 checked：%+v", c)
	}
	if !c.ContainsCredential || c.Credential != CredentialNone {
		t.Fatalf("父目录应标 ContainsCredential 而不是自身 strong：%+v", c)
	}
	if !samePathLoose(c.CredentialPath, filepath.Join(dot, "credentials.json")) {
		t.Fatalf("凭据路径应是实际文件：%q", c.CredentialPath)
	}
	if c.Reads != 2 {
		t.Fatalf("Reads 应按路径去重：%d", c.Reads)
	}
	if len(warns) != 0 {
		t.Fatalf("不应有告警：%v", warns)
	}
	// dconf 噪声、cache 读、公共读都不生成可勾选行；公共行仍以 owner 出现。
	if p := candByPath(cands, filepath.Join(r.XDG["CONFIG_HOME"], "dconf")); p != nil {
		t.Fatalf("dconf 不应成为候选：%+v", p)
	}
	if p := candByPath(cands, filepath.Join(r.XDG["CACHE_HOME"], "zcode")); p != nil {
		t.Fatalf("cache 读不应生成候选：%+v", p)
	}
	pub := candByPath(cands, filepath.Join(r.Home, "Downloads"))
	if pub == nil || pub.Class != ClassPublic || pub.Checked {
		t.Fatalf("公共读应是不可勾选的 public 行：%+v", pub)
	}
	// hidden 只统计"只读非凭据行超过 20 条"的溢出；dconf/cache 这类根本不
	// 生成候选的读路径不占这个计数（否则确认页会报一个对不上的数字）。
	if hidden != 0 {
		t.Fatalf("噪声/cache 不计入 hidden，得到 %d", hidden)
	}
}

// 弱证据只展示、不升级；同名应用目录没有凭据证据也不勾选。
func TestMergeUpperWithReadsWeakAndAppHome(t *testing.T) {
	e, r := testHomeXDG(t)
	cands, _, _, _ := r.MergeUpperWithReads(nil, []string{
		".foo/session.json",
		".zcode/settings.json",
	}, nil, e.Base, "zcode")
	foo := candByPath(cands, filepath.Join(r.Home, ".foo"))
	if foo == nil || foo.Checked || !foo.Warning || foo.Credential != CredentialWeak {
		t.Fatalf("弱证据不能升级灰区：%+v", foo)
	}
	if foo.Why != "probe.why.read_credential_weak" {
		t.Fatalf("弱证据文案键：%q", foo.Why)
	}
	zc := candByPath(cands, filepath.Join(r.Home, ".zcode"))
	if zc == nil || zc.Checked || zc.Why != "probe.why.app_home" {
		t.Fatalf("同名应用目录无凭据证据必须不勾选：%+v", zc)
	}
}

// 噪声例外只给"强凭据且不在共享凭据库"的路径；.pki/keyrings 仍被排除。
func TestMergeUpperWithReadsNoiseException(t *testing.T) {
	e, r := testHomeXDG(t)
	cands, _, _, _ := r.MergeUpperWithReads(nil, []string{
		".config/gnome-shell/credentials",
		".pki/nssdb/credentials",
		".local/share/keyrings/default.keyring",
	}, nil, e.Base, "demo")
	gnome := candByPath(cands, filepath.Join(r.XDG["CONFIG_HOME"], "gnome-shell"))
	if gnome == nil || !gnome.ContainsCredential || !gnome.Checked {
		t.Fatalf("gnome 目录下的强凭据应保留：%+v", gnome)
	}
	if candByPath(cands, filepath.Join(r.Home, ".pki")) != nil {
		t.Fatal(".pki 是共享凭据库，不能被噪声例外救回来")
	}
	kr := candByPath(cands, filepath.Join(r.XDG["DATA_HOME"], "keyrings", "default.keyring"))
	if kr == nil || kr.Class != ClassKeyring || kr.Checked {
		t.Fatalf("keyring 应是独立不可勾选行：%+v", kr)
	}
}

// 写候选已存在时读证据只升级、不改 Class、不重复；cache 写行锁定。
func TestMergeUpperWithReadsUpgradesExistingAndLocksCache(t *testing.T) {
	e, r := testHomeXDG(t)
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "foo")
	entries := []UpperEntry{
		{Path: relHome(t, r, filepath.Join(cfg, "data.db")), Kind: EntryFile},
		{Path: relHome(t, r, filepath.Join(r.XDG["CACHE_HOME"], "foo", "blob")), Kind: EntryFile},
	}
	cands, _, _, _ := r.MergeUpperWithReads(entries, []string{
		".config/foo/credentials.json",
	}, nil, e.Base, "foo")
	var foo *Candidate
	for i := range cands {
		if samePathLoose(cands[i].Path, cfg) {
			foo = &cands[i]
		}
	}
	if foo == nil {
		t.Fatalf("写候选应存在：%+v", cands)
	}
	if foo.Class != ClassConfig || foo.Observed != 1 || foo.Reads != 1 {
		t.Fatalf("读证据应合并进写候选：%+v", foo)
	}
	if !foo.ContainsCredential || !foo.Checked {
		t.Fatalf("读凭据应升级写候选：%+v", foo)
	}
	cache := candByPath(cands, filepath.Join(r.XDG["CACHE_HOME"], "foo"))
	if cache == nil || !cache.Locked || cache.Checked || cache.DisplayClass != ClassCache {
		t.Fatalf("cache 写行必须锁定：%+v", cache)
	}
}

// 只读非凭据行全局最多 20 条；凭据行不占配额。
func TestMergeUpperWithReadsLimit(t *testing.T) {
	e, r := testHomeXDG(t)
	var reads []string
	for i := 0; i < 25; i++ {
		reads = append(reads, filepath.ToSlash(filepath.Join(".config", "app"+string(rune('a'+i)), "settings.json")))
	}
	for i := 0; i < 3; i++ {
		reads = append(reads, filepath.ToSlash(filepath.Join(".config", "cred"+string(rune('a'+i)), "credentials")))
	}
	cands, _, hidden, _ := r.MergeUpperWithReads(nil, reads, nil, e.Base, "demo")
	plain, cred := 0, 0
	for _, c := range cands {
		if c.ContainsCredential || c.Credential != CredentialNone {
			cred++
		} else {
			plain++
		}
	}
	if plain != 20 || cred != 3 {
		t.Fatalf("20 条只读配额 + 凭据豁免：plain=%d cred=%d", plain, cred)
	}
	if hidden != 5 {
		t.Fatalf("超出的 5 条应计入 ReadsHidden：%d", hidden)
	}
}

// DeriveProfile：强凭据进入 credential_paths；弱证据不进入；ReadTrace 原样落盘。
func TestDeriveProfileCredentials(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	dot := filepath.Join(r.Home, ".zcode")
	cands := []Candidate{{
		Path: dot, Class: ClassDotdir, DisplayClass: ClassDotdir, Checked: true,
		CredentialPath:     filepath.Join(dot, "credentials.json"),
		ContainsCredential: true,
	}}
	p := r.DeriveProfile(Profile{
		App: "zcode", Exec: "/usr/bin/zcode", Ignored: DefaultIgnored(),
	}, nil, "overlay", "ok", cands)
	if len(p.Paths) != 1 || p.Paths[0] != "~/.zcode" {
		t.Fatalf("paths：%v", p.Paths)
	}
	if len(p.CredentialPaths) != 1 || p.CredentialPaths[0] != "~/.zcode/credentials.json" {
		t.Fatalf("credential_paths 应是实际凭据文件：%v", p.CredentialPaths)
	}
	if p.ReadTrace != "ok" {
		t.Fatalf("read_trace：%q", p.ReadTrace)
	}
	if p.Exec != "/usr/bin/zcode" || p.Ignored == nil {
		t.Fatalf("非路径字段应保留：%+v", p)
	}

	weak := r.DeriveProfile(Profile{App: "zcode", Ignored: DefaultIgnored()}, nil, "overlay", "partial", []Candidate{{
		Path: dot, Class: ClassDotdir, DisplayClass: ClassDotdir, Checked: true,
		Credential: CredentialWeak,
	}})
	if len(weak.CredentialPaths) != 0 {
		t.Fatalf("弱证据不能进入 credential_paths：%v", weak.CredentialPaths)
	}
}

// 重探凭据并集：旧父→新子收窄、旧子→新父保留、cache 凭据剔除。
func TestDeriveProfileCredentialAdaptation(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "foo")
	sub := filepath.Join(cfg, "sub")
	oldParent := Profile{CredentialPaths: []string{r.ToStoreForm(cfg)}}
	p := r.DeriveProfile(Profile{App: "foo", Ignored: DefaultIgnored()}, &oldParent, "overlay", "ok", []Candidate{{
		Path: sub, Class: ClassConfig, DisplayClass: ClassConfig, Checked: true,
	}})
	if len(p.CredentialPaths) != 1 || p.CredentialPaths[0] != r.ToStoreForm(sub) {
		t.Fatalf("旧父凭据应收窄为新子项：%v", p.CredentialPaths)
	}
	if !containsString(p.Warnings, "credential_scope_narrowed") {
		t.Fatalf("应记录 credential_scope_narrowed：%v", p.Warnings)
	}

	oldChild := Profile{CredentialPaths: []string{r.ToStoreForm(filepath.Join(cfg, "session"))}}
	p = r.DeriveProfile(Profile{App: "foo", Ignored: DefaultIgnored()}, &oldChild, "overlay", "ok", []Candidate{{
		Path: cfg, Class: ClassConfig, DisplayClass: ClassConfig, Checked: true,
	}})
	if len(p.CredentialPaths) != 1 || p.CredentialPaths[0] != r.ToStoreForm(filepath.Join(cfg, "session")) {
		t.Fatalf("旧子凭据在新父路径下应保留：%v", p.CredentialPaths)
	}

	oldCache := Profile{CredentialPaths: []string{r.ToStoreForm(filepath.Join(r.XDG["CACHE_HOME"], "foo", "session"))}}
	p = r.DeriveProfile(Profile{App: "foo", Ignored: DefaultIgnored()}, &oldCache, "overlay", "ok", []Candidate{{
		Path: cfg, Class: ClassConfig, DisplayClass: ClassConfig, Checked: true,
	}})
	if len(p.CredentialPaths) != 0 {
		t.Fatalf("cache 凭据不能保留：%v", p.CredentialPaths)
	}
	if !containsString(p.Warnings, "cache_credential") {
		t.Fatalf("应记录 cache_credential：%v", p.Warnings)
	}
}

// 取消勾选父路径 → credential_paths 同步收缩（不能留下不在 paths 下的凭据）。
func TestDeriveProfileUncheckShrinksCredentials(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "foo")
	base := Profile{CredentialPaths: []string{r.ToStoreForm(filepath.Join(cfg, "session"))}}
	p := r.DeriveProfile(Profile{App: "foo", Ignored: DefaultIgnored()}, &base, "overlay", "ok", []Candidate{{
		Path: cfg, Class: ClassConfig, DisplayClass: ClassConfig, Checked: false,
	}})
	if len(p.Paths) != 0 || len(p.CredentialPaths) != 0 {
		t.Fatalf("取消勾选后 paths/credentials 都应为空：%v / %v", p.Paths, p.CredentialPaths)
	}
}

// 档案 → 候选：字段路由固定 Config，DisplayClass 只展示；cache 锁定、public 不勾选。
func TestCandidatesFromProfile(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	dot := filepath.Join(r.Home, ".zcode")
	cache := filepath.Join(r.XDG["CACHE_HOME"], "foo")
	pub := filepath.Join(r.Home, "Downloads")
	sys := filepath.Join("/opt", "demo")
	shared := filepath.Join(r.XDG["CONFIG_HOME"], "gtk-3.0", "bookmarks")
	keyring := filepath.Join(r.XDG["DATA_HOME"], "keyrings", "k.keyring")
	p := Profile{
		Paths:        []string{r.ToStoreForm(dot), r.ToStoreForm(cache), r.ToStoreForm(pub), sys},
		SharedWrites: []string{r.ToStoreForm(shared)},
		Unsupported:  []Unsupported{{Kind: "keyring", Path: r.ToStoreForm(keyring), Reason: "probe.why.keyring"}},
		CredentialPaths: []string{
			r.ToStoreForm(filepath.Join(dot, "credentials.json")),
		},
	}
	cands, warns := r.CandidatesFromProfileWithWarns(p)
	zc := candByPath(cands, dot)
	if zc == nil || zc.Class != ClassConfig || zc.DisplayClass != ClassDotdir || !zc.Checked {
		t.Fatalf("显式路径 Class 固定 Config、展示用 DisplayClass：%+v", zc)
	}
	if !zc.ContainsCredential {
		t.Fatalf("子凭据应让父行显示 ContainsCredential：%+v", zc)
	}
	ca := candByPath(cands, cache)
	if ca == nil || ca.Class != ClassCache || !ca.Locked || ca.Checked {
		t.Fatalf("cache 行应锁定：%+v", ca)
	}
	pu := candByPath(cands, pub)
	if pu == nil || pu.Class != ClassConfig || pu.DisplayClass != ClassPublic || pu.Checked {
		t.Fatalf("既有 public 路径默认不勾选：%+v", pu)
	}
	// /opt 可能被 Resolve 成真实路径；按 DisplayClass 找 System 行。
	var sy *Candidate
	for i := range cands {
		if cands[i].DisplayClass == ClassSystem {
			sy = &cands[i]
		}
	}
	if sy == nil || sy.Class != ClassConfig || !sy.Checked {
		t.Fatalf("既有 System 行允许保留且默认勾选：%+v", sy)
	}
	sh := candByPath(cands, shared)
	if sh == nil || sh.Class != ClassShared || sh.Checked {
		t.Fatalf("shared 行：%+v", sh)
	}
	kr := candByPath(cands, keyring)
	if kr == nil || kr.Class != ClassKeyring || kr.Checked {
		t.Fatalf("keyring 行：%+v", kr)
	}
	if !containsString(warns, "cache_path_dropped") {
		t.Fatalf("cache 路径应记 cache_path_dropped：%v", warns)
	}
}

// 同一路径同时出现在 paths/shared/unsupported：只留一行 Config，并带 Warning。
func TestCandidatesFromProfileDedupes(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "foo")
	p := Profile{
		Paths:        []string{r.ToStoreForm(cfg)},
		SharedWrites: []string{r.ToStoreForm(cfg)},
	}
	cands, warns := r.CandidatesFromProfileWithWarns(p)
	if len(cands) != 1 || cands[0].Class != ClassConfig || !cands[0].Warning {
		t.Fatalf("应按规范化路径去重并保留 Config：%+v", cands)
	}
	if !containsString(warns, "profile_conflict") {
		t.Fatalf("多类别冲突应记 profile_conflict：%v", warns)
	}
}

// 既有 cache 凭据：不进入新 credential_paths，并记 cache_credential。
func TestCandidatesFromProfileCacheCredential(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "foo")
	cp := filepath.Join(r.XDG["CACHE_HOME"], "foo", "session")
	p := Profile{
		Paths:           []string{r.ToStoreForm(cfg)},
		CredentialPaths: []string{r.ToStoreForm(cp)},
	}
	cands, warns := r.CandidatesFromProfileWithWarns(p)
	c := candByPath(cands, cfg)
	if c == nil || c.Credential != CredentialNone || c.ContainsCredential {
		t.Fatalf("cache 凭据不能展示为 strong：%+v", c)
	}
	if !containsString(warns, "cache_credential") {
		t.Fatalf("应记 cache_credential：%v", warns)
	}
}

// fork：lower 等于/包含/位于凭据路径之下都不挂；路径含空格/中文也要判中。
func TestCredentialBlocksFork(t *testing.T) {
	cred := "/home/u/My Data/凭据"
	cases := []struct {
		lower string
		block bool
	}{
		{cred, true},
		{"/home/u/My Data", true},        // 包含凭据
		{"/home/u/My Data/凭据/sub", true}, // 位于凭据之下
		{"/home/u/My Data/other", false},
		{"/home/u/My Data/凭据备份", false}, // 前缀不是路径关系
	}
	for _, tc := range cases {
		got, msg := credentialBlocksFork(tc.lower, []string{cred})
		if got != tc.block {
			t.Fatalf("%q: got %v want %v (%s)", tc.lower, got, tc.block, msg)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// openat2 flags 不可见时按读计入，但不得参与凭据升级（§6.1）：
// 旧 strace 上"打开过 credentials.json"不能证明它就是凭据文件。
func TestMergeUpperWithReadsFlagsHiddenNoCredential(t *testing.T) {
	e, r := testHomeXDG(t)
	rel := ".foo/credentials.json"
	cands, _, _, _ := r.MergeUpperWithReads(nil, []string{rel}, []string{rel}, e.Base, "foo")
	c := candByPath(cands, filepath.Join(r.Home, ".foo"))
	if c == nil {
		t.Fatalf("应生成 ~/.foo 候选：%+v", cands)
	}
	if c.Checked || c.ContainsCredential || c.Credential != CredentialNone {
		t.Fatalf("flags 不可见的读不能升级凭据：%+v", c)
	}
}

// 只读非凭据行全局上限 20 条；超出的计入 hidden，其余行（cache/public 等）
// 不占配额。
func TestMergeUpperWithReadsPlainLimit(t *testing.T) {
	e, r := testHomeXDG(t)
	var reads []string
	for i := 0; i < 25; i++ {
		reads = append(reads, fmt.Sprintf(".app%02d/data.txt", i))
	}
	cands, _, hidden, _ := r.MergeUpperWithReads(nil, reads, nil, e.Base, "demo")
	plain := 0
	for _, c := range cands {
		if c.Class == ClassDotdir && !c.Checked {
			plain++
		}
	}
	if plain != 20 {
		t.Fatalf("只读非凭据行应截到 20 条，得到 %d", plain)
	}
	if hidden != 5 {
		t.Fatalf("溢出应计入 hidden=5，得到 %d", hidden)
	}
}

// 同一路径同时出现在 shared_writes 与 keyring unsupported 时只留 keyring 行：
// 凭证不能留在 verify 的"预期共享"白名单里。
func TestCandidatesFromProfileKeyringBeatsShared(t *testing.T) {
	_, r := testHomeXDG(t)
	kr := filepath.Join(r.XDG["DATA_HOME"], "keyrings", "default.keyring")
	p := Profile{
		SharedWrites: []string{r.ToStoreForm(kr)},
		Unsupported:  []Unsupported{{Kind: "keyring", Path: r.ToStoreForm(kr)}},
	}
	cands, _ := r.CandidatesFromProfileWithWarns(p)
	var rows []Candidate
	for _, c := range cands {
		if samePathLoose(c.Path, kr) {
			rows = append(rows, c)
		}
	}
	if len(rows) != 1 || rows[0].Class != ClassKeyring || rows[0].Checked {
		t.Fatalf("同路径 shared+keyring 应只留一行 keyring：%+v", rows)
	}
}

// cache 段下的强凭据名文件不是登录态证据：owner 仍是 config，但文件本身
// 可再生，不能升级凭据、也不能进 credential_paths（否则 fork 拒绝整个
// App 目录，分身从空配置起步）。
func TestMergeUpperWithReadsCacheSubdirCredential(t *testing.T) {
	e, r := testHomeXDG(t)
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "App")
	cands, _, _, warns := r.MergeUpperWithReads(nil, []string{".config/App/cache/token.json"}, nil, e.Base, "app")
	c := candByPath(cands, cfg)
	if c == nil {
		t.Fatalf("owner 应生成候选：%+v", cands)
	}
	if c.ContainsCredential || c.Credential != CredentialNone {
		t.Fatalf("cache 子目录里的 token 不能升级凭据：%+v", c)
	}
	if !containsString(warns, "cache_credential") {
		t.Fatalf("应记 cache_credential：%v", warns)
	}
	derived := r.credentialPaths([]Candidate{{
		Path: cfg, Class: ClassConfig, Checked: true,
		Credential: CredentialStrong, CredentialPath: filepath.Join(cfg, "cache", "token.json"),
	}}, []string{r.ToStoreForm(cfg)})
	if len(derived) != 0 {
		t.Fatalf("cache 凭据不能进 credential_paths：%v", derived)
	}
}

// 旧档案里的 keyring 凭据不能展示成 strong，也不能派生进 credential_paths。
func TestCandidatesFromProfileRejectsKeyringCredential(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "foo")
	kr := filepath.Join(cfg, "default.keyring")
	p := Profile{
		Paths:           []string{r.ToStoreForm(cfg)},
		CredentialPaths: []string{r.ToStoreForm(kr)},
	}
	cands, _ := r.CandidatesFromProfileWithWarns(p)
	c := candByPath(cands, cfg)
	if c == nil || c.Credential != CredentialNone || c.ContainsCredential {
		t.Fatalf("keyring 不能当凭据展示：%+v", c)
	}
	if got := r.credentialPaths(cands, p.Paths); len(got) != 0 {
		t.Fatalf("keyring 不能进 credential_paths：%v", got)
	}
}

// HOME 是软链时 ToStoreForm 仍要还原成 ~/...，不能把档案改写成宿主真实路径。
func TestToStoreFormSymlinkHome(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "home-real")
	if err := os.MkdirAll(realHome, 0o700); err != nil {
		t.Fatal(err)
	}
	linkHome := filepath.Join(root, "home")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Fatal(err)
	}
	r := newResolver(linkHome, linkHome, 1000, func(string) string { return "" })
	abs := filepath.Join(realHome, ".config", "App")
	if got := r.ToStoreForm(abs); got != "~/.config/App" {
		t.Fatalf("软链 HOME 应还原为 ~/...，得到 %q", got)
	}
}
