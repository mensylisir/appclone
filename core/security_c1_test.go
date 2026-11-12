package core

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// AC_BASE 的暴露面只在**绑定源**上：src 等于 Base、是 Base 的祖先或位于
// Base 之下，都会让应用经 bind 重新读到实例数据/keyring/私有总线。
func TestValidateProfileBaseExposureSources(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	getenv := func(k string) string {
		switch k {
		case "XDG_CONFIG_HOME":
			return filepath.Join(root, "xdg", "config")
		case "XDG_DATA_HOME":
			return filepath.Join(root, "xdg", "data")
		case "XDG_STATE_HOME":
			return filepath.Join(root, "xdg", "state")
		case "XDG_CACHE_HOME":
			return filepath.Join(root, "xdg", "cache")
		case "XDG_RUNTIME_DIR":
			return filepath.Join(root, "run")
		}
		return ""
	}
	base := filepath.Join(home, ".local", "share", "appclone")
	e := NewEnvAt(base, newResolver(home, home, 1000, getenv))
	e.Trasher = func(paths ...string) error { return nil }
	e.CliPath = "/usr/bin/appclone"
	o := e.opts()

	prof := func() Profile {
		return Profile{App: "x", Version: ProfileVersion, Exec: "/bin/true", Paths: []string{"~/.config/X"}}
	}

	cases := []struct {
		name string
		mut  func(*Profile)
	}{
		{"extra src=HOME", func(p *Profile) {
			p.ExtraBinds = []BindPair{{Src: home, Dst: filepath.Join(root, "mnt")}}
		}},
		{"extra src=Base 祖先", func(p *Profile) {
			p.ExtraBinds = []BindPair{{Src: filepath.Join(home, ".local"), Dst: filepath.Join(root, "mnt")}}
		}},
		{"extra src=Base 本身", func(p *Profile) {
			p.ExtraBinds = []BindPair{{Src: base, Dst: filepath.Join(root, "mnt")}}
		}},
		{"extra src=Base 之下", func(p *Profile) {
			p.ExtraBinds = []BindPair{{Src: filepath.Join(base, "instances"), Dst: filepath.Join(root, "mnt")}}
		}},
		{"shared src=Base 祖先", func(p *Profile) {
			p.SharedWrites = []string{filepath.Join(home, ".local")}
		}},
		{"shared src=Base 之下", func(p *Profile) {
			p.SharedWrites = []string{filepath.Join(base, "keyring")}
		}},
		{"lock dst=Base 之下", func(p *Profile) {
			p.LockFiles = []string{filepath.Join(base, "keyring", "lock")}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := prof()
			c.mut(&p)
			err := ValidateProfile(p, o)
			if ExitCode(err) != CodeInput || !strings.Contains(errKey(err), "err.profile.base_exposure") {
				t.Fatalf("应报 2/base_exposure，得到 %v", err)
			}
		})
	}

	// paths 指向 Base 的**祖先**是合法档案（目标被实例数据替换，Base 仍被
	// tmpfs 遮住）；fork 的 lower/upper 边界用例依赖这条。
	p := prof()
	p.Paths = []string{filepath.Join(home, ".local")}
	if err := ValidateProfile(p, o); err != nil {
		t.Fatalf("paths 的 Base 祖先不该被拒：%v", err)
	}
}

// HOME/XDG 根落在 Base 里时，`--tmpfs Base` 会把它们自己盖掉。
func TestCheckBaseContainers(t *testing.T) {
	o := BuildOptions{Base: "/x/base", BaseReal: "/x/base", Home: "/x/base/home"}
	if err := checkBaseContainers(o); ExitCode(err) != CodeInput {
		t.Fatalf("HOME 在 Base 下必须被拒，得到 %v", err)
	}
	o = BuildOptions{Base: "/x/base", BaseReal: "/x/base", Home: "/x"}
	if err := checkBaseContainers(o); err != nil {
		t.Fatalf("HOME 是 Base 的祖先属正常布局：%v", err)
	}
	o = BuildOptions{Base: "/x/base", BaseReal: "/x/base", XDG: map[string]string{"DATA_HOME": "/x/base/xdg"}}
	if err := checkBaseContainers(o); ExitCode(err) != CodeInput {
		t.Fatalf("XDG 根在 Base 下必须被拒，得到 %v", err)
	}
}

func TestEscapeDBusAddressValue(t *testing.T) {
	cases := map[string]string{
		"/run/user/1000/bus": "/run/user/1000/bus",
		"/tmp/a b/bus":       "/tmp/a%20b/bus",
		"/tmp/中文/bus":        "/tmp/%E4%B8%AD%E6%96%87/bus",
		"/tmp/a,b;c/bus":     "/tmp/a%2Cb%3Bc/bus",
		"/tmp/100%/bus":      "/tmp/100%25/bus",
		"/tmp/a*b=c:d/bus":   "/tmp/a%2Ab%3Dc%3Ad/bus",
	}
	for in, want := range cases {
		if got := escapeDBusAddressValue(in); got != want {
			t.Fatalf("escape(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// 最小 busconfig 必须没有 servicedir：否则请求 org.freedesktop.secrets
// 会激活宿主 keyring daemon，独立 keyring 直接失效。
func TestWriteSessionConfHasNoServiceDirs(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "a b", "bus")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "session.conf")
	if err := writeSessionConf(conf, sock); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, bad := range []string{"servicedir", "standard_session_servicedirs", "includedir"} {
		if strings.Contains(s, bad) {
			t.Fatalf("配置里不能出现 %s：\n%s", bad, s)
		}
	}
	if !strings.Contains(s, "<listen>unix:path="+escapeDBusAddressValue(sock)+"</listen>") {
		t.Fatalf("listen 必须是转义后的路径：\n%s", s)
	}
}

// 总线目录就是放 session.conf 与 bus 的目录：run 传 <inst>/dbus，
// bwrap 的 bind 源是 <inst>/dbus/bus，两者必须同路径。
// 目录显式建在 /tmp 下：t.TempDir() 会带测试名，TMPDIR 很长时会触发
// 短路径回退，让"规范布局"的断言变成环境相关的假失败。
func TestStartSessionBusLayoutAndStop(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("没有 dbus-daemon")
	}
	e := testEnv(t)
	short, err := os.MkdirTemp("/tmp", "acbus-")
	if err != nil {
		t.Skip("没有可写的 /tmp")
	}
	defer os.RemoveAll(short)
	busDir := filepath.Join(short, "dbus")
	sess, err := e.startSessionBus(busDir)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Addr() == "" || !strings.HasPrefix(sess.Addr(), "unix:path=") {
		t.Fatalf("地址异常：%q", sess.Addr())
	}
	if _, err := os.Stat(filepath.Join(busDir, "session.conf")); err != nil {
		t.Fatalf("session.conf 必须在 busDir：%v", err)
	}
	if _, err := os.Stat(filepath.Join(busDir, "bus")); err != nil {
		t.Fatalf("socket 必须在 busDir/bus：%v", err)
	}
	sess.Stop()
	if _, err := os.Stat(filepath.Join(busDir, "bus")); !os.IsNotExist(err) {
		t.Fatalf("Stop 后 socket 必须删除：%v", err)
	}
}

// 私有总线上请求 secrets 服务必须拿到 ServiceUnknown / NameHasOwner=false：
// 这是"禁服务激活"的可观测证据（宿主 keyring 不会被拉起来）。
func TestPrivateBusDoesNotActivateSecrets(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("没有 dbus-daemon")
	}
	if _, err := exec.LookPath("dbus-send"); err != nil {
		t.Skip("没有 dbus-send")
	}
	e := testEnv(t)
	sess, err := e.startSessionBus(filepath.Join(t.TempDir(), "dbus"))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Stop()
	out, err := exec.Command("dbus-send", "--bus="+sess.Addr(), "--print-reply",
		"--dest=org.freedesktop.DBus", "/org/freedesktop/DBus",
		"org.freedesktop.DBus.NameHasOwner", "string:org.freedesktop.secrets").CombinedOutput()
	if err != nil {
		t.Fatalf("NameHasOwner 调用失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "boolean false") {
		t.Fatalf("secrets 不该有 owner（服务激活被禁）：\n%s", out)
	}
}

// socket 路径超过 dbus-daemon 上限时必须退到短目录：AC_BASE/TMPDIR 很长时
// 否则私有总线根本起不来（dbus-daemon 报 "Socket name too long"，实测
// 100 字节即失败），而失败会静默降级成共享宿主总线。
func TestStartSessionBusFallsBackWhenPathTooLong(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("没有 dbus-daemon")
	}
	e := testEnv(t)
	deep := filepath.Join(t.TempDir(), strings.Repeat("deep/", 30), "dbus")
	if len(filepath.Join(deep, "bus")) <= dbusSockMax {
		t.Fatalf("构造的路径不够长，用例失去意义：%s", filepath.Join(deep, "bus"))
	}
	sess, err := e.startSessionBus(deep)
	if err != nil {
		t.Fatalf("长路径应回退到短目录而不是失败：%v", err)
	}
	sock := sess.Sock()
	if sock == "" || len(sock) > dbusSockMax {
		t.Fatalf("回退后的 socket 路径必须在上限内：%q（%d）", sock, len(sock))
	}
	if filepath.Dir(sock) == filepath.Join(deep) {
		t.Fatalf("不应还落在原来的长目录：%s", sock)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("回退目录里必须真的有 socket：%v", err)
	}
	if fi, err := os.Lstat(filepath.Dir(sock)); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("回退目录必须是私有权限：%v", fi.Mode())
	}
	dir := filepath.Dir(sock)
	sess.Stop()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Stop 后回退目录应删掉：%v", err)
	}
}

// 回退目录在 /tmp 这类全局可写目录里，同名软链必须先被拒：跟随软链会在
// 攻击者指定的位置建 socket。
func TestEnsureBusDirRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureBusDir(link, os.Getuid()); err == nil {
		t.Fatal("软链目录必须被拒")
	}
}

// 私有总线起不来（DBusAddress 为空）时是明确的"降级为共享宿主总线"：
// runtime 目录仍被替换，宿主 bus 必须经 RuntimeSocks 绑回去，否则应用继承的
// 宿主地址指向空目录里的占位文件，工具却打印"共用总线"。
func TestBuildBwrapArgsDegradedBusKeepsHostBus(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	bus := filepath.Join(o.RuntimeDir, "bus")
	o.RuntimeSocks = []BindPair{{Src: bus, Dst: bus}}
	p := Profile{App: "x", Version: ProfileVersion, Exec: "/bin/true",
		Paths: []string{"~/.config/X"}, IsolateDBus: true, IsolateRuntime: true}
	in := Instance{App: "x", ID: "work", Root: e.InstancePath("x", "work") + "/root"}
	got, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--ro-bind-try "+bus+" "+bus) {
		t.Fatalf("降级路径必须把宿主 bus 绑回私有 runtime：%s", joined)
	}
	if strings.Contains(joined, "--setenv DBUS_SESSION_BUS_ADDRESS") {
		t.Fatalf("没有私有总线时不能注入私有地址：%s", joined)
	}
}

// 不隔离 runtime/dbus 的普通档案把 AC_BASE 放在 $XDG_RUNTIME_DIR 下是合法的：
// 这些目录不会被 bind，不能误报 base_exposure。只有真的会 bind runtime 时才拒。
func TestCheckBaseVsLateBindsOnlyWhenRuntimeBound(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	o.Base = filepath.Join(o.RuntimeDir, "appclone")
	o.BaseReal = o.Base
	in := Instance{App: "x", ID: "work", Root: e.InstancePath("x", "work") + "/root"}
	p := Profile{App: "x", Version: ProfileVersion, Exec: "/bin/true", Paths: []string{"~/.config/X"}}
	if _, err := BuildBwrapArgs(p, in, o); err != nil {
		t.Fatalf("没有 runtime bind 时不该拒：%v", err)
	}
	p.IsolateRuntime = true
	if _, err := BuildBwrapArgs(p, in, o); ExitCode(err) != CodeInput {
		t.Fatalf("有 runtime bind 时必须拒，得到 %v", err)
	}
}

// 宿主总线可能在 /tmp（dbus-launch）；私有总线在跑时要用 /dev/null 遮住，
// 否则应用能绕过私有总线连回宿主。总线失败时不能遮（那是降级共享）。
func TestBuildBwrapArgsMasksHostBus(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	o.HostBusPaths = []string{"/tmp/dbus-hostbus"}
	p := Profile{App: "x", Version: ProfileVersion, Exec: "/bin/true",
		Paths: []string{"~/.config/X"}, IsolateDBus: true}
	in := Instance{App: "x", ID: "work", Root: e.InstancePath("x", "work") + "/root"}
	o.DBusAddress = "unix:path=/host/bus"
	o.DBusAppAddress = "unix:path=/run/user/1000/bus"
	got, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(got, " "), "--bind /dev/null /tmp/dbus-hostbus") {
		t.Fatalf("私有总线在跑时必须遮蔽宿主总线：%v", got)
	}
	o.DBusAddress = ""
	got, err = BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(got, " "), "/dev/null /tmp/dbus-hostbus") {
		t.Fatalf("降级共享时不能遮蔽宿主总线：%v", got)
	}
}

func TestParseDBusUnixPaths(t *testing.T) {
	got := parseDBusUnixPaths("unix:path=/tmp/a%20b/bus,guid=1;unix:abstract=/tmp/abs;unix:path=/run/bus;unix:path=/tmp/a b/bus")
	want := []string{"/run/bus", "/tmp/a b/bus"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("解析结果 %v，期望 %v", got, want)
	}
	if got := parseDBusUnixPaths(""); len(got) != 0 {
		t.Fatalf("空地址应得到空清单：%v", got)
	}
}

// 遮蔽清单只能包含真实存在的 socket：陈旧路径若进 argv，bwrap 会在宿主创建
// 0444 空文件；目录路径会让整条 bwrap 启动失败。软链别名必须解析成真实
// socket——bwrap 拒绝在最终分量是软链的目标上挂载。
func TestHostBusPathsOnlyExistingSockets(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "bus")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("无法创建 unix socket：%v", err)
	}
	defer ln.Close()
	alias := filepath.Join(dir, "alias-bus")
	if err := os.Symlink(sock, alias); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale-bus")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS",
		"unix:path="+alias+";unix:path="+sock+";unix:path="+stale+";unix:path="+dir+";unix:abstract=/tmp/abs")
	got := testEnv(t).hostBusPaths()
	if len(got) != 1 || got[0] != sock {
		t.Fatalf("软链应解析成真实 socket 且去重，得到 %v", got)
	}
}

// probe 的遮蔽位置在 --dev-bind/--proc 之后：宿主总线在 /dev/shm 时不能被
// dev-bind 覆盖回去。
func TestProbeBwrapArgsMasksHostBus(t *testing.T) {
	argv, err := ProbeBwrapArgs(ProbeSpec{
		Home: "/h", Base: "/h/.local/share/appclone", NoOverlay: true,
		DBusAddress: "unix:path=/h/run/bus", HostBusPaths: []string{"/dev/shm/dbus-host"},
		Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--bind /dev/null /dev/shm/dbus-host") {
		t.Fatalf("probe 必须遮蔽宿主总线：%s", joined)
	}
	if strings.Index(joined, "--proc /proc") > strings.Index(joined, "--bind /dev/null /dev/shm/dbus-host") {
		t.Fatalf("遮蔽必须在 --dev-bind/--proc 之后：%s", joined)
	}
	// 没有注入地址（旧调用方）时不能遮蔽：应用继承宿主地址，遮了等于断网。
	argv, err = ProbeBwrapArgs(ProbeSpec{
		Home: "/h", Base: "/h/.local/share/appclone", NoOverlay: true,
		HostBusPaths: []string{"/dev/shm/dbus-host"}, Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), "/dev/null /dev/shm/dbus-host") {
		t.Fatalf("没有私有地址时不能遮蔽：%v", argv)
	}
}

// 新格式凭证同时缺 masked_base 与 base：必须给 warning，并且方向仍是
// extract_failed（用当前 e.Base 兜底），不能掉成 leak-detected。
func TestJudgeMaskedBaseBothEmpty(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	getenv := func(k string) string {
		switch k {
		case "XDG_CONFIG_HOME":
			return filepath.Join(home, ".config")
		case "XDG_DATA_HOME":
			return filepath.Join(home, ".local", "share")
		case "XDG_STATE_HOME":
			return filepath.Join(home, ".local", "state")
		case "XDG_CACHE_HOME":
			return filepath.Join(home, ".cache")
		case "XDG_RUNTIME_DIR":
			return filepath.Join(root, "run")
		}
		return ""
	}
	base := filepath.Join(home, ".local", "share", "appclone")
	e := NewEnvAt(base, newResolver(home, home, 1000, getenv))
	e.Trasher = func(paths ...string) error { return nil }
	e.CliPath = "/usr/bin/appclone"
	p := Profile{Paths: []string{"~/.config/TestApp"}}
	entries := []UpperEntry{{Path: ".local/share/appclone/instances/x/root/token", Kind: EntryFile}}
	rec := runRecord{Paths: []string{filepath.Join(home, ".config", "TestApp")}, BindsRecorded: true}
	v := e.judge(p, entries, &rec)
	if v.Conclusion != VerdictUnverified || v.Reason != "extract_failed" {
		t.Fatalf("两者都空时仍应判 extract_failed，得到 %s/%s", v.Conclusion, v.Reason)
	}
	if len(v.Warnings) != 1 || v.Warnings[0] != "masked_base_missing" {
		t.Fatalf("两者都空时必须给 warning，得到 %v", v.Warnings)
	}
}

// 回退目录权限必须是 0700：/tmp 里其他用户能进的目录等于把私有总线暴露出去。
func TestEnsureBusDirTightensPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "busdir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureBusDir(dir, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("权限应被收紧到 0700，得到 %v", fi.Mode().Perm())
	}
}

// 应用侧地址优先：AC_BASE 被遮蔽后宿主 socket 路径在沙箱里不存在，
// --setenv 必须写命名空间内的路径；没有应用侧地址时才退回宿主原文（probe）。
func TestBuildBwrapArgsPrefersAppDBusAddress(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	o.DBusAddress = "unix:path=/host/bus,guid=abc"
	o.DBusAppAddress = "unix:path=/run/user/1000/bus%20priv"
	p := Profile{App: "x", Version: ProfileVersion, Exec: "/bin/true",
		Paths: []string{"~/.config/X"}, IsolateDBus: true}
	in := Instance{App: "x", ID: "work", Root: e.InstancePath("x", "work") + "/root"}
	got, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--setenv DBUS_SESSION_BUS_ADDRESS "+o.DBusAppAddress) {
		t.Fatalf("必须用应用侧地址：%s", joined)
	}
	if strings.Contains(joined, "guid=abc") {
		t.Fatalf("宿主原文不该进沙箱：%s", joined)
	}
}

// AC_BASE 现在是 tmpfs 遮蔽路径：Base 下出现写入说明遮蔽没生效，
// 必须判 extract_failed（作废审计），不能掉成普通 leak（下一步是重新探测）。
func TestJudgeMaskedBase(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	getenv := func(k string) string {
		switch k {
		case "XDG_CONFIG_HOME":
			return filepath.Join(home, ".config")
		case "XDG_DATA_HOME":
			return filepath.Join(home, ".local", "share")
		case "XDG_STATE_HOME":
			return filepath.Join(home, ".local", "state")
		case "XDG_CACHE_HOME":
			return filepath.Join(home, ".cache")
		case "XDG_RUNTIME_DIR":
			return filepath.Join(root, "run")
		}
		return ""
	}
	base := filepath.Join(home, ".local", "share", "appclone")
	e := NewEnvAt(base, newResolver(home, home, 1000, getenv))
	e.Trasher = func(paths ...string) error { return nil }
	e.CliPath = "/usr/bin/appclone"
	p := Profile{Paths: []string{"~/.config/TestApp"}}
	entries := []UpperEntry{{Path: ".local/share/appclone/instances/x/root/token", Kind: EntryFile}}

	rec := runRecord{Paths: []string{filepath.Join(home, ".config", "TestApp")},
		MaskedBase: base, BindsRecorded: true}
	v := e.judge(p, entries, &rec)
	if v.Conclusion != VerdictUnverified || v.Reason != "extract_failed" {
		t.Fatalf("MaskedBase 下的写入应判 extract_failed，得到 %s/%s", v.Conclusion, v.Reason)
	}
	if len(v.Warnings) != 0 {
		t.Fatalf("正常新记录不该有 warning：%v", v.Warnings)
	}

	// 新记录异常缺 masked_base：退回 Base，方向仍是 extract_failed，但必须可见。
	rec = runRecord{Paths: []string{filepath.Join(home, ".config", "TestApp")},
		Base: base, BindsRecorded: true}
	v = e.judge(p, entries, &rec)
	if v.Reason != "extract_failed" {
		t.Fatalf("缺 masked_base 仍应判 extract_failed，得到 %s/%s", v.Conclusion, v.Reason)
	}
	if len(v.Warnings) != 1 || v.Warnings[0] != "masked_base_missing" {
		t.Fatalf("缺 masked_base 必须给 warning，得到 %v", v.Warnings)
	}

	// 旧记录（无 binds_recorded）按 e.Base 推断，不产生该 warning。
	old := runRecord{}
	if v := e.judge(p, entries, &old); v.Reason != "extract_failed" || len(v.Warnings) != 0 {
		t.Fatalf("旧记录应按 e.Base 判 extract_failed 且无 warning，得到 %s/%s %v", v.Conclusion, v.Reason, v.Warnings)
	}
}

// 校验 2：AC_BASE 落在 tmpfs 之后才绑的 runtime 目录/宿主 socket 里时，
// `--ro-bind-try` 会把整个目录绑回来，Base 重新可见——必须在 argv 阶段报 2。
func TestBuildBwrapArgsRejectsBaseUnderRuntimeSocks(t *testing.T) {
	e := testEnv(t)
	o := e.opts()
	x11 := filepath.Join(o.RuntimeDir, "X11-unix")
	o.Base = filepath.Join(x11, "appclone")
	o.BaseReal = o.Base
	o.RuntimeSocks = []BindPair{{Src: x11, Dst: x11}}
	p := Profile{App: "x", Version: ProfileVersion, Exec: "/bin/true",
		Paths: []string{"~/.config/X"}, IsolateRuntime: true}
	in := Instance{App: "x", ID: "work", Root: e.InstancePath("x", "work") + "/root"}
	_, err := BuildBwrapArgs(p, in, o)
	if ExitCode(err) != CodeInput || !strings.Contains(errKey(err), "err.profile.base_exposure") {
		t.Fatalf("Base 在 runtime socket 目录里应报 2/base_exposure，得到 %v", err)
	}

	// 无关 socket 不该误拒。
	o.Base = filepath.Join(e.Base, "x")
	o.BaseReal = o.Base
	o.RuntimeSocks = []BindPair{{Src: filepath.Join(o.RuntimeDir, "wayland-0"), Dst: filepath.Join(o.RuntimeDir, "wayland-0")}}
	if _, err := BuildBwrapArgs(p, in, o); err != nil {
		t.Fatalf("无关 socket 不该被拒：%v", err)
	}
}

// 探测总线起不来时必须注入不存在的私有地址：留空会让探测应用继承宿主
// DBUS_SESSION_BUS_ADDRESS，反而连回宿主 keyring（fail-open）。
func TestProbeBusFailureUsesUnavailableAddress(t *testing.T) {
	addr := unavailableBusAddress("/w/dbus")
	if addr == "" || !strings.HasPrefix(addr, "unix:path=") {
		t.Fatalf("必须是有效的私有地址：%q", addr)
	}
	argv, err := ProbeBwrapArgs(ProbeSpec{
		Home: "/h", Base: "/h/.local/share/appclone", NoOverlay: true,
		DBusAddress: addr, Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--setenv DBUS_SESSION_BUS_ADDRESS "+addr) {
		t.Fatalf("必须显式设置无效私有地址：%s", joined)
	}
}

// 旧审计记录写了 base 时必须用它：AC_BASE 改过后按当前 e.Base 重放会漏判。
func TestJudgeOldRecordUsesRecordedBase(t *testing.T) {
	e := testEnv(t)
	p := Profile{Paths: []string{"~/.config/TestApp"}}
	oldBase := filepath.Join(e.Resolver.Home, "oldbase")
	entries := []UpperEntry{{Path: "oldbase/x", Kind: EntryFile}}
	rec := runRecord{Base: oldBase}
	if v := e.judge(p, entries, &rec); v.Reason != "extract_failed" {
		t.Fatalf("旧记录 base 下的写入应判 extract_failed，得到 %s/%s", v.Conclusion, v.Reason)
	}
}

// Warnings 必须能落进 summary.toml 并在重放时保留，否则 masked_base_missing
// 只在第一次看到，用户回看摘要时异常消失。
func TestSummaryPersistsWarnings(t *testing.T) {
	sum := Summary{Conclusion: VerdictVerified, Warnings: []string{"masked_base_missing"}}
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(sum); err != nil {
		t.Fatal(err)
	}
	var got Summary
	if _, err := toml.Decode(buf.String(), &got); err != nil {
		t.Fatal(err)
	}
	v := got.Verdict()
	if len(v.Warnings) != 1 || v.Warnings[0] != "masked_base_missing" {
		t.Fatalf("重放后 warnings 丢失：%v", v.Warnings)
	}
}
