package core

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestInstance(t *testing.T) Instance {
	t.Helper()
	dir := t.TempDir()
	in := Instance{App: "testapp", ID: "main", Root: filepath.Join(dir, "root")}
	if err := os.MkdirAll(in.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	return in
}

// 密码文件：0600、32 字节 hex；重复调用不重写；已有 keyring 文件但缺密码
// 时绝不补一个（猜错会让文件永久打不开）。
func TestEnsureKeyringPasswordLifecycle(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	if err := e.EnsureKeyring(in); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	pwPath := filepath.Join(keyringDir(in), "keyring.pw")
	fi, err := os.Stat(pwPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("密码文件必须是 0600，得到 %v", fi.Mode().Perm())
	}
	pw1, err := readKeyringPassword(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(pw1) != keyringPwBytes*2 {
		t.Fatalf("密码应为 %d 个 hex 字符，得到 %d", keyringPwBytes*2, len(pw1))
	}
	if err := e.EnsureKeyring(in); err != nil {
		t.Fatal(err)
	}
	pw2, err := readKeyringPassword(in)
	if err != nil {
		t.Fatal(err)
	}
	if pw1 != pw2 {
		t.Fatal("密码一旦创建绝不重写")
	}

	// 已有 keyring 文件但缺密码：报错、不补、不删数据。
	if err := os.WriteFile(filepath.Join(keyringDataDir(in), "login.keyring"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pwPath); err != nil {
		t.Fatal(err)
	}
	err = e.EnsureKeyring(in)
	if errKey(err) != "err.env.keyring_pw_missing" {
		t.Fatalf("缺密码应报 keyring_pw_missing，得到 %v", err)
	}
	if _, serr := os.Stat(pwPath); !os.IsNotExist(serr) {
		t.Fatal("缺密码时不得自动补写")
	}
	if _, serr := os.Stat(filepath.Join(keyringDataDir(in), "login.keyring")); serr != nil {
		t.Fatal("已有 keyring 文件不得被删除")
	}
}

// 两个进程/协程同时建密码：O_EXCL 保证只有一个写成功，另一个接受已有密码。
func TestEnsureKeyringConcurrent(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.EnsureKeyring(in)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发创建失败：%v", err)
		}
	}
	pw, err := readKeyringPassword(in)
	if err != nil || len(pw) != keyringPwBytes*2 {
		t.Fatalf("并发后密码不可用：%q %v", pw, err)
	}
}

// 五站点（除 doctor 的地址构造外都在 argv 里）：isolate_keyring=true 且
// isolate_dbus=false 时，行为必须与开启 isolate_dbus 完全一致。
func TestBuildBwrapArgsKeyringOnly(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	p := Profile{
		App: "testapp", Exec: "/opt/app", Paths: []string{"~/.config/TestApp"},
		ProbeMethod: "manual", IsolateKeyring: true,
	}
	o := e.opts()
	o.DBusAddress = "unix:path=/run/user/1000/bus"
	o.DBusAppAddress = AppDBusAddress(o.RuntimeDir)
	o.BusSock = filepath.Join(in.Dir(), "dbus", "bus")
	hostBus := filepath.Join(o.RuntimeDir, "bus")
	o.HostBusPaths = []string{hostBus}
	o.RuntimeSocks = []BindPair{
		{Src: hostBus, Dst: hostBus},
		{Src: "/run/user/1000/wayland-0", Dst: filepath.Join(o.RuntimeDir, "wayland-0")},
	}
	argv, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\x00")
	must := []string{
		"--bind\x00/dev/null\x00" + hostBus, // 宿主总线遮蔽
		"--bind\x00" + o.BusSock + "\x00" + filepath.Join(o.RuntimeDir, "bus"),
		"--setenv\x00DBUS_SESSION_BUS_ADDRESS\x00" + o.DBusAppAddress,
		"--bind\x00" + filepath.Join(in.Dir(), "runtime") + "\x00" + o.RuntimeDir,
		"--bind\x00" + keyringDataDir(in) + "\x00" + filepath.Join(o.XDG["XDG_DATA_HOME"], "keyrings"),
		"--bind\x00" + keyringDataDir(in) + "\x00" + filepath.Join(o.XDG["XDG_CONFIG_HOME"], "keyrings"),
	}
	for _, m := range must {
		if !strings.Contains(joined, m) {
			t.Fatalf("keyring-only argv 缺少 %q\n%s", m, strings.Join(argv, "\n"))
		}
	}
	// 宿主 bus 不能再经 RuntimeSocks 绑回私有 runtime。
	if strings.Contains(joined, "--ro-bind-try\x00"+hostBus) {
		t.Fatal("keyring-only 时宿主 bus 必须被过滤")
	}
	if !strings.Contains(joined, "--ro-bind-try\x00/run/user/1000/wayland-0") {
		t.Fatal("非 bus 的宿主 socket 应照常引入")
	}
}

// 总线地址为空（doctor 的离线基线）时 keyring 仍要求私有 runtime，
// 但不产生 bus bind。
func TestBuildBwrapArgsKeyringWithoutBusAddress(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	p := Profile{App: "testapp", Exec: "/opt/app", Paths: []string{"~/.config/TestApp"}, ProbeMethod: "manual", IsolateKeyring: true}
	o := e.opts()
	argv, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\x00")
	if !strings.Contains(joined, "--bind\x00"+filepath.Join(in.Dir(), "runtime")+"\x00"+o.RuntimeDir) {
		t.Fatal("keyring 必须隐含私有 runtime")
	}
	if strings.Contains(joined, "DBUS_SESSION_BUS_ADDRESS") {
		t.Fatal("没有总线地址时不该产生 bus setenv")
	}
}

// 两个 keyring 目标不得与 AC_BASE 重叠。
func TestValidateProfileKeyringTargetExposure(t *testing.T) {
	e := testEnv(t)
	o := e.BuildOptions()
	o.XDG = map[string]string{
		"XDG_DATA_HOME":   e.Base,
		"XDG_CONFIG_HOME": filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "ok"),
	}
	p := Profile{App: "app", Exec: "/opt/app", Paths: []string{"~/.config/App"}, IsolateKeyring: true}
	if err := ValidateProfile(p, o); errKey(err) != "err.profile.base_exposure" {
		t.Fatalf("keyring 目标落在 Base 下应被拒绝，得到 %v", err)
	}
}

// 目标准备的三类情形：不存在→建；是文件→报错；软链到另一个 keyring 目标→
// 跳过；软链到别处→报错。
func TestPrepareKeyringTargets(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	if err := os.MkdirAll(keyringDataDir(in), 0o700); err != nil {
		t.Fatal(err)
	}
	targets := e.BuildOptions().keyringTargets()
	if len(targets) != 2 {
		t.Fatalf("测试环境应有两个 keyring 目标，得到 %v", targets)
	}

	created, err := e.prepareKeyringTargets(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) == 0 {
		t.Fatal("目标不存在时应记录本次新建的目录（供回滚）")
	}
	// 源目录本身也可能还不存在（建实例时只建目录，不预造密码）：必须一并
	// 建出来并计入回滚清单，否则 bind 源缺失会在 run 时直接失败。
	if err := os.RemoveAll(keyringDataDir(in)); err != nil {
		t.Fatal(err)
	}
	created, err = e.prepareKeyringTargets(in)
	if err != nil {
		t.Fatalf("源目录缺失时应自动建出来：%v", err)
	}
	if fi, serr := os.Stat(keyringDataDir(in)); serr != nil || !fi.IsDir() {
		t.Fatalf("源目录没有建出来：%v %v", fi, serr)
	}
	if !slices.Contains(created, keyringDataDir(in)) {
		t.Fatalf("源目录必须计入回滚清单：%v", created)
	}
	if _, serr := os.Stat(filepath.Join(keyringDir(in), "keyring.pw")); !os.IsNotExist(serr) {
		t.Fatal("准备目标不得预造密码文件")
	}
	for _, tgt := range targets {
		fi, err := os.Stat(tgt)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("目标 %s 应为 0700 目录：%v %v", tgt, fi, err)
		}
	}

	// 目标是普通文件 → 报错。
	if err := os.RemoveAll(targets[1]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targets[1], nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareKeyringTargets(in); errKey(err) != "err.profile.keyring_target" {
		t.Fatalf("目标为文件应报 keyring_target，得到 %v", err)
	}
	if err := os.Remove(targets[1]); err != nil {
		t.Fatal(err)
	}

	// 软链到另一个 keyring 目标 → 跳过（另一个 bind 已覆盖）。
	if err := os.Symlink(targets[0], targets[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareKeyringTargets(in); err != nil {
		t.Fatalf("软链到另一个 keyring 目标应跳过，得到 %v", err)
	}
	if err := os.Remove(targets[1]); err != nil {
		t.Fatal(err)
	}

	// 软链到别处 → 报错，不静默跟随。
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, targets[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareKeyringTargets(in); errKey(err) != "err.profile.keyring_target" {
		t.Fatalf("软链到别处应报 keyring_target，得到 %v", err)
	}
}

// P1：keyring-only 时总线起不来必须硬失败；仅 dbus 时保留降级告警。
func TestStartRunBusFailureDirections(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	o := e.BuildOptions()
	empty := t.TempDir()
	t.Setenv("PATH", empty)

	var warn bytes.Buffer
	sess, err := e.startRunBus(Profile{IsolateKeyring: true}, in, &o, &warn)
	if sess != nil || errKey(err) != "err.env.keyring_requires_bus" {
		t.Fatalf("keyring-only 必须硬失败，得到 sess=%v err=%v", sess, err)
	}

	warn.Reset()
	sess, err = e.startRunBus(Profile{IsolateDBus: true}, in, &o, &warn)
	if err != nil || sess != nil {
		t.Fatalf("仅 dbus 应降级：sess=%v err=%v", sess, err)
	}
	if !strings.Contains(warn.String(), "共用会话总线") {
		t.Fatalf("降级必须明确告知，得到 %q", warn.String())
	}
}

// 就绪三重：服务存在 → 默认集合 → Unlock 返回 prompt "/" 且数组非空。
func TestWaitKeyringReady(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *NameHasOwner*) echo "   boolean true"; exit 0;;
  *ReadAlias*) echo '   object path "/org/freedesktop/secrets/collection/login"'; exit 0;;
  *Unlock*) printf '   array [\n      object path "/org/freedesktop/secrets/collection/login"\n   ]\n   object path "/"\n'; exit 0;;
esac
exit 1
`
	fake := filepath.Join(dir, "dbus-send-ok")
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := waitKeyringReady(fake, "unix:path=/tmp/bus"); err != nil {
		t.Fatalf("应就绪：%v", err)
	}

	locked := strings.Replace(script, `object path "/"`, `object path "/org/freedesktop/secrets/prompt/1"`, 1)
	lf := filepath.Join(dir, "dbus-send-locked")
	if err := os.WriteFile(lf, []byte(locked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := waitKeyringReady(lf, "unix:path=/tmp/bus"); errKey(err) != "err.env.keyring_not_ready" {
		t.Fatalf("未解锁应报 keyring_not_ready，得到 %v", err)
	}

	// ReadAlias 无默认别名时返回 "/" 哨兵：必须退回 login 集合再 Unlock，
	// 否则会拿 `array:objpath:/` 去调用并永远等不到解锁数组。这里让 Unlock
	// 只对 login 集合成功，回退没发生就会超时报 keyring_not_ready。
	sentinel := `#!/bin/sh
case "$*" in
  *NameHasOwner*) echo "   boolean true"; exit 0;;
  *ReadAlias*) echo '   object path "/"'; exit 0;;
  *Unlock*collection/login*) printf '   array [\n      object path "/org/freedesktop/secrets/collection/login"\n   ]\n   object path "/"\n'; exit 0;;
esac
exit 1
`
	sf := filepath.Join(dir, "dbus-send-alias-sentinel")
	if err := os.WriteFile(sf, []byte(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := waitKeyringReady(sf, "unix:path=/tmp/bus"); err != nil {
		t.Fatalf("ReadAlias 返回 / 哨兵时应退回 login 集合：%v", err)
	}

	missing := strings.Replace(script, "boolean true", "boolean false", 1)
	mf := filepath.Join(dir, "dbus-send-missing")
	if err := os.WriteFile(mf, []byte(missing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := waitKeyringReady(mf, "unix:path=/tmp/bus"); errKey(err) != "err.env.keyring_not_ready" {
		t.Fatalf("服务缺失应报 keyring_not_ready，得到 %v", err)
	}
}

// Stop 与 dbusSession 同构：SIGTERM 后等在途 Wait 关闭 Done，不重复 Wait。
func TestKeyringSessionStop(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep 不可用：%v", err)
	}
	// daemon 会在 <runDir>/keyring 留下 control/ssh socket；Stop 必须只删
	// socket 类型并回收空目录，否则下一次 run 复用目录会撞上残留 socket。
	runDir := t.TempDir()
	kdir := filepath.Join(runDir, "keyring")
	if err := os.MkdirAll(kdir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(kdir, "control"))
	if err != nil {
		t.Skipf("unix socket 不可用：%v", err)
	}
	defer ln.Close()
	keep := filepath.Join(kdir, "keep.txt")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sess := &keyringSession{cmd: cmd, done: make(chan struct{}), runDir: runDir}
	go func() {
		_ = cmd.Wait()
		close(sess.done)
	}()
	done := make(chan struct{})
	go func() {
		sess.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 没有在 5 秒内完成")
	}
	select {
	case <-sess.Done():
	default:
		t.Fatal("Stop 返回时 Done 应已关闭")
	}
	if _, err := os.Lstat(filepath.Join(kdir, "control")); !os.IsNotExist(err) {
		t.Fatalf("Stop 必须删掉 daemon 残留 socket，得到 err=%v", err)
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Fatalf("Stop 不得删除非 socket 文件：%v", err)
	}
}

// 临时 keyring 目录绝不能落在 AC_BASE 下；系统临时目录不安全时回退到
// $XDG_RUNTIME_DIR 的候选。
func TestProbeKeyringRootAvoidsBase(t *testing.T) {
	e := testEnv(t)
	underBase := filepath.Join(e.Base, "tmp")
	if err := os.MkdirAll(underBase, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", underBase)
	if err := os.MkdirAll(e.Resolver.XDG["RUNTIME_DIR"], 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := e.probeKeyringRoot()
	if err != nil {
		t.Fatalf("应回退到 runtime dir：%v", err)
	}
	defer os.RemoveAll(root)
	base := e.BuildOptions().BaseReal
	if Real(root) == base || IsUnder(base, Real(root)) {
		t.Fatalf("临时 keyring 落在 AC_BASE 下：%s", root)
	}
	if !IsUnder(e.Resolver.XDG["RUNTIME_DIR"], root) {
		t.Fatalf("应回退到 XDG_RUNTIME_DIR，得到 %s", root)
	}
}

// 临时 keyring 起不来只告警、探测继续，并且临时目录要清理干净。
func TestProbeKeyringFailureWarnsAndCleans(t *testing.T) {
	e := testEnv(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	bin := t.TempDir()
	fakeSend := filepath.Join(bin, "dbus-send")
	if err := os.WriteFile(fakeSend, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeDaemon := filepath.Join(bin, "gnome-keyring-daemon")
	if err := os.WriteFile(fakeDaemon, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.KeyringCapsOverride = &KeyringCaps{Available: true, Daemon: fakeDaemon, DbusSend: fakeSend}
	var stderr bytes.Buffer
	sess := e.startProbeKeyring("unix:path=/tmp/bus", ProbeOptions{Stderr: &stderr})
	if sess != nil {
		t.Fatal("就绪失败时应放弃临时 keyring")
	}
	if !strings.Contains(stderr.String(), "未启用探测期临时密钥环") {
		t.Fatalf("失败必须告警，得到 %q", stderr.String())
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), "appclone-probe-keyring-") {
			t.Fatalf("失败后临时目录没清理：%s", ent.Name())
		}
	}
}

// ⑦.5 的全序 golden：keyring bind 必须排在 paths / shared_writes / extra_binds
// 之后、lock_files / runtime 之前；软链目标解析后与同组另一个目标同目录时
// 不得出现在 argv（bwrap 拒绝在软链目标上挂载）。
func TestBuildBwrapArgsKeyringOrderAndSymlinkSkip(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	keyringSrc := keyringDataDir(in)
	p := Profile{
		App:            "testapp",
		Version:        ProfileVersion,
		Exec:           "/opt/app",
		Paths:          []string{"~/.config/TestApp"},
		SharedWrites:   []string{"~/.config/gtk-3.0/bookmarks"},
		ExtraBinds:     []BindPair{{Src: "~/.config/extra", Dst: "~/.config/extra"}},
		LockFiles:      []string{"~/.config/TestApp/lock"},
		IsolateKeyring: true,
		ProbeMethod:    "manual",
	}
	o := e.opts()
	targets := o.keyringBinds()
	if len(targets) != 2 {
		t.Fatalf("测试环境应有两个 keyring 目标，得到 %v", targets)
	}
	argv, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	pathDst := o.Home + "/.config/TestApp"
	sharedDst := o.Home + "/.config/gtk-3.0/bookmarks"
	extraDst := o.Home + "/.config/extra"
	lockDst := o.Home + "/.config/TestApp/lock"
	lockSrc := in.Dir() + "/locks/" + lockFileName(lockDst)
	order := [][]string{
		{"--bind", instSrc(in.Root, pathDst), pathDst},  // ⑤
		{"--bind-try", sharedDst, sharedDst},            // ⑥
		{"--bind", o.Home + "/.config/extra", extraDst}, // ⑦
		{"--bind", keyringSrc, targets[0]},              // ⑦.5 数据目标
		{"--bind", keyringSrc, targets[1]},              // ⑦.5 配置目标
		{"--bind", lockSrc, lockDst},                    // ⑧
		{"--bind", in.Dir() + "/runtime", o.RuntimeDir}, // ⑨
	}
	prev := -1
	for _, seq := range order {
		idx := indexOfSeq(argv, seq)
		if idx < 0 {
			t.Fatalf("argv 缺少序列 %v", seq)
		}
		if idx <= prev {
			t.Fatalf("序列 %v 顺序错误：位置 %d 不在 %d 之后", seq, idx, prev)
		}
		prev = idx
	}

	// 常见布局：~/.config/keyrings 是指向 ~/.local/share/keyrings 的软链。
	// 数据目标存在后软链才能解析；此时 conf 目标必须从 bind 列表里消失。
	if err := os.MkdirAll(targets[0], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(targets[1]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targets[0], targets[1]); err != nil {
		t.Fatal(err)
	}
	o2 := e.opts()
	if got := o2.keyringBinds(); len(got) != 1 || got[0] != targets[0] {
		t.Fatalf("软链目标应被跳过，得到 %v", got)
	}
	argv2, err := BuildBwrapArgs(p, in, o2)
	if err != nil {
		t.Fatal(err)
	}
	if indexOfSeq(argv2, []string{"--bind", keyringSrc, targets[1]}) >= 0 {
		t.Fatalf("软链目标不得出现在 argv（bwrap 不能在软链上挂载）：%v", argv2)
	}
	if indexOfSeq(argv2, []string{"--bind", keyringSrc, targets[0]}) < 0 {
		t.Fatalf("另一个 keyring 目标仍必须 bind：%v", argv2)
	}
}

// indexOfSeq 返回 seq 在 args 中第一次出现的起始下标，找不到返回 -1。
func indexOfSeq(args, seq []string) int {
	if len(seq) == 0 || len(seq) > len(args) {
		return -1
	}
	for i := 0; i+len(seq) <= len(args); i++ {
		ok := true
		for j, want := range seq {
			if args[i+j] != want {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// 两个 keyring 目标都软链到**同一个**目录（data/conf 都指向 ~/vault/keyrings）：
// 逐目标"被同组覆盖"会得到空列表，退回原始软链则 bwrap 拒绝挂载。必须 bind
// 解析后的真实目录，且 argv 里不出现软链目标。
func TestResolveKeyringTargetsDoubleSymlink(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	keyringSrc := keyringDataDir(in)
	p := Profile{
		App: "testapp", Version: ProfileVersion, Exec: "/opt/app",
		Paths: []string{"~/.config/TestApp"}, ProbeMethod: "manual",
		IsolateKeyring: true,
	}
	raw := e.BuildOptions().keyringTargets()
	if len(raw) != 2 {
		t.Fatalf("测试环境应有两个 keyring 目标，得到 %v", raw)
	}
	for _, tgt := range raw {
		if err := os.MkdirAll(filepath.Dir(tgt), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(tgt); err != nil {
			t.Fatal(err)
		}
	}
	vault := filepath.Join(t.TempDir(), "vault", "keyrings")
	if err := os.MkdirAll(vault, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tgt := range raw {
		if err := os.Symlink(vault, tgt); err != nil {
			t.Fatal(err)
		}
	}

	o := e.opts()
	got := o.keyringBinds()
	if len(got) != 1 {
		t.Fatalf("双侧软链应折叠成一个真实目标，得到 %v", got)
	}
	if Real(got[0]) != Real(vault) {
		t.Fatalf("必须 bind 解析后的真实目录 %s，得到 %v", vault, got)
	}
	argv, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, tgt := range raw {
		if indexOfSeq(argv, []string{"--bind", keyringSrc, tgt}) >= 0 {
			t.Fatalf("软链目标 %s 不得出现在 argv（bwrap 不能在软链上挂载）", tgt)
		}
	}
	if indexOfSeq(argv, []string{"--bind", keyringSrc, got[0]}) < 0 {
		t.Fatalf("真实目录必须出现在 argv：%v", argv)
	}
}

// doctor 用的只读目标校验：不建目录、不改状态，但结论与 add/run 一致。
func TestCheckKeyringTargets(t *testing.T) {
	e := testEnv(t)
	raw := e.BuildOptions().keyringTargets()
	if len(raw) != 2 {
		t.Fatalf("测试环境应有两个 keyring 目标，得到 %v", raw)
	}
	for _, tgt := range raw {
		if err := os.MkdirAll(filepath.Dir(tgt), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(tgt); err != nil {
			t.Fatal(err)
		}
	}
	// 都不存在：正常（首次 run 才创建），且不得顺手建出来。
	if err := e.CheckKeyringTargets(); err != nil {
		t.Fatalf("目标不存在应视为正常：%v", err)
	}
	if _, err := os.Lstat(raw[0]); !os.IsNotExist(err) {
		t.Fatalf("只读校验不得创建目录，得到 err=%v", err)
	}
	// 已存在但是文件：报 keyring_target。
	if err := os.WriteFile(raw[1], nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckKeyringTargets(); errKey(err) != "err.profile.keyring_target" {
		t.Fatalf("目标是文件应报 keyring_target，得到 %v", err)
	}
	if err := os.Remove(raw[1]); err != nil {
		t.Fatal(err)
	}
	// 软链到同一目录：允许（与 prepare 同口径）。
	vault := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vault, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tgt := range raw {
		if err := os.Symlink(vault, tgt); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.CheckKeyringTargets(); err != nil {
		t.Fatalf("双侧软链到同一目录应通过：%v", err)
	}
}

// 双侧软链指向的 vault 目录**尚不存在**时（首次运行前的常见布局），解析
// 不能失败：必须折叠成真实目录并把它建出来，否则 add 会报 keyring_target
// 或 bind 一个不存在的源。
func TestResolveKeyringTargetsMissingVault(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	raw := e.BuildOptions().keyringTargets()
	if len(raw) != 2 {
		t.Fatalf("测试环境应有两个 keyring 目标，得到 %v", raw)
	}
	for _, tgt := range raw {
		if err := os.MkdirAll(filepath.Dir(tgt), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(tgt); err != nil {
			t.Fatal(err)
		}
	}
	vault := filepath.Join(t.TempDir(), "vault", "keyrings") // 父目录也不存在
	for _, tgt := range raw {
		if err := os.Symlink(vault, tgt); err != nil {
			t.Fatal(err)
		}
	}
	o := e.opts()
	got := o.keyringBinds()
	if len(got) != 1 || got[0] != Real(vault) {
		t.Fatalf("悬空双侧软链应折叠为真实目录 %s，得到 %v", Real(vault), got)
	}
	created, err := e.prepareKeyringTargets(in)
	if err != nil {
		t.Fatalf("prepare 应建出 vault 而不是报错：%v", err)
	}
	if len(created) == 0 {
		t.Fatal("新建的 vault 必须记入回滚清单")
	}
	if fi, err := os.Stat(vault); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("vault 应为 0700 目录：%v %v", fi, err)
	}
}

// 审计写层根对普通 run 与探测会话都必须被 tmpfs 遮蔽：其他分身尚未 verify
// 的 upper 副本按真实路径可读，只在审计会话里遮自己的目录不够。
func TestAuditRootMaskedEverywhere(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	ar := e.auditRoot()
	if err := os.MkdirAll(filepath.Join(ar, "other.id", "upper", "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := Profile{
		App: "testapp", Version: ProfileVersion, Exec: "/opt/app",
		Paths: []string{"~/.config/TestApp"}, ProbeMethod: "manual",
	}
	o := e.opts()
	if o.AuditRoot != ar {
		t.Fatalf("BuildOptions 应给出存在的审计根 %s，得到 %q", ar, o.AuditRoot)
	}
	argv, err := BuildBwrapArgs(p, in, o)
	if err != nil {
		t.Fatal(err)
	}
	if indexOfSeq(argv, []string{"--tmpfs", ar}) < 0 {
		t.Fatalf("普通 run 必须遮蔽审计写层根：%v", argv)
	}
	pargv, err := ProbeBwrapArgs(ProbeSpec{
		Home: o.Home, Base: o.BaseReal, NoOverlay: true,
		Command: []string{"/bin/true"}, AuditRoot: e.auditRoot(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if indexOfSeq(pargv, []string{"--tmpfs", ar}) < 0 {
		t.Fatalf("探测会话必须遮蔽审计写层根：%v", pargv)
	}
	// 根不存在时也必须遮蔽（不能按存在性取快照）：先由 ensureAuditRoot 以
	// 0700 建出来，BuildOptions 永远给出同一个根。
	if err := os.RemoveAll(ar); err != nil {
		t.Fatal(err)
	}
	if got := e.BuildOptions().AuditRoot; got != ar {
		t.Fatalf("审计根应始终出现在 BuildOptions，得到 %q", got)
	}
	if err := e.ensureAuditRoot(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(ar); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("ensureAuditRoot 应建出 0700 目录：%v %v", fi, err)
	}

	// 两个"不重复挂载"分支：根等于 Base（④.6 已遮）与根包含 HOME（再遮会把
	// HOME 和实例 bind 一起盖掉）。前者只能断言"只有 ④.6 那一次"。
	countTmpfs := func(argv []string, target string) int {
		n := 0
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--tmpfs" && argv[i+1] == target {
				n++
			}
		}
		return n
	}
	ob := e.opts()
	ob.AuditRoot = ob.BaseReal
	argv, err = BuildBwrapArgs(p, in, ob)
	if err != nil {
		t.Fatal(err)
	}
	if got := countTmpfs(argv, ob.BaseReal); got != 1 {
		t.Fatalf("审计根等于 Base 时只应有 ④.6 的一次 tmpfs，得到 %d 次：%v", got, argv)
	}
	oh := e.opts()
	oh.RealHome = filepath.Join(t.TempDir(), "home")
	oh.Home = oh.RealHome
	oh.AuditRoot = filepath.Dir(oh.RealHome)
	argv, err = BuildBwrapArgs(p, in, oh)
	if err != nil {
		t.Fatal(err)
	}
	if got := countTmpfs(argv, oh.AuditRoot); got != 0 {
		t.Fatalf("审计根包含 HOME 时不得再挂 tmpfs，得到 %d 次：%v", got, argv)
	}
}

// prepareKeyringTargets 在后续目标失败时必须把本次已建目录交回调用方回滚，
// 否则失败后会在宿主留下空目录链。
func TestPrepareKeyringTargetsReturnsCreatedOnError(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	targets := e.BuildOptions().keyringTargets()
	if len(targets) != 2 {
		t.Fatalf("测试环境应有两个目标，得到 %v", targets)
	}
	for _, d := range []string{filepath.Dir(targets[0]), filepath.Dir(targets[1])} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(targets[1], nil, 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := e.prepareKeyringTargets(in)
	if errKey(err) != "err.profile.keyring_target" {
		t.Fatalf("第二个目标是文件时应报 keyring_target，得到 %v", err)
	}
	if !slices.Contains(created, targets[0]) {
		t.Fatalf("失败路径必须返回本次新建清单（供回滚）：%v", created)
	}
}

// 双侧软链指向尚不存在的 vault：prepare 要建出真实目录（0700）并计入回滚，
// 否则 add 直接失败、用户没有任何出路。
func TestPrepareKeyringTargetsDanglingDoubleSymlink(t *testing.T) {
	e := testEnv(t)
	in := newTestInstance(t)
	raw := e.BuildOptions().keyringTargets()
	vault := filepath.Join(t.TempDir(), "vault", "keyrings")
	for _, tgt := range raw {
		if err := os.MkdirAll(filepath.Dir(tgt), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(tgt); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(vault, tgt); err != nil {
			t.Fatal(err)
		}
	}
	created, err := e.prepareKeyringTargets(in)
	if err != nil {
		t.Fatalf("悬空双侧软链应自动建出真实目录：%v", err)
	}
	fi, serr := os.Stat(vault)
	if serr != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("vault 应为 0700 目录：%v %v", fi, serr)
	}
	if !slices.Contains(created, vault) {
		t.Fatalf("新建的 vault 必须计入回滚清单：%v", created)
	}
}
