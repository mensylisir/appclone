package core

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// isolate_keyring 的三态：没写（跟默认走）/ true / 显式 false。指针必须把
// "没写"与"写 false"分开，否则旧档案经一次保存就被改成"用户关掉了"。
func TestProfileIsolateKeyringRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		profile Profile
		raw     string // 期望档案里出现的片段；"" 表示不该出现
		wantVal bool
		wantSet bool
	}{
		{"未写", Profile{}, "", false, false},
		{"true", Profile{IsolateKeyring: true}, "isolate_keyring = true", true, true},
		{"显式false", Profile{IsolateKeyring: false, KeyringSet: true}, "isolate_keyring = false", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := testEnv(t)
			p := c.profile
			p.App = "testapp"
			p.Exec = "/opt/app"
			p.Paths = []string{"~/.config/App"}
			p.ProbeMethod = "overlay"
			if err := e.SaveProfile(p); err != nil {
				t.Fatalf("save: %v", err)
			}
			raw := readRawProfile(t, e, "testapp")
			if c.raw == "" {
				if strings.Contains(raw, "isolate_keyring") {
					t.Fatalf("没写时不该落盘：\n%s", raw)
				}
			} else if !strings.Contains(raw, c.raw) {
				t.Fatalf("档案缺少 %q：\n%s", c.raw, raw)
			}
			got, err := e.LoadProfile("testapp")
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got.IsolateKeyring != c.wantVal || got.KeyringSet != c.wantSet {
				t.Fatalf("往返丢状态：value=%v set=%v", got.IsolateKeyring, got.KeyringSet)
			}
		})
	}
}

// load → save 字节稳定：GUI 只改显示名也不该把档案重写成另一份形状。
func TestProfileKeyringLoadSaveByteStable(t *testing.T) {
	e := testEnv(t)
	p := Profile{
		App: "testapp", Exec: "/opt/app", Paths: []string{"~/.config/App"},
		ProbeMethod: "overlay", IsolateKeyring: true, KeyringSet: true,
	}
	if err := e.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	first := readRawProfile(t, e, "testapp")
	got, err := e.LoadProfile("testapp")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SaveProfile(got); err != nil {
		t.Fatal(err)
	}
	second := readRawProfile(t, e, "testapp")
	if first != second {
		t.Fatalf("load→save 不字节稳定：\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s", first, second)
	}
}

// 合并优先级：base 显式写过时显式值胜出；没写过时探测默认生效。
func TestMergeKeyringPriority(t *testing.T) {
	cases := []struct {
		name        string
		probe, base Profile
		want        bool
	}{
		{"档案显式关、探测默认开", Profile{IsolateKeyring: true}, Profile{IsolateKeyring: false, KeyringSet: true}, false},
		{"档案显式开、探测默认关", Profile{}, Profile{IsolateKeyring: true, KeyringSet: true}, true},
		{"档案没写、探测默认开", Profile{IsolateKeyring: true}, Profile{}, true},
		{"档案没写、探测默认关", Profile{}, Profile{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MergeNonPathFields(c.probe, c.base)
			if got.IsolateKeyring != c.want {
				t.Fatalf("合并结果 %v，期望 %v", got.IsolateKeyring, c.want)
			}
			if c.base.KeyringSet && !got.KeyringSet {
				t.Fatal("显式状态必须保留，否则下次合并又会退回默认")
			}
		})
	}
}

// DeriveProfile 从候选新建档案（零值起步）：探测默认值必须穿过
// ProfileFromCandidates 落到最终档案，否则「observed 默认开」只存在于
// ProbeResult 里，CLI/GUI 拿到的档案永远是关的（GTK 用例抓到的真实缺陷）。
func TestDeriveProfileKeepsKeyringDefault(t *testing.T) {
	e := testEnv(t)
	cfg := filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "App")
	cands := []Candidate{{Path: cfg, Class: ClassConfig, Checked: true}}
	p := e.Resolver.DeriveProfile(Profile{App: "app", IsolateKeyring: true}, nil, "overlay", ReadTraceOK, cands)
	if !p.IsolateKeyring {
		t.Fatalf("探测默认值丢失：%+v", p)
	}
	if p.KeyringSet {
		t.Fatal("默认值不是用户显式选择，不能标 KeyringSet")
	}
	// 显式关闭的旧档案重探后仍必须保持关（默认值不得覆盖用户选择）。
	old := Profile{App: "app", IsolateKeyring: false, KeyringSet: true}
	p = e.Resolver.DeriveProfile(Profile{App: "app", IsolateKeyring: true}, &old, "overlay", ReadTraceOK, cands)
	if p.IsolateKeyring || !p.KeyringSet {
		t.Fatalf("显式关闭被默认值覆盖：%+v", p)
	}
}

// unsupported 的 keyring 条目必须与 isolate_keyring 双向一致，且路径用
// resolver 的 XDG_DATA_HOME（自定义 XDG 也不能写死 ~/.local/share）。
func TestNormalizeKeyringUnsupported(t *testing.T) {
	e := testEnv(t)
	canon := filepath.Join(e.Resolver.XDG["DATA_HOME"], "keyrings")
	old := Unsupported{Kind: "keyring", Path: "~/.local/share/keyrings", Reason: "probe.why.keyring"}

	p := Profile{Unsupported: []Unsupported{old, old}}
	e.normalizeKeyring(&p)
	if len(p.Unsupported) != 1 {
		t.Fatalf("关闭时只应保留一条规范条目，得到 %+v", p.Unsupported)
	}
	if p.Unsupported[0].Path != e.Resolver.ToStoreForm(canon) {
		t.Fatalf("规范路径应为 %q，得到 %q", e.Resolver.ToStoreForm(canon), p.Unsupported[0].Path)
	}

	p = Profile{IsolateKeyring: true, Unsupported: []Unsupported{old}}
	e.normalizeKeyring(&p)
	if len(p.Unsupported) != 0 {
		t.Fatalf("开启时 keyring 条目必须移除，得到 %+v", p.Unsupported)
	}

	// 没有条目时也要补一条（"隔离不了"的说明始终可见）。
	p = Profile{}
	e.normalizeKeyring(&p)
	if len(p.Unsupported) != 1 || p.Unsupported[0].Kind != "keyring" {
		t.Fatalf("关闭时必须补规范条目，得到 %+v", p.Unsupported)
	}
}

// keyring 目录不能进 paths：否则两个分身共享同一份密钥。
func TestValidateProfileRejectsKeyringPath(t *testing.T) {
	e := testEnv(t)
	o := e.BuildOptions()
	for _, pth := range []string{
		filepath.Join(e.Resolver.XDG["DATA_HOME"], "keyrings"),
		filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "keyrings", "sub"),
	} {
		p := Profile{App: "app", Exec: "/opt/app", Paths: []string{pth}}
		err := ValidateProfile(p, o)
		if errKey(err) != "err.profile.keyring_in_paths" {
			t.Fatalf("paths=%q 应被拒绝，得到 %v", pth, err)
		}
	}
	// `.keyring` 后缀不再单独判定：paths 支持精确文件，应用自己的
	// `~/.config/app/foo.keyring` 不是系统密钥环，不能被误杀。
	p := Profile{
		App: "app", Exec: "/opt/app",
		Paths: []string{filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "app", "custom.keyring")},
	}
	if err := ValidateProfile(p, o); err != nil {
		t.Fatalf("普通 .keyring 文件不应被拒绝，得到 %v", err)
	}
}

// fork 不克隆 keyring：即使档案被手改成包含 keyring 路径（ValidateProfile
// 会拒绝，但 forkMounts 是第二道防线），也不得对它建 overlay。
func TestForkSkipsKeyringPaths(t *testing.T) {
	e := testEnv(t)
	keyrings := filepath.Join(e.Resolver.XDG["DATA_HOME"], "keyrings")
	if err := os.MkdirAll(keyrings, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyrings, "login.keyring"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fm := &recordingMounter{}
	e.Mounter = fm
	p := Profile{Paths: []string{keyrings}}
	in := Instance{App: "app", ID: "work", Root: filepath.Join(t.TempDir(), "root"), Fork: true}
	if err := os.MkdirAll(in.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	sources, cleanup := e.forkMounts(p, in, nil, func(string) {})
	defer cleanup()
	if len(fm.calls) != 0 {
		t.Fatalf("keyring 路径不该建 overlay：%+v", fm.calls)
	}
	if len(sources) != 0 {
		t.Fatalf("keyring 路径不该产生 bind 源：%v", sources)
	}
}

// 证据三来源：候选类别、读路径、sendmsg 字面量。普通应用必须是空证据。
func TestKeyringEvidence(t *testing.T) {
	e := testEnv(t)
	script := filepath.Join(t.TempDir(), "app.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	keyrings := filepath.Join(e.Resolver.XDG["DATA_HOME"], "keyrings")
	cases := []struct {
		name  string
		cands []Candidate
		reads []string
		hits  []string
		want  string
	}{
		{"普通应用", nil, nil, nil, KeyringEvidenceNone},
		{"候选类别", []Candidate{{Class: ClassKeyring, Path: keyrings}}, nil, nil, KeyringEvidenceObserved},
		{"读路径", nil, []string{filepath.Join(keyrings, "login.keyring")}, nil, KeyringEvidenceObserved},
		{"sendmsg 字面量", nil, nil, []string{"org.freedesktop.secrets"}, KeyringEvidenceObserved},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := e.keyringEvidence(script, c.cands, c.reads, c.hits); got != c.want {
				t.Fatalf("证据 %q，期望 %q", got, c.want)
			}
		})
	}
}

// 能力解析只看固定候选目录，不读用户 PATH；/usr/libexec 必须在候选里。
func TestKeyringCapabilityLookup(t *testing.T) {
	if !slices.Contains(keyringPathDirs, "/usr/libexec") {
		t.Fatal("候选目录必须包含 /usr/libexec")
	}
	dir := t.TempDir()
	if got := lookPathIn([]string{dir}, "gnome-keyring-daemon"); got != "" {
		t.Fatalf("空目录不该找到可执行文件，得到 %q", got)
	}
	bin := filepath.Join(dir, "gnome-keyring-daemon")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := lookPathIn([]string{dir}, "gnome-keyring-daemon"); got != bin {
		t.Fatalf("应解析到 %q，得到 %q", bin, got)
	}
	if err := os.Chmod(bin, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lookPathIn([]string{dir}, "gnome-keyring-daemon"); got != "" {
		t.Fatalf("不可执行的文件不该算能力可用，得到 %q", got)
	}
}

// 默认策略：observed 且能力可用才开；linked 永不默认开。
func TestKeyringDefaultPolicy(t *testing.T) {
	if on, warn := KeyringDefault(KeyringEvidenceLinked); on || warn != "" {
		t.Fatalf("linked 只提示、默认关：on=%v warn=%q", on, warn)
	}
	if on, warn := KeyringDefault(KeyringEvidenceNone); on || warn != "" {
		t.Fatalf("无证据默认关：on=%v warn=%q", on, warn)
	}
	on, warn := KeyringDefault(KeyringEvidenceObserved)
	if KeyringCapability().Available {
		if !on || warn != "" {
			t.Fatalf("能力可用时 observed 应默认开：on=%v warn=%q", on, warn)
		}
	} else {
		if on || warn != "keyring_unavailable" {
			t.Fatalf("能力缺失时应记 keyring_unavailable：on=%v warn=%q", on, warn)
		}
	}
}

// 系统 bin 根不参与 ELF 扫描；shell wrapper 也不扫同目录。
func TestElfLinksSecretSkipsSystemRoots(t *testing.T) {
	if elfLinksSecret("/usr/bin/true") {
		t.Fatal("/usr/bin 属于系统 bin 根，不该被扫描")
	}
	script := filepath.Join(t.TempDir(), "app.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /opt/app\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if elfLinksSecret(script) {
		t.Fatal("shell wrapper 不该靠同目录猜证据")
	}
}

// sendmsg 行只做字面量扫描：不追加 Reads/Writes，也不触发 size_exceeded。
func TestStraceSendmsgEvidenceOnly(t *testing.T) {
	const hit = `1234  sendmsg(3</run/user/1000/bus>, {msg_name=NULL, msg_namelen=0, msg_iov=[{iov_base="l\1\0\1\0\0\0\0\1\0\0\0\0\0\0\0org.freedesktop.secrets", iov_len=40}], msg_iovlen=1}, 0) = 40`
	res := ParseStraceAll(strings.NewReader(hit+"\n"), "/home/u")
	if len(res.KeyringHits) != 1 || res.KeyringHits[0] != keyringServiceLiteral {
		t.Fatalf("应命中 keyring 字面量，得到 %v", res.KeyringHits)
	}
	if len(res.Reads) != 0 || len(res.Writes) != 0 {
		t.Fatalf("sendmsg 不得产生路径证据：reads=%v writes=%v", res.Reads, res.Writes)
	}
	if res.Stats.SizeExceeded {
		t.Fatal("sendmsg 不得触发 size_exceeded")
	}
	// 大端 method call 头是 `B\1`（类型 1），不是 `B\0`。
	const hitBE = `1234  sendmsg(3</run/user/1000/bus>, {msg_name=NULL, msg_namelen=0, msg_iov=[{iov_base="B\1\0\1\0\0\0\0\1org.freedesktop.secrets", iov_len=40}], msg_iovlen=1}, 0) = 40`
	resBE := ParseStraceAll(strings.NewReader(hitBE+"\n"), "/home/u")
	if len(resBE.KeyringHits) != 1 {
		t.Fatalf("大端 method call 应命中，得到 %v", resBE.KeyringHits)
	}
}

// 普通文本里出现同名字面量、非 unix socket、缺 -y 注解都不算证据。
func TestStraceSendmsgNoFalsePositive(t *testing.T) {
	lines := []string{
		`1234  sendmsg(3</run/user/1000/bus>, {msg_name=NULL, msg_namelen=0, msg_iov=[{iov_base="hello org.freedesktop.secrets", iov_len=29}], msg_iovlen=1}, 0) = 29`,
		`1234  sendmsg(4<socket:[12345]>, {msg_name=NULL, msg_namelen=0, msg_iov=[{iov_base="l\1\0\1\0\0\0\0\1org.freedesktop.secrets", iov_len=40}], msg_iovlen=1}, 0) = 40`,
		`1234  sendmsg(5, {msg_name=NULL, msg_namelen=0, msg_iov=[{iov_base="l\1\0\1\0\0\0\0\1org.freedesktop.secrets", iov_len=40}], msg_iovlen=1}, 0) = 40`,
		// 普通文件 fd 上的 sendmsg 必然失败（ENOTSOCK）：返回值 <=0 不算证据。
		`1234  sendmsg(3</home/u/plain.txt>, {msg_name=NULL, msg_namelen=0, msg_iov=[{iov_base="l\1\0\1\0\0\0\0\1org.freedesktop.secrets", iov_len=40}], msg_iovlen=1}, 0) = -1 ENOTSOCK (Socket operation on non-socket)`,
	}
	for _, line := range lines {
		res := ParseStraceAll(strings.NewReader(line+"\n"), "/home/u")
		if len(res.KeyringHits) != 0 {
			t.Fatalf("不应命中：%s\n得到 %v", line, res.KeyringHits)
		}
		if res.KeyringTraceDropped != 0 {
			t.Fatalf("缺注解的行不算丢弃：%d", res.KeyringTraceDropped)
		}
	}
}

// 扫描上限：超出的 sendmsg 行计入 KeyringTraceDropped，但不判 size_exceeded。
func TestStraceSendmsgScanLimit(t *testing.T) {
	line := `1234  sendmsg(3</run/user/1000/bus>, {msg_name=NULL, msg_namelen=0, msg_iov=[{iov_base="x", iov_len=1}], msg_iovlen=1}, 0) = 1`
	var b strings.Builder
	for i := 0; i < keyringTraceLimit+3; i++ {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	res := ParseStraceAll(strings.NewReader(b.String()), "/home/u")
	if res.KeyringTraceDropped != 3 {
		t.Fatalf("应丢弃 3 行，得到 %d", res.KeyringTraceDropped)
	}
	if res.Stats.SizeExceeded {
		t.Fatal("sendmsg 上限不得判成 size_exceeded")
	}
}

// 路径条目已满时 sendmsg 扫描必须继续，且计数不能被每行的临时 sink 重置。
func TestTraceReaderKeyringWhenPathEntriesFull(t *testing.T) {
	r := &traceReader{
		home: "/home/u", readSeen: map[string]bool{}, writeSeen: map[string]bool{},
		entries: traceReadLimit,
	}
	r.res.keyringSeen = map[string]bool{}
	hit := `1234  sendmsg(3</run/user/1000/bus>, {msg_name=NULL, msg_namelen=0, msg_iov=[{iov_base="l\1\0\1\0\0\0\0\1org.freedesktop.secrets", iov_len=40}], msg_iovlen=1}, 0) = 40`
	r.consume(hit)
	r.consume(hit) // 去重：同一字面量只记一次
	if len(r.res.KeyringHits) != 1 {
		t.Fatalf("路径满时仍应采集 keyring 证据，得到 %v", r.res.KeyringHits)
	}
	if r.sizeHit {
		t.Fatal("sendmsg 行不得置 sizeHit")
	}
	r.consume(`1234  openat(AT_FDCWD, "/home/u/.config/App/x", O_RDONLY) = 3`)
	if !r.sizeHit {
		t.Fatal("路径条目已满时的路径行必须置 sizeHit")
	}
	if len(r.res.KeyringHits) != 1 {
		t.Fatalf("路径行不应清掉 keyring 证据：%v", r.res.KeyringHits)
	}
	if len(r.res.Reads) != 0 {
		t.Fatalf("已满后路径结果应丢弃，得到 %v", r.res.Reads)
	}
}

// 读列表与兜底列表都必须带 sendmsg；probeCapsReadList 的 golden 不受影响。
func TestStraceCommandIncludesSendmsg(t *testing.T) {
	e := testEnv(t)
	st := &probeReadState{
		e: e, readOK: true, reader: &traceReader{fifo: filepath.Join(t.TempDir(), "reads.fifo")},
		caps: ProbeCaps{Read: true, Y: true, Openat2: true},
	}
	for _, fallback := range []bool{false, true} {
		argv := st.command([]string{"true"}, fallback)
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "sendmsg") {
			t.Fatalf("fallback=%v 的命令缺少 sendmsg：%s", fallback, joined)
		}
	}
	if slices.Contains(probeCapsReadList(), "sendmsg") {
		t.Fatal("sendmsg 不得混进 probeCapsReadList（golden 与读能力预检依赖原列表）")
	}
}
