package core

import (
	"bufio"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// strace 解析（docs/discover.md §3.4、docs/tech-stack.md ADR-034）。
//
// 能力边界先说清：strace 能看到"应用打开了什么"，看不到"写进了哪个层"，
// 所以它给不出 overlay 那样零噪声的写入集——读路径必须靠 flags 过滤，
// 且 openat2 的 flags 在旧 strace 上可能不可见。代价是漏判（`O_PATH`、
// mmap 写、io_uring 绕过）和误判（打开后只读不写也算"打开过"），
// 档案里记 `probe_method = "strace"`，界面按"低置信"展示（§6.1）。

// straceReadSyscalls 是读证据关注的调用。
var straceReadSyscalls = map[string]bool{
	"open": true, "openat": true, "openat2": true,
	"execve": true, "execveat": true,
}

// straceWriteSyscalls 是写证据关注的调用。`%file` 会把 stat 之类也带上，
// 那对判定"写入集"没有意义，还会让日志大一个数量级。
var straceWriteSyscalls = map[string]bool{
	"open": true, "openat": true, "openat2": true, "creat": true,
	"mkdir": true, "mkdirat": true,
	"rename": true, "renameat": true, "renameat2": true,
	"link": true, "linkat": true, "symlink": true, "symlinkat": true,
	"unlink": true, "unlinkat": true,
	"truncate": true, "chmod": true, "fchmodat": true, "utimensat": true,
}

// StraceStats 是解析期的质量计数。SizeExceeded 由采集器设置，不在这里。
type StraceStats struct {
	ReadsDropped       int
	ParseErrors        int
	NoFDAnnotation     bool
	Openat2FlagsHidden bool
	SizeExceeded       bool
}

// StraceResult 是一次解析的全部证据。
type StraceResult struct {
	Reads  []string     // 相对 $HOME 的读路径（已去重、已剥离 (deleted)）
	Writes []UpperEntry // 与 overlay upper 同形
	// ExecSeen/Execs 是活性证据（不是读路径）：execve/execveat 成功即置位。
	// T5 用它区分"应用根本没 exec"与"exec 了但零读写"（tracee_unverified）。
	ExecSeen bool
	Execs    []string
	// FlagsHidden 是 openat2 flags 不可见、按读计入但**不参与凭据升级**的路径。
	FlagsHidden []string
	Stats       StraceStats
	// KeyringHits 是 sendmsg 行里命中的 D-Bus 服务字面量（去重、按首次出现
	// 顺序）。它只是**证据**，绝不进入 Reads/Writes，也不参与路径解析。
	KeyringHits []string
	// KeyringTraceDropped 是超过扫描上限后被丢弃的 sendmsg 行数（计数，
	// 不是布尔标记）。它不触发 size_exceeded：sendmsg 不产生路径条目。
	KeyringTraceDropped int

	// keyringSeen/keyringScanned 是扫描器的内部状态：去重键与已扫描行数。
	// 不导出，避免调用方把它们当成结果的一部分。
	keyringSeen    map[string]bool
	keyringScanned int
}

// ParseStraceAll 同时解析读与写证据。读/写两条路径共用同一份行语法，
// 避免"读解析支持 -y、写解析不支持"这种半修状态。
func ParseStraceAll(r io.Reader, home string) StraceResult {
	home = filepath.Clean(home)
	res := StraceResult{keyringSeen: map[string]bool{}}
	readSeen := map[string]bool{}
	writeSeen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.Contains(line, "<unfinished") || strings.Contains(line, " resumed>") {
			// 多线程下 strace 会把一次调用拆成两行，拼起来太脆；计数并跳过，
			// 最终由 parse_error 告警把"证据不完整"暴露给用户。
			res.Stats.ParseErrors++
			continue
		}
		parseStraceLineInto(line, home, &res, readSeen, writeSeen)
	}
	if err := sc.Err(); err != nil {
		// 单行超过 1MiB 会让 Scanner 停下；结果被截断，必须让 read_trace
		// 降级而不是静默 ok。
		res.Stats.ParseErrors++
	}
	sort.Strings(res.Reads)
	return res
}

// parseStraceLineInto 是流式采集与整文件解析共用的单行入口。
func parseStraceLineInto(line, home string, res *StraceResult, readSeen, writeSeen map[string]bool) {
	c, ok := parseStraceLine(line)
	if !ok {
		return
	}
	if c.parseErr {
		res.Stats.ParseErrors++
		return
	}
	if straceReadSyscalls[c.call] {
		parseReadCall(c, home, res, readSeen)
	}
	if straceWriteSyscalls[c.call] {
		parseWriteCall(c, home, res, writeSeen)
	}
	if c.call == "sendmsg" {
		parseKeyringCall(c, res)
	}
}

// keyringServiceLiteral 是 keyring 强证据扫描的目标字面量。libsecret/GDBus
// 请求 org.freedesktop.secrets 时，服务名以明文出现在 sendmsg 的 iov 里
// （strace -s 4096 可见）。
const keyringServiceLiteral = "org.freedesktop.secrets"

// keyringTraceLimit 是 sendmsg 行的扫描上限。超过后停止扫描并累加
// KeyringTraceDropped——它**不**触发 size_exceeded：路径条目上限与证据
// 扫描上限是两套口径（方案 §6）。
const keyringTraceLimit = 20000

// parseKeyringCall 只做 sendmsg 字面量扫描，**硬约束**：绝不追加 Reads/
// Writes、绝不进入路径解析。未知系统调用本来就被忽略，所以兜底会话里多出
// 的 sendmsg 行不会污染写入证据。
//
// 过滤条件（方案 §6 / D4）：
//   - 必须有 -y 注解且是 unix socket 路径：注解缺失时无法确认 fd 是 socket，
//     跳过该行（caps.Y=false 时整轮都没有注解，自然零命中、零丢弃）；
//   - 载荷必须以 D-Bus 消息头开头：普通文本里恰好出现同名字面量不算证据；
//   - 非 unix socket（TCP/匿名 socket，注解形如 socket:[123]）不扫描。
func parseKeyringCall(c straceCall, res *StraceResult) {
	// 只认**成功**的 sendmsg：普通文件 fd 上的 sendmsg 必然失败
	// （ENOTSOCK），返回值 <=0 的行不是总线流量，不能升级为证据。
	if c.ret <= 0 {
		return
	}
	ann := sendmsgFDAnnotation(c.args)
	if ann == "" || !strings.HasPrefix(ann, "/") {
		return
	}
	if res.keyringScanned >= keyringTraceLimit {
		res.KeyringTraceDropped++
		return
	}
	res.keyringScanned++
	if !strings.Contains(c.args, keyringServiceLiteral) || !dbusMessagePayload(c.args) {
		return
	}
	if res.keyringSeen == nil {
		res.keyringSeen = map[string]bool{}
	}
	if res.keyringSeen[keyringServiceLiteral] {
		return
	}
	res.keyringSeen[keyringServiceLiteral] = true
	res.KeyringHits = append(res.KeyringHits, keyringServiceLiteral)
}

// dbusMessagePayload 判断 iov 载荷是否以 D-Bus 消息头开头。头四个字节是
// 字节序（'l'/'B'）、消息类型（1=method call）、flags、协议版本 1。
// strace 把不可打印字节写成短八进制转义（`\1`），所以按字面量比对；
// 大端同样是 method call 类型 1（`B\1`），不是 `B\0`。
func dbusMessagePayload(args string) bool {
	return strings.Contains(args, `iov_base="l\1`) ||
		strings.Contains(args, `iov_base="B\1`)
}

// sendmsgFDAnnotation 取第一个参数上的 -y 注解（`3</run/user/1000/bus>` →
// `/run/user/1000/bus`）。注解在**参数**里，不在返回值尾部：parseStraceLine
// 的 ann 只解析 `= ret<...>` 那一段，sendmsg 的 fd 注解必须单独取。
func sendmsgFDAnnotation(args string) string {
	first := args
	if i := strings.IndexByte(args, ','); i >= 0 {
		first = args[:i]
	}
	open := strings.IndexByte(first, '<')
	closeIdx := strings.LastIndexByte(first, '>')
	if open < 0 || closeIdx <= open {
		return ""
	}
	return first[open+1 : closeIdx]
}

// ParseStrace 保留原有只取写入的签名（降级路径与旧测试仍可用）。
func ParseStrace(r io.Reader, home string) []UpperEntry {
	return ParseStraceAll(r, home).Writes
}

type straceCall struct {
	call     string
	args     string
	ret      int64
	ann      string
	parseErr bool
}

// parseStraceLine 解析 `[pid ]call(args) = ret[<ann>][ errno]`。
//
// 行首可能是 `1234 `（-f）、`[pid 1234] `（-f 另一种形态）或时间戳；
// 返回值后的 `<...>` 是 -y 的 fd 注解，路径本身可能含 `<`/`>`/`) = 3`，
// 所以必须从 `call(` 起做引号感知的括号配对，不能从行尾反推。
func parseStraceLine(line string) (straceCall, bool) {
	s := line
	// 行首 pid 前缀（纯数字 + 空格）或 [pid N]。
	if strings.HasPrefix(s, "[pid ") {
		if i := strings.Index(s, "] "); i > 0 {
			s = s[i+2:]
		}
	} else {
		if i := strings.IndexByte(s, ' '); i > 0 && allDigits(s[:i]) {
			s = s[i+1:]
		}
	}
	// 可选时间戳（-t/-tt）：纯数字（可带小数点）+ 空格。
	if i := strings.IndexByte(s, ' '); i > 0 && isTimestamp(s[:i]) {
		s = s[i+1:]
	}
	s = strings.TrimLeft(s, " ")
	open := strings.IndexByte(s, '(')
	if open <= 0 {
		return straceCall{}, false
	}
	call := s[:open]
	if !validCallName(call) {
		return straceCall{}, false
	}
	closeIdx := matchParen(s, open)
	if closeIdx < 0 {
		return straceCall{call: call, parseErr: true}, true
	}
	args := s[open+1 : closeIdx]
	rest := strings.TrimSpace(s[closeIdx+1:])
	if !strings.HasPrefix(rest, "= ") {
		return straceCall{call: call, parseErr: true}, true
	}
	rest = strings.TrimSpace(rest[2:])
	// 返回值的可解析部分是开头的整数；后面可能紧跟 <ann>，也可能有 errno 尾词。
	end := 0
	if end < len(rest) && rest[end] == '-' {
		end++
	}
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 || (end == 1 && rest[0] == '-') {
		return straceCall{call: call, parseErr: true}, true
	}
	ret, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		return straceCall{call: call, parseErr: true}, true
	}
	ann := ""
	if tail := strings.TrimSpace(rest[end:]); strings.HasPrefix(tail, "<") {
		if i := strings.LastIndexByte(tail, '>'); i > 0 {
			ann = tail[1:i]
		}
	}
	return straceCall{call: call, args: args, ret: ret, ann: ann}, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isTimestamp(s string) bool {
	dot, colon := false, false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] >= '0' && s[i] <= '9':
		case s[i] == '.' && !dot:
			dot = true
		case s[i] == ':' && !colon:
			// -t/-tt 的 HH:MM:SS[.micro]
			colon = true
		default:
			return false
		}
	}
	return true
}

func validCallName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// matchParen 返回与 open 处 '(' 配对的 ')' 下标，跳过引号与转义。
func matchParen(s string, open int) int {
	depth := 0
	inQuote := false
	for i := open; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitArgs 按顶层逗号切分参数，跳过引号、花括号与嵌套括号。
func splitArgs(s string) []string {
	var out []string
	start, depth := 0, 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case '{', '(', '[':
			depth++
		case '}', ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

// quoted 返回参数里的字符串字面量（反转义后）。
func quoted(arg string) (string, bool) {
	arg = strings.TrimSpace(arg)
	if len(arg) < 2 || arg[0] != '"' {
		return "", false
	}
	end := -1
	for i := 1; i < len(arg); i++ {
		if arg[i] == '\\' {
			i++
			continue
		}
		if arg[i] == '"' {
			end = i
			break
		}
	}
	if end < 0 {
		return "", false
	}
	return unescapeStrace(arg[1:end]), true
}

// argAnnotation 取参数末尾的 `</path>` 注解（-y 的 dirfd 形态）。
// 路径本身可能含 `<`/`>`，所以从第一个 `<` 取到最后一个 `>`。
func argAnnotation(arg string) string {
	i := strings.IndexByte(arg, '<')
	if i < 0 {
		return ""
	}
	j := strings.LastIndexByte(arg, '>')
	if j <= i {
		return ""
	}
	return arg[i+1 : j]
}

// flagsOf 取 open/openat/openat2 的 flags 文本。
func flagsOf(call, args string) (string, bool) {
	parts := splitArgs(args)
	switch call {
	case "open":
		if len(parts) >= 2 {
			return parts[1], true
		}
	case "openat":
		if len(parts) >= 3 {
			return parts[2], true
		}
	case "openat2":
		if len(parts) >= 3 {
			// {flags=O_RDONLY|O_CLOEXEC, resolve=0}
			body := parts[2]
			if i := strings.Index(body, "flags="); i >= 0 {
				body = body[i+len("flags="):]
				if j := strings.IndexAny(body, ",}"); j >= 0 {
					body = body[:j]
				}
				return strings.TrimSpace(body), true
			}
			return "", false
		}
	}
	return "", false
}

// hasFlag 按 `|` 分词判断，禁止 strings.Contains：`O_PATH` 不能因为
// `O_PATHNAME` 之类的子串被误判。
func hasFlag(flags, name string) bool {
	for _, f := range strings.Split(flags, "|") {
		if strings.TrimSpace(f) == name {
			return true
		}
	}
	return false
}

// numericFlags 判定 flags 是否是纯数值（旧 strace 无法解码）。
// 十六进制必须一起认：`0x241` 是 O_WRONLY|O_CREAT|O_TRUNC，只认十进制会把它
// 当成"看不见写入意图"的读打开，既伪造读证据又漏掉真实写入。
func numericFlags(flags string) bool {
	flags = strings.TrimSpace(flags)
	if flags == "" {
		return false
	}
	if len(flags) > 2 && (strings.HasPrefix(flags, "0x") || strings.HasPrefix(flags, "0X")) {
		for i := 2; i < len(flags); i++ {
			c := flags[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(flags); i++ {
		if flags[i] < '0' || flags[i] > '9' {
			return false
		}
	}
	return true
}

// execveatEmptyPath 判定 execveat 的第 5 个参数是否含 AT_EMPTY_PATH。
// 必须只看 flags 参数：对整行做子串匹配时，路径文本里恰好出现
// "AT_EMPTY_PATH" 会被误判成空路径语义而丢掉 exec 证据。
func execveatEmptyPath(flags string) bool {
	flags = strings.TrimSpace(strings.ToLower(flags))
	if strings.Contains(flags, "at_empty_path") {
		return true
	}
	// 老 strace 可能只打印数值：AT_EMPTY_PATH == 0x1000。
	return flags == "0x1000" || flags == "4096"
}

// isReadOpen 判定 open 系列是否产生读证据：
// 排除 O_WRONLY/O_TRUNC/O_APPEND/O_PATH/O_TMPFILE；O_RDWR/O_CREAT 不排除
// （SQLite 的常态）。flags 不可见时按读计入，但由调用方记 openat2_flags_hidden。
func isReadOpen(call, flags string) bool {
	if call == "creat" {
		return false
	}
	for _, f := range []string{"O_WRONLY", "O_TRUNC", "O_APPEND", "O_PATH", "O_TMPFILE"} {
		if hasFlag(flags, f) {
			return false
		}
	}
	return true
}

// isWriteOpen 判定 open 系列是否带写入意图。
func isWriteOpen(call, flags string) bool {
	if call == "creat" {
		return true
	}
	for _, f := range []string{"O_WRONLY", "O_RDWR", "O_CREAT", "O_TRUNC", "O_APPEND", "O_TMPFILE"} {
		if hasFlag(flags, f) {
			return true
		}
	}
	return false
}

func parseReadCall(c straceCall, home string, res *StraceResult, seen map[string]bool) {
	if c.ret < 0 {
		return
	}
	var raw string
	isExec := c.call == "execve" || c.call == "execveat"
	switch c.call {
	case "open":
		parts := splitArgs(c.args)
		if len(parts) == 0 {
			res.Stats.ParseErrors++
			return
		}
		raw, _ = quoted(parts[0])
	case "openat", "openat2":
		parts := splitArgs(c.args)
		if len(parts) < 2 {
			res.Stats.ParseErrors++
			return
		}
		raw, _ = quoted(parts[1])
	case "execve":
		parts := splitArgs(c.args)
		if len(parts) == 0 {
			return
		}
		raw, _ = quoted(parts[0])
	case "execveat":
		parts := splitArgs(c.args)
		if len(parts) < 2 {
			return
		}
		res.ExecSeen = true
		flags := ""
		if len(parts) >= 4 {
			// 标准形态是 execveat(fd, path, argv, envp, flags)；老 strace 或
			// 测试夹具可能省略 envp，flags 仍是最后一个参数。
			flags = parts[len(parts)-1]
		}
		raw, _ = quoted(parts[1])
		if raw == "" && execveatEmptyPath(flags) {
			// execveat(fd, "", …, AT_EMPTY_PATH)：执行的就是 fd 指向的文件。
			// 不能用整行子串匹配 AT_EMPTY_PATH——路径本身可能含这段文本。
			base := argAnnotation(parts[0])
			if !filepath.IsAbs(base) {
				res.Stats.ReadsDropped++
				return
			}
			raw = base
		}
	}
	if raw == "" {
		return
	}
	if truncatedPath(raw) {
		res.Stats.ParseErrors++
		return
	}
	if isExec {
		// 活性证据与 HOME 读路径分开：exec 一个 HOME 外的程序仍证明应用启动了。
		res.ExecSeen = true
	}
	flagsHidden := false
	if c.call == "open" || c.call == "openat" || c.call == "openat2" {
		flags, ok := flagsOf(c.call, c.args)
		switch {
		case !ok && c.call == "openat2":
			// 旧 strace 不展开结构体：按读计入，但打上"flags 不可见"，
			// 这条路径不参与凭据升级（由候选阶段处理）。
			res.Stats.Openat2FlagsHidden = true
			res.Stats.ParseErrors++
			flagsHidden = true
		case !ok:
			res.Stats.ParseErrors++
			return
		case numericFlags(flags):
			res.Stats.ParseErrors++
			return
		case !isReadOpen(c.call, flags):
			return
		}
	}
	raw = stripDeleted(raw)
	if isExec && filepath.IsAbs(raw) {
		res.Execs = append(res.Execs, raw)
	}

	// 相对路径优先用参数侧 dirfd 注解，其次用返回值注解（fd 指向的真实路径）。
	if !filepath.IsAbs(raw) {
		if isExec && c.call != "execveat" {
			// execve 的相对路径依赖 cwd，无法可靠归属；已计入 ExecSeen。
			return
		}
		parts := splitArgs(c.args)
		if len(parts) == 0 {
			res.Stats.ReadsDropped++
			return
		}
		base := argAnnotation(parts[0])
		if base == "" || !filepath.IsAbs(base) {
			if filepath.IsAbs(c.ann) {
				// 返回值注解同样可能带 ` (deleted)`：不剥离会生成一条
				// 永远命中不了真实路径的候选/凭据。
				raw = stripDeleted(c.ann)
				goto resolved
			}
			res.Stats.ReadsDropped++
			res.Stats.NoFDAnnotation = true
			return
		}
		raw = filepath.Join(base, raw)
	}
resolved:
	if isExec && filepath.IsAbs(raw) {
		// 相对 execveat 解析后的目标也要记录（绝对路径已在上面记录过）。
		dup := false
		for _, x := range res.Execs {
			if x == raw {
				dup = true
				break
			}
		}
		if !dup {
			res.Execs = append(res.Execs, raw)
		}
	}
	rel, ok := homeRel(home, raw)
	if !ok {
		if isExec {
			return // exec 目标在 HOME 外：活性证据已记录，不是读路径
		}
		res.Stats.ReadsDropped++
		return
	}
	if flagsHidden && !seen["fh:"+rel] {
		seen["fh:"+rel] = true
		res.FlagsHidden = append(res.FlagsHidden, rel)
	}
	if seen[rel] {
		return
	}
	seen[rel] = true
	res.Reads = append(res.Reads, rel)
}

func parseWriteCall(c straceCall, home string, res *StraceResult, seen map[string]bool) {
	if c.ret < 0 {
		return
	}
	parts := splitArgs(c.args)
	if len(parts) == 0 {
		return
	}
	// 选路径参数：link/symlink/rename 取**最后一个字符串参数**（新建链接/目标）；
	// linkat/symlinkat 尾部可能有 flags，不能直接用 len-1。
	idx := 0
	switch c.call {
	case "openat", "openat2", "mkdirat", "unlinkat", "fchmodat", "utimensat", "execveat":
		idx = 1
	case "link", "linkat", "symlink", "symlinkat", "rename", "renameat", "renameat2":
		idx = -1
	}
	var raw string
	var ok bool
	if idx == -1 {
		for i := len(parts) - 1; i >= 0; i-- {
			if v, q := quoted(parts[i]); q {
				raw, ok = v, true
				break
			}
		}
	} else {
		if idx < 0 || idx >= len(parts) {
			return
		}
		raw, ok = quoted(parts[idx])
	}
	if !ok {
		return
	}
	if truncatedPath(raw) {
		res.Stats.ParseErrors++
		return
	}
	raw = stripDeleted(raw)
	if !filepath.IsAbs(raw) {
		// 相对路径：用该调用真正的 dirfd 参数注解，其次用返回值注解。
		di := writeDirfdIndex(c.call, len(parts))
		base := ""
		if di >= 0 {
			base = argAnnotation(parts[di])
		}
		if base == "" || !filepath.IsAbs(base) {
			if filepath.IsAbs(c.ann) {
				raw = stripDeleted(c.ann)
				goto resolved
			}
			return
		}
		raw = filepath.Join(base, raw)
	}
resolved:
	rel, ok := homeRel(home, raw)
	if !ok {
		return
	}
	kind := EntryFile
	switch c.call {
	case "mkdir", "mkdirat":
		kind = EntryDir
	case "unlink", "unlinkat":
		if !seen["w:"+rel] {
			seen["w:"+rel] = true
			res.Writes = append(res.Writes, UpperEntry{Path: rel, Kind: EntryWhiteout})
		}
		return
	}
	// creat 没有 flags 参数（等价 O_WRONLY|O_CREAT|O_TRUNC），无条件算写。
	if c.call == "open" || c.call == "openat" || c.call == "openat2" {
		flags, ok := flagsOf(c.call, c.args)
		if !ok || !isWriteOpen(c.call, flags) {
			return
		}
	}
	if seen["w:"+rel] {
		return
	}
	seen["w:"+rel] = true
	res.Writes = append(res.Writes, UpperEntry{Path: rel, Kind: kind})
}

func stripDeleted(p string) string {
	return strings.TrimSuffix(p, " (deleted)")
}

// writeDirfdIndex 返回"新路径"所在调用的 dirfd 参数下标。linkat 是
// (olddirfd, oldpath, newdirfd, newpath[, flags])，symlinkat 是
// (target, newdirfd, linkpath)，renameat* 是 (olddirfd, oldpath, newdirfd, newpath)。
// 相对写路径的归属必须用这个参数，不能一律拿 parts[0]。
func writeDirfdIndex(call string, n int) int {
	switch call {
	case "symlinkat":
		if n > 1 {
			return 1
		}
	case "linkat", "renameat", "renameat2":
		if n > 2 {
			return 2
		}
	case "mkdirat", "unlinkat", "fchmodat", "utimensat", "openat", "openat2":
		if n > 0 {
			return 0
		}
	}
	return -1
}

// truncatedPath 判定 -s 4096 截断：长度接近上限且以 "..." 结尾。
// 不能用单纯 HasSuffix("...")：字面以 "..." 结尾的文件名会被误判（已知歧义）。
func truncatedPath(p string) bool {
	return len(p) >= 4090 && strings.HasSuffix(p, "...")
}

// homeRel 把绝对路径归属到 $HOME：词法命中后还要验证真实落点仍在 HOME 内。
// 只比词法会让 `~/.config/App` 软链到 `/mnt/policy` 时被当成家目录内路径，
// 进而把共享策略文件隔离成每个分身一份（§6.2 规则 4）。
func homeRel(home, abs string) (string, bool) {
	home = filepath.Clean(home)
	abs = filepath.Clean(abs)
	rhome := Real(home)
	rabs := Real(abs)
	if rel, ok := relUnder(home, abs); ok && rel != "" {
		if _, inside := relUnder(rhome, rabs); inside {
			return filepath.ToSlash(rel), true
		}
		return "", false
	}
	if rhome != home || rabs != abs {
		if rel, ok := relUnder(rhome, rabs); ok && rel != "" {
			return filepath.ToSlash(rel), true
		}
	}
	return "", false
}

// unescapeStrace 处理 strace 打印路径时的转义（`\"`、`\\`、八进制 `\123`）。
func unescapeStrace(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '\\', '"':
			b.WriteByte(s[i])
		default:
			if s[i] >= '0' && s[i] <= '7' && i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i:i+3], 8, 8); err == nil {
					b.WriteByte(byte(v))
					i += 2
					continue
				}
			}
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// StraceCommand 把一条 bwrap 命令包进 strace（向后兼容包装）。
//
// 旧行为必须保持不变：`-f -qq -e trace=%file -o <log> -- <inner>`。
func StraceCommand(logPath string, inner []string) []string {
	return StraceCommandWith(StraceSpec{
		LogPath: logPath, Syscalls: []string{"%file"}, Tracee: inner,
	})
}

// StraceSpec 是会话命令的完整输入。
type StraceSpec struct {
	Binary   string // 空 = "strace"；生产路径传 exec.LookPath 的结果
	LogPath  string
	Syscalls []string
	Y        bool
	V        bool
	Tracee   []string
}

// StraceCommandWith 生成会话期 strace 命令。参数顺序固定，golden 测试锁定：
// `strace -f -qq [-s 4096] [-y] [-v] -e trace=<列表> -o <fifo> -- <bwrap...>`。
func StraceCommandWith(spec StraceSpec) []string {
	bin := spec.Binary
	if bin == "" {
		bin = "strace"
	}
	argv := []string{bin, "-f", "-qq", "-s", "4096"}
	if spec.Y {
		argv = append(argv, "-y")
	}
	if spec.V {
		argv = append(argv, "-v")
	}
	argv = append(argv, "-e", "trace="+strings.Join(spec.Syscalls, ","), "-o", spec.LogPath, "--")
	return append(argv, spec.Tracee...)
}
