package core

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// App 是列表页的一行（§11）。GUI 列表所需的全部信息都在这里：
// 它不再自己挑 Name[lang]、也不再自己算 creatable。
type App struct {
	DesktopID      string
	Key            string // 目录名 <app>（归一化结果），与档案/实例目录同名
	Name           string // 已按当前 locale 取好 Name[lang]
	Icon           string
	Exec           string
	ExecArgs       []string
	FieldCodes     string // 源 Exec 的 %U/%F，生成入口时要原样保留（§4.1）
	Categories     string
	StartupWMClass string
	HasProfile     bool
	Creatable      bool
	InstanceN      int
	RunningN       int
	LastUsed       *time.Time
	ProbeMethod    string
	Sources        []string // 命中的 .desktop 路径（多来源去重后，用于排障）
}

// ScanError 是单个 .desktop 的解析失败。它不阻断整体扫描：一个坏文件
// 不该让整页应用消失（§4 的 <app> 规则里专门要求"记一条 ScanError，不进列表"）。
type ScanError struct {
	Path string
	Key  string
	Err  error
}

// AppDirs 返回需要扫描的三个应用来源目录（§6）。
// 只扫用户级会漏掉 97% 的应用——/usr/share/applications 是主体。
func (e *Env) AppDirs() []string {
	if len(e.AppDirList) > 0 {
		return e.AppDirList
	}
	return []string{
		filepath.Join(e.Resolver.XDG["DATA_HOME"], "applications"),
		"/usr/share/applications",
		"/var/lib/flatpak/exports/share/applications",
	}
}

// ScanApps 扫描并解析 .desktop。返回成功项与逐条失败，二者都要交给调用方：
// 静默丢掉坏文件会让"应用列表里少一个应用"变成无法诊断的问题。
func (e *Env) ScanApps(ctx context.Context) ([]App, []ScanError) {
	var apps []App
	var errs []ScanError
	seen := map[string]int{} // key → apps 下标，用于撞车时加后缀

	for _, dir := range e.AppDirs() {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue // 目录不存在是常态（没有 flatpak），不是错误
		}
		for _, ent := range ents {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".desktop") {
				continue
			}
			select {
			case <-ctx.Done():
				return apps, errs
			default:
			}
			path := filepath.Join(dir, ent.Name())
			// 自己生成的分身入口按前缀排除，否则会把分身当应用再分身（§4.1）。
			if strings.HasPrefix(ent.Name(), "appclone-") {
				continue
			}
			a, err := parseDesktopFile(path)
			if err != nil {
				errs = append(errs, ScanError{Path: path, Key: ent.Name(), Err: err})
				continue
			}
			if a == nil {
				continue // 不是 Application、NoDisplay、Hidden、不属于当前桌面
			}
			if err := ValidateAppKey(a.Key); err != nil {
				errs = append(errs, ScanError{Path: path, Key: a.Key, Err: err})
				continue
			}
			if idx, dup := seen[a.Key]; dup {
				// 归一化撞车（Foo.desktop 与 foo.desktop）：保留高优先级来源
				// （用户级先扫），另一个加稳定短哈希后缀，保证同机每次扫描结果一致（§4）。
				sum := sha256.Sum256([]byte(path))
				suffixed := a.Key + "-" + hex.EncodeToString(sum[:])[:6]
				if err := ValidateAppKey(suffixed); err != nil {
					errs = append(errs, ScanError{Path: path, Key: suffixed, Err: err})
					continue
				}
				a.Key = suffixed
				apps[idx].Sources = append(apps[idx].Sources, path)
			}
			seen[a.Key] = len(apps)
			apps = append(apps, *a)
		}
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Key < apps[j].Key })
	return apps, errs
}

// parseDesktopFile 解析单个 .desktop。返回 (nil, nil) 表示"合法但不应显示"。
func parseDesktopFile(path string) (*App, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errEnv("err.env.read_dir", map[string]any{"path": path}, err)
	}
	defer f.Close()

	keys := map[string]string{}
	inEntry := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			inEntry = trimmed == "[Desktop Entry]"
			continue
		}
		if !inEntry {
			continue
		}
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		keys[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return nil, errEnv("err.env.read_dir", map[string]any{"path": path}, err)
	}

	if keys["Type"] != "Application" {
		return nil, nil
	}
	if isTrue(keys["NoDisplay"]) || isTrue(keys["Hidden"]) {
		return nil, nil
	}
	if only := keys["OnlyShowIn"]; only != "" && !inCurrentDesktop(only) {
		return nil, nil
	}

	execRaw := keys["Exec"]
	if strings.TrimSpace(execRaw) == "" {
		return nil, errInput("err.app.no_exec", map[string]any{"path": path})
	}
	exec, execArgs, codes, err := parseExecLine(execRaw)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(exec) {
		// 裸命令用 LookPath 解析成绝对路径；仍非绝对则档案无效（§5 exec 字段）。
		if p, lerr := osexec.LookPath(exec); lerr == nil {
			exec = p
		} else {
			return nil, errInput("err.app.exec_unresolved", map[string]any{"exec": execRaw, "path": path})
		}
	}

	base := filepath.Base(path)
	// 真实机器上存在 "CC Switch.desktop" 这类带空格的文件名：直接按字符集校验
	// 会把它整个丢掉（用户看到"应用列表里没有它"却查不出原因）。所以先归一化成
	// 安全 key，归一化后仍不合规才拒绝（§4 <app> 规则）。
	key, kerr := SanitizeAppKey(strings.TrimSuffix(base, ".desktop"))
	if kerr != nil {
		return nil, kerr
	}
	a := &App{
		DesktopID:      base,
		Key:            key,
		Name:           localizedName(keys),
		Icon:           keys["Icon"],
		Exec:           exec,
		ExecArgs:       execArgs,
		FieldCodes:     codes,
		Categories:     keys["Categories"],
		StartupWMClass: keys["StartupWMClass"],
		Sources:        []string{path},
	}
	return a, nil
}

func isTrue(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "true" || v == "1"
}

func inCurrentDesktop(list string) bool {
	cur := strings.ToUpper(strings.TrimSpace(os.Getenv("XDG_CURRENT_DESKTOP")))
	if cur == "" {
		return true // 判不出来就保留：宁可多显示，也不要让应用凭空消失
	}
	for _, item := range strings.Split(cur, ":") {
		for _, want := range strings.Split(list, ";") {
			if strings.EqualFold(strings.TrimSpace(want), item) {
				return true
			}
		}
	}
	return false
}

// localizedName 取当前 locale 的 Name[lang]；zh 用 zh_CN，其余用默认 Name。
// GUI 不再自己挑——挑拣规则写两遍必然不一致。
func localizedName(keys map[string]string) string {
	if isZhLocale() {
		if v := keys["Name[zh_CN]"]; v != "" {
			return v
		}
	}
	if v := keys["Name"]; v != "" {
		return v
	}
	return keys["Name[zh]"]
}

// parseExecLine 把 Exec= 拆成（可执行文件, 参数, 字段码）。
//
// 两处语义相反，别混：解析成档案时要**剥掉**字段码（§5 exec 字段），
// 生成分身入口时要**保留**（§4.1）。所以这里把字段码单独返回。
func parseExecLine(raw string) (string, []string, string, error) {
	toks, err := splitExec(raw)
	if err != nil {
		return "", nil, "", err
	}
	// 去掉 env VAR=x 前缀（Flatpak/部分发行版的 desktop 会这么写）。
	if len(toks) > 0 && filepath.Base(toks[0]) == "env" {
		i := 1
		for i < len(toks) && strings.Contains(toks[i], "=") && !strings.HasPrefix(toks[i], "-") {
			i++
		}
		toks = toks[i:]
	}
	if len(toks) == 0 {
		return "", nil, "", errInput("err.app.no_exec", nil)
	}
	exec := toks[0]
	args := make([]string, 0, len(toks)-1)
	var codes []string
	for _, t := range toks[1:] {
		if strings.HasPrefix(t, "%") && len(t) == 2 {
			// %i 是 --icon 占位，没有值就整项丢掉；其余字段码单独收集。
			if t != "%i" {
				codes = append(codes, t)
			}
			continue
		}
		args = append(args, t)
	}
	return exec, args, strings.Join(codes, " "), nil
}

// splitExec 按 Desktop Entry 的 Exec 语法拆分：双引号成组、反斜杠转义。
func splitExec(raw string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote := false
	has := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '\\' && i+1 < len(raw):
			i++
			cur.WriteByte(raw[i])
			has = true
		case c == '"':
			inQuote = !inQuote
			has = true
		case (c == ' ' || c == '\t') && !inQuote:
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	if inQuote {
		return nil, errInput("err.app.exec_unbalanced_quote", map[string]any{"exec": raw})
	}
	if has {
		out = append(out, cur.String())
	}
	return out, nil
}

// LoadApp 按 key 找一个应用。找不到时返回 CodeInput：
// "应用被卸载但分身数据还在"必须能被区分出来（孤儿实例仍要可删）。
func (e *Env) LoadApp(key string) (App, error) {
	apps, _ := e.ScanApps(context.Background())
	for _, a := range apps {
		if a.Key == key {
			return a, nil
		}
	}
	return App{}, errInput("err.app.not_found", map[string]any{"app": key})
}

// ListApps 是列表页的数据源：三目录扫描 ∪ 有档案的 app ∪ 有实例的 app。
// 后两者不能省——应用卸载后孤儿实例必须还能被看见和删除（§6）。
func (e *Env) ListApps(ctx context.Context) ([]App, []ScanError) {
	apps, errs := e.ScanApps(ctx)
	byKey := make(map[string]int, len(apps))
	for i, a := range apps {
		byKey[a.Key] = i
	}
	addOrGet := func(key string) int {
		if i, ok := byKey[key]; ok {
			return i
		}
		apps = append(apps, App{Key: key, Name: key})
		byKey[key] = len(apps) - 1
		return len(apps) - 1
	}

	if ents, err := os.ReadDir(filepath.Join(e.Base, "profiles.d")); err == nil {
		for _, ent := range ents {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".toml") {
				continue
			}
			addOrGet(strings.TrimSuffix(ent.Name(), ".toml"))
		}
	}
	if ents, err := os.ReadDir(filepath.Join(e.Base, "instances")); err == nil {
		for _, ent := range ents {
			if ent.IsDir() {
				addOrGet(ent.Name())
			}
		}
	}

	for i := range apps {
		a := &apps[i]
		hasProfile, creatable, err := e.ProfileState(a.Key)
		if err == nil {
			a.HasProfile, a.Creatable = hasProfile, creatable
		}
		if hasProfile {
			if p, err := e.LoadProfile(a.Key); err == nil {
				a.ProbeMethod = p.ProbeMethod
				if a.Exec == "" {
					a.Exec = p.Exec
				}
			}
		}
		insts, _ := e.ListInstances(a.Key)
		a.InstanceN = len(insts)
		// 只对已有实例的应用做 run.lock 试探（全是 0 的应用不做 IO）：
		// 397 个应用 × 一次 flock 会把首屏拖慢到可感知（§11 App.RunningN）。
		if a.InstanceN > 0 {
			for _, in := range insts {
				if running, err := e.IsRunning(a.Key, in.ID); err == nil && running {
					a.RunningN++
				}
				if in.LastUsedAt != nil && (a.LastUsed == nil || in.LastUsedAt.After(*a.LastUsed)) {
					t := *in.LastUsedAt
					a.LastUsed = &t
				}
			}
		}
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Key < apps[j].Key })
	return apps, errs
}
