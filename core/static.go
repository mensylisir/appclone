package core

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// StaticHints 是 L0 的产出（docs/discover.md §4）：**零执行**的预填。
//
// 它不是结论，是"冷启动前就能猜到的东西"：L1 的 overlay 才是权威。
// 它唯一被探测直接消费的字段是 LockFiles——把锁文件 bind 成空文件，主实例
// 在跑时应用才不会把参数转发过去然后自己退出（§3.2.2）。
type StaticHints struct {
	Paths     []string `json:"paths"`      // 低置信候选（目录，且必须已存在）
	LockFiles []string `json:"lock_files"` // 已存在的锁文件（绝对路径）
	Sources   []string `json:"sources"`    // 命中了哪些来源，排障用
}

// StaticHints 收集 L0 的三类线索：已知模式、AppArmor 的 owner-write 规则、Flatpak persist。
//
// **召回预期要诚实**（§4 末）：Arch 上大概率三项都拿不到，只剩"`~/.config/<id>` 模式匹配"。
// 所以它只做预填，任何一项失败都静默跳过，绝不让探测因此失败。
func (e *Env) StaticHints(ctx context.Context, d App) StaticHints {
	var h StaticHints
	addPath := func(p string) {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			h.Paths = append(h.Paths, p)
		}
	}
	// ① 已知模式：XDG 下的 `<key>`/`<desktop-id>`，以及家目录顶层的 `.<key>`。
	// 用 key 与 desktop id 两个名字都试：`org.telegram.desktop` 与归一化后的
	// `org.telegram.desktop` 常常一致，但 `CC Switch` 这类归一化后会变。
	candidates := []string{d.Key}
	if id := strings.TrimSuffix(d.DesktopID, ".desktop"); id != "" && id != d.Key {
		candidates = append(candidates, id)
	}
	for _, name := range candidates {
		addPath(filepath.Join(e.Resolver.XDG["CONFIG_HOME"], name))
		addPath(filepath.Join(e.Resolver.XDG["DATA_HOME"], name))
		addPath(filepath.Join(e.Resolver.Home, "."+name))
		addPath(filepath.Join(e.Resolver.Home, ".var", "app", name))
	}
	if len(h.Paths) > 0 {
		h.Sources = append(h.Sources, "pattern")
	}

	// ② AppArmor：发行版人工策展的写入路径清单，免费的知识库。
	// 只有该发行版启用 AppArmor 才有 /etc/apparmor.d（Arch 通常没有）。
	if paths := e.appArmorPaths(d); len(paths) > 0 {
		for _, p := range paths {
			addPath(p)
		}
		h.Sources = append(h.Sources, "apparmor")
	}

	// ③ Flatpak persist 列表（只对 flatpak 应用有效）。
	if paths := e.flatpakPaths(ctx, d); len(paths) > 0 {
		for _, p := range paths {
			addPath(p)
		}
		h.Sources = append(h.Sources, "flatpak")
	}

	h.Paths = dedupeSorted(h.Paths)
	h.LockFiles = e.lockFilesIn(h.Paths)
	return h
}

// appArmorPaths 从 `/etc/apparmor.d/<profile>` 里抠出 owner-write 规则里家目录下的路径。
//
// profile 名是"可执行文件路径把 / 换掉"（`/usr/bin/firefox` → `usr.bin.firefox`），
// 也常见 `<binary>` 本身。两种都试。
func (e *Env) appArmorPaths(d App) []string {
	if d.Exec == "" {
		return nil
	}
	bin := filepath.Base(d.Exec)
	names := []string{
		strings.ReplaceAll(strings.TrimPrefix(d.Exec, "/"), "/", "."),
		bin,
	}
	for _, name := range names {
		f, err := os.Open(filepath.Join("/etc/apparmor.d", name))
		if err != nil {
			continue
		}
		out := parseAppArmorProfile(f, e.Resolver.Home)
		f.Close()
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// parseAppArmorProfile 抽出 `owner ... w` 规则里家目录下的具体路径。
//
// 只认"自己写自己"的规则：不带 owner 的 w 规则是系统级可写路径（`/etc/**` 之类），
// 那不属于应用数据。带通配（`*`、`?`、`[`）的规则没法落成路径，交给 L1。
func parseAppArmorProfile(r io.Reader, home string) []string {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "owner ") || !strings.ContainsRune(line, 'w') {
			continue
		}
		// AppArmor 的规则是 `路径 权限集,`：最后一个字段才是权限，前面可能有
		// 多个路径。把这段搞混的后果是"一个路径都解析不出来"（我自己先踩过）。
		fields := strings.Fields(strings.TrimPrefix(line, "owner "))
		if len(fields) < 2 {
			continue
		}
		if !strings.ContainsRune(fields[len(fields)-1], 'w') {
			continue
		}
		for _, tok := range fields[:len(fields)-1] {
			p := strings.TrimSuffix(tok, ",")
			p = strings.TrimSuffix(p, "/**")
			p = strings.ReplaceAll(p, "@{HOME}", home)
			if !filepath.IsAbs(p) || strings.ContainsAny(p, "*?[") {
				continue
			}
			if !IsUnder(home, p) {
				continue
			}
			out = append(out, p)
		}
	}
	return dedupeSorted(out)
}

// flatpakPaths 解析 `flatpak info --show-permissions` 的 filesystems 列表。
// 只有 `~` 开头的项能落成具体路径；`home` / `xdg-download` 这类太宽，跳过
// （把整个家目录当候选会让用户勾出一个"隔离一切"的档案）。
func (e *Env) flatpakPaths(ctx context.Context, d App) []string {
	id := strings.TrimSuffix(d.DesktopID, ".desktop")
	if id == "" || !strings.HasPrefix(d.Exec, "/") {
		return nil
	}
	run := e.RunCmd
	if run == nil {
		run = defaultRunCmd
	}
	out, err := run(ctx, "flatpak", "info", "--show-permissions", id)
	if err != nil {
		return nil // 不是 flatpak 应用 / 没装 flatpak：正常情况，静默跳过
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "filesystems" {
			continue
		}
		for _, item := range strings.Split(val, ";") {
			item = strings.TrimSpace(item)
			if !strings.HasPrefix(item, "~") {
				continue
			}
			p := strings.TrimSuffix(strings.Replace(item, "~", e.Resolver.Home, 1), "/")
			if p != "" && IsUnder(e.Resolver.Home, p) {
				paths = append(paths, p)
			}
		}
	}
	return dedupeSorted(paths)
}

// lockFileNames 是"应用用来判定'已经有实例在跑'的文件名"。命中它们就把探测
// 会话里的同名路径 bind 成空文件——否则应用发现锁已被占用，直接把参数转发给
// 主实例然后退出，探测得到零写入（§3.2.2），而用户看到的是"这个应用不写盘"。
var lockFileNames = []string{
	"SingletonLock", "singleton.lock", "lockfile", "lock", ".lock", "LOCK",
}

// lockFilesIn 在候选目录里找已存在的锁文件（每个目录最多一个，避免把缓存锁也算进去）。
func (e *Env) lockFilesIn(dirs []string) []string {
	var out []string
	for _, dir := range dirs {
		for _, name := range lockFileNames {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				out = append(out, p)
				break
			}
		}
	}
	return dedupeSorted(out)
}

// defaultRunCmd 是"跑一个只读的外部命令并拿到输出"。
func defaultRunCmd(ctx context.Context, name string, args ...string) (string, error) {
	b, err := exec.CommandContext(ctx, name, args...).Output()
	return string(b), err
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
