package core

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// ProfileVersion 是当前档案 schema 版本（§5.5）。读到更大的版本必须拒绝，
// 猜新字段语义 = 用旧代码解释新数据，最坏情况是把该隔离的路径漏掉。
const ProfileVersion = 1

// Profile 是 per-app 路径知识库。字段语义见 docs/方案设计.md §5。
type Profile struct {
	// App 不落盘：档案文件名就是应用 key 的权威来源。它只用来让
	// LoadProfile/SaveProfile 的方法签名保持一元（否则每个调用点都要
	// 自己把 app 传两遍，传错一次就写错文件）。
	App            string `toml:"-"`
	Version        int
	Exec           string
	ExecArgs       []string
	Paths          []string
	SharedWrites   []string
	LockFiles      []string
	Sockets        []string
	ExtraBinds     []BindPair
	IsolateDBus    bool
	IsolateRuntime bool
	Env            map[string]string
	Cwd            string
	ProbeMethod    string // overlay | strace | manual
	Audit          bool
	Unsupported    []Unsupported
	Ignored        []string

	// Unknown 保存本版本不认识的键，重写档案时原样回填（§5.5）。
	// 否则用旧版改一次显示名就把新版字段抹掉了。
	Unknown map[string]toml.Primitive

	// Warnings 是本次加载/校验产生的告警（父子路径归一之类），不落盘。
	Warnings []string
}

// Unsupported 是"根本不在文件路径上、这个方案隔离不了"的项（keyring 等）。
// 它不进 paths、也不进 shared_writes——后者是 verify 的"预期共享"白名单，
// 混进去会让对账口径失真（§5.4）。
type Unsupported struct {
	Kind   string
	Path   string
	Reason string
}

// Instance 是 instances/<app>/<id>/instance.toml 的内容（§4）。
// id 与 display_name 是两件事：id 只出现在路径里，display_name 才是用户看到的。
type Instance struct {
	Version     int
	App         string
	ID          string
	DisplayName string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	ProbeMethod string
	Root        string // instances/<app>/<id>/root 的绝对路径
}

// Dir 返回实例目录的绝对路径。
func (in Instance) Dir() string { return filepath.Dir(in.Root) }

// profileWire 是落盘形状。它与 Profile 分开的理由：ExtraBinds 在 TOML 里是
// "源|目标" 字符串，而内存里是结构体；混用会让 Load/Save 各自做一次转换。
type profileWire struct {
	Version        int               `toml:"version"`
	Exec           string            `toml:"exec"`
	ExecArgs       []string          `toml:"exec_args,omitempty"`
	Paths          []string          `toml:"paths"`
	SharedWrites   []string          `toml:"shared_writes,omitempty"`
	LockFiles      []string          `toml:"lock_files,omitempty"`
	Sockets        []string          `toml:"sockets,omitempty"`
	ExtraBinds     []string          `toml:"extra_binds,omitempty"`
	IsolateDBus    bool              `toml:"isolate_dbus,omitempty"`
	IsolateRuntime bool              `toml:"isolate_runtime,omitempty"`
	Env            map[string]string `toml:"env,omitempty"`
	Cwd            string            `toml:"cwd,omitempty"`
	ProbeMethod    string            `toml:"probe_method,omitempty"`
	Audit          bool              `toml:"audit,omitempty"`
	Unsupported    []unsupportedWire `toml:"unsupported,omitempty"`
	// Ignored 用指针区分"没写"与"写了空表"：`ignored = []` 是用户要求
	// "全部隔离、不要默认忽略表"的显式声明，nil 才是"没声明"（用默认表）。
	// 用 []string + omitempty 会把空表当零值丢掉，重新加载后又变回 nil，
	// 等于把用户的显式配置静默换成默认值（§5.5）。
	Ignored *[]string `toml:"ignored,omitempty"`
}

type unsupportedWire struct {
	Kind   string `toml:"kind"`
	Path   string `toml:"path,omitempty"`
	Reason string `toml:"reason,omitempty"`
}

type instanceWire struct {
	Version     int    `toml:"version"`
	App         string `toml:"app"`
	ID          string `toml:"id"`
	DisplayName string `toml:"display_name"`
	CreatedAt   string `toml:"created_at"`
	LastUsedAt  string `toml:"last_used_at,omitempty"`
	ProbeMethod string `toml:"probe_method,omitempty"`
}

// knownProfileKeys 是 v1 认识的全部顶层键。新增字段时必须同步这里，
// 否则新字段会被当成"未知字段"原样保留，而结构体里永远读不到它——
// 表现出来就是"档案里写了、程序不认"，且没有任何报错。
var knownProfileKeys = map[string]bool{
	"version": true, "exec": true, "exec_args": true, "paths": true,
	"shared_writes": true, "lock_files": true, "sockets": true, "extra_binds": true,
	"isolate_dbus": true, "isolate_runtime": true, "env": true, "cwd": true,
	"probe_method": true, "audit": true, "unsupported": true, "ignored": true,
}

// decodeKnown 把 Primitive 表按已知字段解码进 wire 结构。
func decodeKnown(md toml.MetaData, prims map[string]toml.Primitive, w *profileWire) error {
	get := func(key string, dst any) error {
		prim, ok := prims[key]
		if !ok {
			return nil
		}
		return md.PrimitiveDecode(prim, dst)
	}
	for key, dst := range map[string]any{
		"version": &w.Version, "exec": &w.Exec, "exec_args": &w.ExecArgs,
		"paths": &w.Paths, "shared_writes": &w.SharedWrites, "lock_files": &w.LockFiles,
		"sockets": &w.Sockets, "extra_binds": &w.ExtraBinds,
		"isolate_dbus": &w.IsolateDBus, "isolate_runtime": &w.IsolateRuntime,
		"env": &w.Env, "cwd": &w.Cwd, "probe_method": &w.ProbeMethod,
		"audit": &w.Audit, "unsupported": &w.Unsupported, "ignored": &w.Ignored,
	} {
		if err := get(key, dst); err != nil {
			return err
		}
	}
	return nil
}

// unknownFields 返回本版本不认识的键，重写档案时原样回填（§5.5）。
func unknownFields(prims map[string]toml.Primitive) map[string]toml.Primitive {
	out := map[string]toml.Primitive{}
	for k, v := range prims {
		if !knownProfileKeys[k] {
			out[k] = v
		}
	}
	return out
}

// ProfilePath 返回运行时档案的路径。注意与仓库内 profiles/ 模板库区分（§4）。
func (e *Env) ProfilePath(app string) string {
	return filepath.Join(e.Base, "profiles.d", app+".toml")
}

// InstancePath 返回某个实例目录。
func (e *Env) InstancePath(app, id string) string {
	return filepath.Join(e.Base, "instances", app, id)
}

// LoadProfile 读运行时档案并按 §5.5 判版本。档案不存在返回 CodeInput 错误，
// 调用方据此走"首次配置"；这与"档案损坏"必须是两类，故错误键分开。
func (e *Env) LoadProfile(app string) (Profile, error) {
	p := Profile{Version: ProfileVersion, App: app}
	path := e.ProfilePath(app)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, errInput("err.profile.not_found", map[string]any{"app": app})
		}
		return p, errInternal("err.internal.read_profile", map[string]any{"path": path}, err)
	}

	return parseProfile(app, path, data)
}

// parseProfile 把档案字节解析成 Profile。模板（内置 profiles/）与运行时档案
// 走同一段解析：两套解析器迟早会在"未知字段/版本策略"上分叉。
func parseProfile(app, path string, data []byte) (Profile, error) {
	p := Profile{Version: ProfileVersion, App: app}
	// 一次解码到 Primitive 表：既能按已知字段取值，也能把不认识的键原样留存。
	// 分两次解码（一次 struct、一次 map）会在两次解析之间留下不一致的窗口，
	// 而"未知字段保留"的要求恰恰要求两者来自同一份字节。
	var prims map[string]toml.Primitive
	md, err := toml.Decode(string(data), &prims)
	if err != nil {
		return p, newErr(CodeInput, "err.profile.parse", map[string]any{"path": path}, err)
	}
	w := profileWire{Version: ProfileVersion}
	if err := decodeKnown(md, prims, &w); err != nil {
		return p, newErr(CodeInput, "err.profile.parse", map[string]any{"path": path}, err)
	}
	if w.Version > ProfileVersion {
		return p, errInput("err.profile.version_ahead", map[string]any{
			"app": app, "version": w.Version, "current": ProfileVersion,
		})
	}
	if w.Version < ProfileVersion {
		// v1 是首版，没有迁移器；未来版本在这里串迁移函数链，不许"缺字段填默认值"糊过去。
		w.Version = ProfileVersion
	}

	p = Profile{
		Version:        w.Version,
		Exec:           w.Exec,
		ExecArgs:       w.ExecArgs,
		Paths:          w.Paths,
		SharedWrites:   w.SharedWrites,
		LockFiles:      w.LockFiles,
		Sockets:        w.Sockets,
		IsolateDBus:    w.IsolateDBus,
		IsolateRuntime: w.IsolateRuntime,
		Env:            w.Env,
		Cwd:            w.Cwd,
		ProbeMethod:    w.ProbeMethod,
		Audit:          w.Audit,
		Unknown:        unknownFields(prims),
	}
	// `ignored = []` 与"没写 ignored"语义不同：前者是用户显式声明空表，
	// 后者才走默认忽略表。指针保留这个区别，不能折叠成同一个 nil。
	if w.Ignored != nil {
		p.Ignored = *w.Ignored
	}
	for _, u := range w.Unsupported {
		p.Unsupported = append(p.Unsupported, Unsupported{Kind: u.Kind, Path: u.Path, Reason: u.Reason})
	}
	for _, s := range w.ExtraBinds {
		src, dst, ok := strings.Cut(s, "|")
		if !ok {
			return p, errInput("err.profile.extra_bind_format", map[string]any{"value": s})
		}
		p.ExtraBinds = append(p.ExtraBinds, BindPair{Src: strings.TrimSpace(src), Dst: strings.TrimSpace(dst)})
	}
	if p.Env == nil {
		p.Env = map[string]string{}
	}
	p.App = app
	return p, nil
}

// SaveProfile 原子写档案，并把 Unknown 里的键原样写回。
// 顺序：同目录 tmp → Sync → Rename（§4.2）。中途崩溃只留一个 tmp 文件，
// 不会出现"半份档案"让下次解析失败。
func (e *Env) SaveProfile(p Profile) error {
	if p.Version == 0 {
		p.Version = ProfileVersion
	}
	w := profileWire{
		Version:        p.Version,
		Exec:           p.Exec,
		ExecArgs:       p.ExecArgs,
		Paths:          p.Paths,
		SharedWrites:   p.SharedWrites,
		LockFiles:      p.LockFiles,
		Sockets:        p.Sockets,
		IsolateDBus:    p.IsolateDBus,
		IsolateRuntime: p.IsolateRuntime,
		Env:            p.Env,
		Cwd:            p.Cwd,
		ProbeMethod:    p.ProbeMethod,
		Audit:          p.Audit,
		Ignored:        ignoredWire(p.Ignored),
	}
	for _, b := range p.ExtraBinds {
		w.ExtraBinds = append(w.ExtraBinds, b.Src+"|"+b.Dst)
	}
	for _, u := range p.Unsupported {
		w.Unsupported = append(w.Unsupported, unsupportedWire{Kind: u.Kind, Path: u.Path, Reason: u.Reason})
	}

	data, err := encodeProfileOf(w, p.Unknown)
	if err != nil {
		return err
	}
	dir := filepath.Dir(e.ProfilePath(p.App))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errEnv("err.env.mkdir", map[string]any{"path": dir}, err)
	}
	return writeFileAtomic(e.ProfilePath(p.App), data, 0o600)
}

// ignoredWire 把内存里的 ignored 带进落盘形状，保留 nil 与空表的区别。
// nil（没声明）不写这个键；非 nil（含 `[]`）都写出来，空表写成 `ignored = []`。
func ignoredWire(v []string) *[]string {
	if v == nil {
		return nil
	}
	return &v
}

// encodeProfile 是 CreateInstance 事务的前半段：它要拿到字节写进临时文件，
// 由提交点的 rename 决定是否真的落盘，所以不能直接调 SaveProfile。
func encodeProfile(p Profile) ([]byte, error) {
	w := profileWire{
		Version:        p.Version,
		Exec:           p.Exec,
		ExecArgs:       p.ExecArgs,
		Paths:          p.Paths,
		SharedWrites:   p.SharedWrites,
		LockFiles:      p.LockFiles,
		Sockets:        p.Sockets,
		IsolateDBus:    p.IsolateDBus,
		IsolateRuntime: p.IsolateRuntime,
		Env:            p.Env,
		Cwd:            p.Cwd,
		ProbeMethod:    p.ProbeMethod,
		Audit:          p.Audit,
		Ignored:        ignoredWire(p.Ignored),
	}
	for _, b := range p.ExtraBinds {
		w.ExtraBinds = append(w.ExtraBinds, b.Src+"|"+b.Dst)
	}
	for _, u := range p.Unsupported {
		w.Unsupported = append(w.Unsupported, unsupportedWire{Kind: u.Kind, Path: u.Path, Reason: u.Reason})
	}
	return encodeProfileOf(w, p.Unknown)
}

func encodeProfileOf(w profileWire, unknown map[string]toml.Primitive) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(w); err != nil {
		return nil, errInternal("err.internal.encode_profile", nil, err)
	}
	// 未知字段按 key 字典序回填，保证同一份档案多次保存字节稳定（便于 diff）。
	keys := make([]string, 0, len(unknown))
	for k := range unknown {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var frag bytes.Buffer
		one := map[string]toml.Primitive{k: unknown[k]}
		if err := toml.NewEncoder(&frag).Encode(one); err != nil {
			return nil, errInternal("err.internal.encode_profile", nil, err)
		}
		buf.Write(frag.Bytes())
	}
	return buf.Bytes(), nil
}

// loadInstance 读 instance.toml。instance.toml 是实例元数据的唯一权威来源，
// 目录名只是它的冗余索引——不读目录推断，否则重命名/搬家立刻失效。
func (e *Env) loadInstance(app, id string) (Instance, error) {
	dir := e.InstancePath(app, id)
	path := filepath.Join(dir, "instance.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Instance{}, errConflict("err.instance.not_found", map[string]any{"app": app, "id": id})
		}
		return Instance{}, errInternal("err.internal.read_instance", map[string]any{"path": path}, err)
	}
	var w instanceWire
	if _, err := toml.Decode(string(data), &w); err != nil {
		return Instance{}, newErr(CodeInput, "err.instance.parse", map[string]any{"path": path}, err)
	}
	in := Instance{
		Version:     w.Version,
		App:         w.App,
		ID:          w.ID,
		DisplayName: w.DisplayName,
		ProbeMethod: w.ProbeMethod,
		Root:        filepath.Join(dir, "root"),
	}
	if in.App == "" {
		in.App = app
	}
	if in.ID == "" {
		in.ID = id
	}
	if in.CreatedAt, err = parseTime(w.CreatedAt); err != nil {
		return Instance{}, newErr(CodeInput, "err.instance.bad_time", map[string]any{"path": path}, err)
	}
	if w.LastUsedAt != "" {
		t, err := parseTime(w.LastUsedAt)
		if err != nil {
			return Instance{}, newErr(CodeInput, "err.instance.bad_time", map[string]any{"path": path}, err)
		}
		in.LastUsedAt = &t
	}
	return in, nil
}

func (e *Env) saveInstance(in Instance) error {
	w := instanceWire{
		Version:     ProfileVersion,
		App:         in.App,
		ID:          in.ID,
		DisplayName: in.DisplayName,
		CreatedAt:   formatTime(in.CreatedAt),
		ProbeMethod: in.ProbeMethod,
	}
	if in.LastUsedAt != nil {
		w.LastUsedAt = formatTime(*in.LastUsedAt)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(w); err != nil {
		return errInternal("err.internal.encode_instance", nil, err)
	}
	return writeFileAtomic(filepath.Join(in.Dir(), "instance.toml"), buf.Bytes(), 0o600)
}

// 时间一律 RFC3339 UTC（§4）：本地时间会让跨时区/夏令时的时间戳无法比较。
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, strings.TrimSpace(s))
}

// DefaultIgnored 是审计对账的预期噪声（§5 的 ignored 字段）。不给默认值的话，
// 任何真实应用都是"泄漏"——正常启动就写 ~/.cache。
func DefaultIgnored() []string {
	return []string{"~/.cache", "~/.thumbnails", "~/.local/share/Trash", "~/.local/state"}
}

func (p Profile) String() string {
	return fmt.Sprintf("profile(exec=%s paths=%d)", p.Exec, len(p.Paths))
}
