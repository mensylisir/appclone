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
// 携带凭据的旗标。名称归一化后按**包含**匹配（--user-data-dir=… → userdatadir，
// --x-user-data-dir → xuserdatadir，--disable-sandbox → disablesandbox 都命中），
// 避免攻击者在危险词前加前缀绕过；自动预填宁可少写。
func dangerousSelfRelaunchFlag(arg string) bool {
	name := normalizeFlagName(arg)
	if name == "" {
		// `--` / `-` 没有旗标名，无法判断语义，不能带进档案。
		return true
	}
	for _, bad := range []string{
		// 凭据
		"token", "accesstoken", "refreshtoken", "authtoken", "sessiontoken",
		"passwd", "pwd", "secret", "clientsecret", "credential", "cookie",
		"session", "apikey", "privatekey", "cert", "pass", "auth",
		"keyfile", "keystore", "signingkey", "encryptionkey", "accesskey",
		// 数据目录 / 状态 / 配置边界
		"userdatadir", "profiledirectory", "profile", "diskcachedir", "cachedir",
		"cache", "datadir", "configdir", "config", "homedir", "home",
		"userdir", "userdata", "statedir", "state", "runtimedir", "runtime",
		"tempdir", "tmpdir", "temp", "tmp", "workdir", "workingdir", "cwd",
		"basedir", "storedir", "root", "prefix", "path",
		// 沙箱 / 调试 / 网络 / 日志边界
		"nosandbox", "sandbox", "disablesetuidsandbox", "disableseccomp",
		"seccomp", "singleprocess", "disablegpusandbox",
		"remotedebug", "inspect", "inspectbrk", "inspectport",
		"proxyserver", "proxy", "hostresolverrules",
		"logfile", "log", "sslkeylogfile", "enablelogging", "debug",
		"jsflags", "disablewebsecurity", "ignorecertificateerrors",
		"socket", "dbus",
	} {
		if strings.Contains(name, bad) {
			return true
		}
	}
	return false
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
