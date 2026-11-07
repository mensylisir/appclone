package core

import (
	"bufio"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// strace 降级路径的解析（docs/discover.md §3.4）。
//
// 能力边界先说清：strace 能看到"应用打开了什么"，看不到"写进了哪个层"，
// 所以它给不出 overlay 那样零噪声的写入集——它会给出一堆**读**操作，
// 必须靠 flags 过滤。代价是漏判（应用用 `O_PATH`、`mmap` 写、或经由
// `io_uring` 绕过）和误判（打开后只读不写也算"打开过"），档案里记
// `probe_method = "strace"`，界面要按"低置信"展示（§6.1 文案红线）。

// straceWriteSyscalls 是我们关心的系统调用。`%file` 会把 `stat` 之类也带上，
// 那对判定"写入集"没有意义，还会让日志大一个数量级。
var straceWriteSyscalls = map[string]bool{
	"open": true, "openat": true, "openat2": true, "creat": true,
	"mkdir": true, "mkdirat": true,
	"rename": true, "renameat": true, "renameat2": true,
	"link": true, "linkat": true, "symlink": true, "symlinkat": true,
	"unlink": true, "unlinkat": true,
	"truncate": true, "chmod": true, "fchmodat": true, "utimensat": true,
}

// straceLine 形如：
//
//	1234  openat(AT_FDCWD, "/home/u/.config/App/x", O_WRONLY|O_CREAT|O_CLOEXEC, 0666) = 3
//	1234  mkdirat(AT_FDCWD, "/home/u/.local/share/App", 0700) = 0
//
// `-f` 会加 pid 前缀；我们同时容忍带时间戳（`-t`）的形态。
var straceLine = regexp.MustCompile(`^(?:\d+\s+)?(?:(?:\d+(?:\.\d+)?)\s+)?([a-z_0-9]+)\((.*)\)\s+=\s+(-?\d+)`)

// straceQuoted 抓出所有字符串字面量（路径）。转义只在极少数字符上出现，
// 这里按 strace 的规则反转义 `\"` 与 `\\`，其余原样保留。
var straceQuoted = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// ParseStrace 把 strace 日志转成"相对 $HOME 的写入条目"，形状与 overlay 的
// upper 条目一致，好让后续的归并/分类/落档案完全复用。
//
// 只保留**写入意图**：`openat(..., O_RDONLY)` 不算（否则每个应用都会把整个
// 家目录的读取路径贡献成候选）。删除类系统调用记为 whiteout——"删过"不等于
// "写过"，混在一起会让用户在已删路径上勾"隔离"（§3.2.4）。
func ParseStrace(r io.Reader, home string) []UpperEntry {
	home = filepath.Clean(home)
	var out []UpperEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.Contains(line, "<unfinished") || strings.Contains(line, " resumed>") {
			continue // 多线程下 strace 会把一次调用拆成两行，拼起来太脆，直接跳过
		}
		m := straceLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		call, rest, ret := m[1], m[2], m[3]
		if !straceWriteSyscalls[call] {
			continue
		}
		if ret == "-1" {
			// 失败的调用没写任何东西。`mkdir` 返回 EEXIST 是**下一次启动**的常态，
			// 把它算成写入会让每个应用都多一条"目录已存在"的假候选。
			continue
		}
		paths := stracePaths(rest, home)
		if len(paths) == 0 {
			continue
		}
		kind := EntryFile
		switch call {
		case "mkdir", "mkdirat":
			kind = EntryDir
		case "unlink", "unlinkat":
			out = append(out, UpperEntry{Path: paths[0], Kind: EntryWhiteout})
			continue
		}
		if isWriteOpen(call, rest) {
			// rename 有两个路径：源被搬走（等价于删除）、目标是新写入。
			if call == "rename" || call == "renameat" || call == "renameat2" {
				out = append(out, UpperEntry{Path: paths[len(paths)-1], Kind: EntryFile})
				continue
			}
			out = append(out, UpperEntry{Path: paths[0], Kind: kind})
		}
	}
	return out
}

// stracePaths 取出这次调用里落在 $HOME 之下的绝对路径（相对 $HOME）。
func stracePaths(rest, home string) []string {
	var out []string
	for _, m := range straceQuoted.FindAllStringSubmatch(rest, -1) {
		p := unescapeStrace(m[1])
		if !filepath.IsAbs(p) {
			continue
		}
		rel, ok := relUnder(home, filepath.Clean(p))
		if !ok || rel == "" {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

// isWriteOpen 判定 open 系列是否带写入意图。
//
// 只认 `O_RDONLY` 的打开一律跳过；`O_PATH` 也跳过（它连读都不算，拿不到内容）。
// 其余只要有 W/RDWR/CREAT/TRUNC/APPEND 之一就算——`O_CREAT|O_RDONLY` 也会建文件。
func isWriteOpen(call, rest string) bool {
	switch call {
	case "open", "openat", "openat2", "creat":
	default:
		return true // mkdir/rename/link/symlink/truncate/chmod 本身就是写
	}
	if call == "creat" {
		return true
	}
	switch {
	case strings.Contains(rest, "O_WRONLY"), strings.Contains(rest, "O_RDWR"),
		strings.Contains(rest, "O_CREAT"), strings.Contains(rest, "O_TRUNC"),
		strings.Contains(rest, "O_APPEND"):
		return true
	}
	return false
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
			// 八进制转义：\NNN
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

// StraceCommand 把一条 bwrap 命令包进 strace。
//
// strace 跑在**沙箱外面**：日志由它在宿主写，不需要考虑"沙箱里往哪写日志"；
// 跟随 fork（`-f`）才能看到 bwrap 拉起的应用；`%file` 是唯一需要的类别。
func StraceCommand(logPath string, inner []string) []string {
	argv := []string{"strace", "-f", "-qq", "-e", "trace=%file", "-o", logPath, "--"}
	return append(argv, inner...)
}
