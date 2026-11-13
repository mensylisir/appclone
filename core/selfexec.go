package core

import (
	"slices"
	"strings"
)

// SelfRelaunch 是“应用自己重启并追加参数”的识别结果。
type SelfRelaunch struct {
	Args   []string // initialArgs + filteredDelta；OK 时有效
	Delta  []string
	Rounds int
	OK     bool
	// Known 表示接受的 delta 至少有一条 RetKnown=true 的调用。全为
	// `= ?` / `<unfinished>` 证据时 OK 仍可能为 true，但只能作 run-scoped
	// 诊断（ProbeResult.SelfRelaunchUnknown），不改变落盘行为。
	Known bool
	// Ambiguous 表示“有候选 delta，但多解或轮次不足”。没有候选时保持 false。
	Ambiguous bool
}

// DetectSelfRelaunchArgs 从 execve/execveat 证据识别自重启追加参数。
//
// 模型（docs/discover.md §3.5.3 / ADR-036）：
//   - 同路径第一条调用是基线（Args[1:]；Args 含 argv[0]）；
//   - 后续调用必须严格以基线为前缀且更长，多出的部分才是 delta；
//   - delta 与 initialArgs 做多重集差，只接受旗标，过滤子进程选择器与
//     危险/凭据旗标；
//   - 按 delta 统计“不同轮次”，同一轮重复只算一次；生产两轮要求 minRounds=2。
func DetectSelfRelaunchArgs(execPath string, initialArgs []string, calls []ExecCall, minRounds int) SelfRelaunch {
	if execPath == "" {
		return SelfRelaunch{}
	}
	if minRounds < 1 {
		minRounds = 1
	}

	var same []ExecCall
	for _, c := range calls {
		if sameExecPath(c.Path, execPath) {
			same = append(same, c)
		}
	}
	if len(same) < 2 {
		return SelfRelaunch{}
	}

	baseIdx := -1
	var baseline []string
	for i, c := range same {
		if len(c.Args) == 0 {
			// argv 缺失的调用不能当基线（reader 通常已跳过，这里再兜一层）。
			continue
		}
		baseIdx = i
		baseline = append([]string(nil), c.Args[1:]...)
		break
	}
	if baseIdx < 0 {
		return SelfRelaunch{}
	}

	type deltaInfo struct {
		rounds map[int]int // round -> 出现次数（用于“同轮重复只算一次”）
		known  bool
		delta  []string
	}
	byDelta := map[string]*deltaInfo{}
	var order []string
	for _, c := range same[baseIdx+1:] {
		if len(c.Args) == 0 {
			continue
		}
		args := c.Args[1:]
		if len(args) <= len(baseline) {
			continue
		}
		if !slices.Equal(args[:len(baseline)], baseline) {
			continue
		}
		rawDelta := append([]string(nil), args[len(baseline):]...)
		delta, ok := filterSelfRelaunchDelta(rawDelta, initialArgs)
		if !ok {
			continue
		}
		key := strings.Join(delta, "\x00")
		info := byDelta[key]
		if info == nil {
			info = &deltaInfo{rounds: map[int]int{}, delta: delta}
			byDelta[key] = info
			order = append(order, key)
		}
		info.rounds[c.Round]++
		if c.RetKnown {
			info.known = true
		}
	}
	if len(order) == 0 {
		return SelfRelaunch{}
	}

	anyKnown := false
	for _, k := range order {
		if byDelta[k].known {
			anyKnown = true
			break
		}
	}
	var candidates []*deltaInfo
	for _, k := range order {
		info := byDelta[k]
		// 有 known delta 时，不同 delta 的 unknown 调用只是噪声（典型是
		// namespace teardown 的竞争行）；同 delta 的 unknown 已在上面合并。
		if anyKnown && !info.known {
			continue
		}
		candidates = append(candidates, info)
	}
	if len(candidates) == 0 {
		return SelfRelaunch{}
	}
	if len(candidates) > 1 {
		return SelfRelaunch{Ambiguous: true}
	}

	info := candidates[0]
	rounds := len(info.rounds)
	if rounds < minRounds {
		return SelfRelaunch{
			Delta:     append([]string(nil), info.delta...),
			Rounds:    rounds,
			Ambiguous: true,
		}
	}
	args := append(append([]string(nil), initialArgs...), info.delta...)
	return SelfRelaunch{
		Args:   args,
		Delta:  append([]string(nil), info.delta...),
		Rounds: rounds,
		OK:     true,
		Known:  info.known,
	}
}

// filterSelfRelaunchDelta 返回可落进档案的追加参数。任何一项不可信就整条
// 调用拒绝：自动预填宁可少写，也不能把子进程/凭据/隔离边界参数带进去。
func filterSelfRelaunchDelta(rawDelta, initialArgs []string) ([]string, bool) {
	remaining := map[string]int{}
	for _, a := range initialArgs {
		remaining[a]++
	}
	out := make([]string, 0, len(rawDelta))
	for _, a := range rawDelta {
		if remaining[a] > 0 {
			remaining[a]--
			continue // initialArgs 里已有的参数不重复追加
		}
		if !strings.HasPrefix(a, "--") || !validExecArg(a) {
			return nil, false // 位置参数/裸值/短旗标/非法字符串
		}
		if childProcessSelector(a) || dangerousSelfRelaunchFlag(a) {
			return nil, false
		}
		out = append(out, a)
	}
	if len(out) == 0 || len(out) > 16 {
		// 上限 16：正常自重启只追加一两个旗标；异常长的增量宁可不预填。
		return nil, false
	}
	return out, true
}

// childProcessSelector 识别 Chromium/Electron 给子进程用的参数。这些参数
// 出现说明“同路径 exec”是渲染/工具进程，不是应用自重启。
func childProcessSelector(arg string) bool {
	for _, p := range []string{
		"--type=", "--utility-sub-type=", "--service-sandbox-type=",
		"--renderer-client-id=", "--shared-files", "--field-trial-handle",
		"--crashpad-handler", "--mojo-platform-channel-handle",
	} {
		if strings.HasPrefix(arg, p) {
			return true
		}
	}
	return arg == "--no-zygote" || arg == "--zygote"
}

// dangerousSelfRelaunchFlag 拒绝会改写数据目录/沙箱/调试/代理/日志边界或
// 携带凭据的旗标。名称按 `-`/`_`/`.` 等分隔符切成词，再逐词与连续词组合
// 匹配：`--disable-sandbox` 命中词 sandbox，`--x-user-data-dir` 命中组合
// userdatadir，而 `--catalog`/`--rootless`/`--attempts` 这类"危险词只是
// 更长单词的一部分"不会被误拒。自动预填宁可少写，但不能把常见无害旗标
// 一律拒掉——那会让分身起不来。
func dangerousSelfRelaunchFlag(arg string) bool {
	name := normalizeFlagName(arg)
	if name == "" {
		// `--` / `-` 没有旗标名，无法判断语义，不能带进档案。
		return true
	}
	// 归一化全名兜底：对抗性大小写（--dIsAbLeWeBsEcUrItY）会被 camelCase 切出
	// 很多段，超出 4 段组合窗口；对全名再做一次长词匹配把它关掉。
	for _, cand := range append(flagWordJoins(flagNameWords(arg)), name) {
		for _, bad := range dangerousFlagTokens {
			if cand == bad {
				return true
			}
			// 长词（≥5）允许前后缀：--mytoken / --app-config / --x-user-data-dir
			// 这类加前缀的写法不能绕过；短词只认整词，避免 --catalog 命中 log。
			if len(bad) >= 5 && (strings.HasPrefix(cand, bad) || strings.HasSuffix(cand, bad)) {
				return true
			}
			// 短凭据词（pass/pwd/auth/cert）允许出现在单词任意位置：
			// --dbpassword / --userpass / --clientcert / --basicauth 这类粘连写法
			// 必须拒绝；少数普通单词（bypass/compass/author 等）显式放行。
			if shortCredentialTokens[bad] && len(cand) > len(bad) &&
				strings.Contains(cand, bad) && !shortTokenExceptions[cand] {
				return true
			}
		}
	}
	return false
}

// flagNameWords 把旗标名切成小写单词（去掉前导 `-` 与 `=` 之后的值）。
// 除分隔符外还按 camelCase 边界切：`--logDirectory` → log/directory，
// 驼峰拼写（如 db + Password）否则会成为绕过路径。
func flagNameWords(arg string) []string {
	s := strings.TrimSpace(arg)
	s = strings.TrimLeft(s, "-")
	if i := strings.IndexByte(s, '='); i >= 0 {
		s = s[:i]
	}
	var words []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			words = append(words, b.String())
			b.Reset()
		}
	}
	var prev rune
	for _, r := range s {
		lower := r
		if r >= 'A' && r <= 'Z' {
			lower = r + ('a' - 'A')
		}
		if r >= 'A' && r <= 'Z' && prev >= 'a' && prev <= 'z' {
			flush() // camelCase 边界
		}
		if (lower >= 'a' && lower <= 'z') || (lower >= '0' && lower <= '9') {
			b.WriteRune(lower)
		} else {
			flush()
		}
		prev = r
	}
	flush()
	return words
}

// flagWordJoins 返回每个词及最多 4 个连续词的拼接，覆盖 user-data-dir →
// userdatadir 这类多段旗标；上限 4 段足够，避免病态长旗标产生平方级组合。
func flagWordJoins(words []string) []string {
	out := make([]string, 0, len(words)*2)
	for i := range words {
		var b strings.Builder
		for j := i; j < len(words) && j < i+4; j++ {
			b.WriteString(words[j])
			out = append(out, b.String())
		}
	}
	return out
}

// shortCredentialTokens 是允许"单词内任意位置"匹配的短凭据词。它们太短，
// 无法只靠整词判断：--dbpassword / --userpass / --clientcert / --basicauth
// 都必须拒绝。匹配到这些词的普通英文单词在 shortTokenExceptions 里显式放行。
var shortCredentialTokens = map[string]bool{
	"pass": true, "pwd": true, "auth": true, "cert": true,
}

// shortTokenExceptions 是包含短凭据词、但语义无关的常见单词。宁可枚举，
// 也不要让 --bypass 这类正常旗标被误拒（误拒会让分身起不来）。
var shortTokenExceptions = map[string]bool{
	"bypass": true, "compass": true, "surpass": true, "trespass": true,
	"passage": true, "passenger": true,
	"author": true, "authority": true, "authors": true,
	"concert": true, "certain": true, "ascertain": true,
}

// dangerousFlagTokens 是归一化后的危险词表。短词（<5）只做整词匹配，
// 长词额外允许前后缀匹配。
var dangerousFlagTokens = []string{
	// 凭据
	"token", "accesstoken", "refreshtoken", "authtoken", "sessiontoken",
	"passwd", "passphrase", "passcode", "pwd", "pass", "secret",
	"clientsecret", "credential", "cookie", "session", "apikey", "privatekey",
	"cert", "certificate", "auth", "oauth", "keyfile", "keystore", "signingkey",
	"encryptionkey", "accesskey", "sshpass",
	// 数据目录 / 状态 / 配置边界
	"userdatadir", "profiledirectory", "profile", "diskcachedir", "cachedir",
	"cache", "datadir", "datapath", "configdir", "config", "homedir", "home",
	"homepath", "userdir", "userdata", "userpath", "statedir", "state",
	"runtimedir", "runtime", "rundir", "tempdir", "tmpdir", "temppath", "tmppath",
	"temp", "tmp", "workdir", "workingdir", "workpath", "cwd",
	"basedir", "basepath", "storedir", "storepath", "rootdir", "rootpath",
	"root", "chroot", "prefix", "path", "apppath", "keypath",
	// 沙箱 / 调试 / 网络 / 日志边界
	"nosandbox", "sandbox", "disablesetuidsandbox", "disableseccomp",
	"seccomp", "singleprocess", "disablegpusandbox",
	"remotedebug", "inspect", "inspectbrk", "inspectport",
	"proxyserver", "proxy", "hostresolverrules",
	"logfile", "log", "logdir", "logpath", "sslkeylogfile", "enablelogging", "debug",
	"jsflags", "disablewebsecurity", "ignorecertificateerrors",
	"socket", "dbus", "dbuspath", "unsandboxed", "rootfs",
}

func normalizeFlagName(arg string) string {
	s := strings.ToLower(strings.TrimSpace(arg))
	s = strings.TrimLeft(s, "-")
	if i := strings.IndexByte(s, '='); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}
