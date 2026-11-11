package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProbeCaps 是读/写追踪能力的预检结果（ADR-034 §4.2）。
// bwrap 不可用是硬错误，不在这里表达；Read/Write 失败只降级对应证据。
type ProbeCaps struct {
	Read, Write, Openat2, Y bool
	ReadReason, WriteReason string // expression | ptrace | bwrap | unknown
	// TrueCmd 记录预检实际使用的 true 命令（三级回退），供 doctor/排障显示。
	TrueCmd []string
}

type probeCapsKey struct {
	strace, bwrap string // path+mtime+size+version
	lists         string
	y             bool
	home, base    string
	noOverlay     bool
}

var (
	probeCapsMu    sync.Mutex
	probeCapsCache = map[probeCapsKey]probeCapsEntry{}
	versionCacheMu sync.Mutex
	versionCache   = map[string]string{}
)

type probeCapsEntry struct {
	caps ProbeCaps
	at   time.Time
}

// ProbeCaps 用与会话同源的 bwrap 参数 + true 命令试跑 strace。
// traceDir 必须是本次 work/trace（预检与会话的 --tmpfs 完全一致）；
// 预检日志写 /dev/null，绝不能用会话 FIFO（没有 reader 时会阻塞）。
func (e *Env) ProbeCaps(ctx context.Context, traceDir string) (ProbeCaps, error) {
	key := e.probeCapsCacheKey(traceDir)
	probeCapsMu.Lock()
	if ent, ok := probeCapsCache[key]; ok && time.Since(ent.at) < 60*time.Second {
		probeCapsMu.Unlock()
		return ent.caps, nil
	}
	probeCapsMu.Unlock()

	caps, err := e.runProbeCaps(ctx, traceDir)
	if err != nil {
		// 失败不缓存：权限/环境可能刚被用户修好。
		return caps, err
	}
	if !cacheableCaps(caps) {
		// 能力为 false 同样是"可能刚被修好"的状态（用户装 ptrace 权限、退出
		// Flatpak 等）。缓存 60 秒会让用户重试时仍拿到旧结论，与"失败不缓存"
		// 的语义相悖；只有两项都成功时才值得复用。
		return caps, nil
	}
	probeCapsMu.Lock()
	// 双重检查：两个并发调用都在第一次检查后放行时，后到的直接复用结果，
	// 避免把同一组 5 秒预检再跑一遍。
	if ent, ok := probeCapsCache[key]; ok && time.Since(ent.at) < 60*time.Second {
		caps = ent.caps
	} else {
		probeCapsCache[key] = probeCapsEntry{caps: caps, at: time.Now()}
	}
	probeCapsMu.Unlock()
	return caps, nil
}

func (e *Env) probeCapsCacheKey(traceDir string) probeCapsKey {
	bwrap, _ := exec.LookPath("bwrap")
	return probeCapsKey{
		strace:    toolStamp(e.stracePath()),
		bwrap:     toolStamp(bwrap),
		lists:     capsHash(append(probeCapsReadList(), probeCapsWriteList()...)),
		y:         true,
		home:      e.Resolver.Home,
		base:      e.Base,
		noOverlay: true,
	}
}

// toolStamp 是缓存键里的工具身份：路径 + mtime + size + 版本。只比路径会让
// "用户刚升级 strace/bwrap"继续命中旧能力结论。
func toolStamp(path string) string {
	if path == "" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return path
	}
	return path + "|" + fi.ModTime().UTC().Format(time.RFC3339Nano) + "|" +
		strconv.FormatInt(fi.Size(), 10) + "|" + toolVersion(path)
}

func toolVersion(path string) string {
	versionCacheMu.Lock()
	if v, ok := versionCache[path]; ok {
		versionCacheMu.Unlock()
		return v
	}
	versionCacheMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	args := []string{"--version"}
	if strings.Contains(strings.ToLower(path), "strace") {
		args = []string{"-V"}
	}
	out, _ := exec.CommandContext(ctx, path, args...).Output()
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	versionCacheMu.Lock()
	versionCache[path] = line
	versionCacheMu.Unlock()
	return line
}

func (e *Env) stracePath() string {
	if p, err := exec.LookPath("strace"); err == nil {
		return p
	}
	return "strace"
}

// probeCapsReadList / probeCapsWriteList 是预检与实际会话共用的系统调用集合，
// 顺序也与会话命令一致。缓存键必须从同一对函数生成：只改一处、另一处仍是
// 旧列表时，60 秒内会命中一份与当前命令不符的能力结论（ADR-034 §4.2）。
func probeCapsReadList() []string {
	return []string{"open", "openat", "openat2", "execve", "execveat"}
}

func probeCapsWriteList() []string {
	return []string{
		"open", "openat", "openat2", "creat", "mkdir", "mkdirat", "rename", "renameat", "renameat2",
		"link", "linkat", "symlink", "symlinkat", "unlink", "unlinkat",
		"truncate", "chmod", "fchmodat", "utimensat",
	}
}

func (e *Env) runProbeCaps(ctx context.Context, traceDir string) (ProbeCaps, error) {
	var caps ProbeCaps
	caps.Y = true
	caps.TrueCmd = trueCommand()
	// bwrap 缺失是硬错误，必须在这里就报：strace 的输出形如
	// `strace: Can't stat 'bwrap': No such file or directory`，不含 "bwrap:"，
	// 否则会被归成"读能力不可用"，用户被引去查 strace（§4.2）。
	if _, err := exec.LookPath("bwrap"); err != nil {
		return caps, bwrapCapsError(err)
	}
	readList := probeCapsReadList()
	writeList := probeCapsWriteList()

	// openat2 单独试：失败时从读/写列表一起移除，记盲区。若只因 -y 失败，
	// 去掉 -y 再试；成功则全局 Y=false。
	ok, reason, err := e.tryCaps(ctx, traceDir, []string{"openat2"}, true, caps.TrueCmd)
	if err != nil {
		if reason == "bwrap" {
			return caps, bwrapCapsError(err)
		}
		ok2, reason2, err2 := e.tryCaps(ctx, traceDir, []string{"openat2"}, false, caps.TrueCmd)
		if reason2 == "bwrap" && err2 != nil {
			return caps, bwrapCapsError(err2)
		}
		if ok2 {
			caps.Openat2, caps.Y = true, false
		} else {
			caps.Openat2 = false
			readList = removeString(readList, "openat2")
		}
	} else if ok {
		caps.Openat2 = true
	}
	if !caps.Openat2 {
		writeList = removeString(writeList, "openat2")
	}

	readOK, readReason, yUsed, readErr := e.tryListWithRetry(ctx, traceDir, &readList, caps.Y, caps.TrueCmd)
	if !readOK && readReason == "bwrap" {
		// bwrap 是两条路径共同的硬错误：不能当成"读能力失败"继续跑写列表。
		return caps, bwrapCapsError(readErr)
	}
	if readOK {
		caps.Read = true
		if yUsed != caps.Y {
			caps.Y = yUsed
		}
	} else {
		caps.Read = false
		caps.ReadReason = readReason
	}
	if !containsSyscall(readList, "openat2") {
		caps.Openat2 = false
		writeList = removeString(writeList, "openat2")
	}

	writeOK, writeReason, writeY, writeErr := e.tryListWithRetry(ctx, traceDir, &writeList, caps.Y, caps.TrueCmd)
	if !writeOK && writeReason == "bwrap" {
		return caps, bwrapCapsError(writeErr)
	}
	if writeOK {
		caps.Write = true
		if writeY != caps.Y {
			// 写列表去掉 -y 才成功：会话整体切到 Y=false，并重新确认读列表。
			caps.Y = writeY
			if caps.Read {
				if ok, r, _ := e.tryCaps(ctx, traceDir, readList, false, caps.TrueCmd); !ok {
					caps.Read = false
					caps.ReadReason = r
				}
			}
		}
	} else {
		caps.Write = false
		caps.WriteReason = writeReason
	}
	if !containsSyscall(writeList, "openat2") {
		caps.Openat2 = false
	}
	return caps, nil
}

// tryListWithRetry 对读/写列表做两级降级：去掉 openat2 重试，再去掉 -y 重试。
// 写能力是兜底路径的硬前提，不能因为一个可恢复的 flags 组合直接判死。
// 返回值 y 是实际成功的 -y 取值；err 是最后一次尝试的错误（reason=="bwrap"
// 时调用方必须把它升级成硬错误）。
func (e *Env) tryListWithRetry(ctx context.Context, traceDir string, list *[]string, y bool, trueCmd []string) (bool, string, bool, error) {
	ok, reason, err := e.tryCaps(ctx, traceDir, *list, y, trueCmd)
	if err == nil {
		return ok, "", y, nil
	}
	if reason == "bwrap" {
		return false, reason, y, err
	}
	if containsSyscall(*list, "openat2") {
		*list = removeString(*list, "openat2")
		ok, reason, err = e.tryCaps(ctx, traceDir, *list, y, trueCmd)
		if err == nil {
			return ok, "", y, nil
		}
		if reason == "bwrap" {
			return false, reason, y, err
		}
	}
	if y {
		ok, reason, err = e.tryCaps(ctx, traceDir, *list, false, trueCmd)
		if err == nil {
			return ok, "", false, nil
		}
	}
	return false, reason, y, err
}

func containsSyscall(list []string, name string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

func removeString(in []string, drop string) []string {
	out := in[:0]
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// trueCommand 是预检的三级回退：/bin/true → PATH 里的 true → sh -c :。
func trueCommand() []string {
	if _, err := os.Stat("/bin/true"); err == nil {
		return []string{"/bin/true"}
	}
	if p, err := exec.LookPath("true"); err == nil {
		return []string{p}
	}
	return []string{"/bin/sh", "-c", ":"}
}

func bwrapCapsError(err error) error {
	return errEnv("err.probe.bwrap_unavailable", map[string]any{"message": err.Error()}, err)
}

// cacheableCaps 决定预检结果能否缓存 60 秒：只有读/写两项都确认可用时
// 才复用；false 往往来自可立即修复的权限/沙盒状态（§4.2）。
func cacheableCaps(caps ProbeCaps) bool { return caps.Read && caps.Write }

// tryCaps 跑一次预检。reason 用于映射"读失败"的处置；bwrap 是硬错误。
func (e *Env) tryCaps(ctx context.Context, traceDir string, list []string, y bool, trueCmd []string) (bool, string, error) {
	if len(trueCmd) == 0 {
		return false, "true", errEnv("err.probe.true_unavailable", nil, nil)
	}
	bin := e.stracePath()
	if _, err := exec.LookPath(bin); err != nil && !strings.ContainsRune(bin, '/') {
		return false, "strace", err
	}
	bwrapArgv, err := ProbeBwrapArgs(ProbeSpec{
		Home:      e.Resolver.Home,
		Base:      e.Base,
		NoOverlay: true,
		TraceDir:  traceDir,
		Command:   trueCmd,
	})
	if err != nil {
		return false, "unknown", err
	}
	argv := StraceCommandWith(StraceSpec{
		Binary:   bin,
		LogPath:  "/dev/null",
		Syscalls: list,
		Y:        y,
		V:        true,
		Tracee:   bwrapArgv,
	})
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr == nil {
		return true, "", nil
	}
	return false, classifyProbeFailure(stderr.String(), runErr), runErr
}

// classifyProbeFailure 按 bwrap > ptrace > expression > unknown 的优先级映射。
func classifyProbeFailure(stderr string, err error) string {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "creating new namespace"),
		strings.Contains(low, "no permissions"),
		strings.Contains(low, "bwrap:"),
		// strace 在 exec bwrap 之前失败（bwrap 被删除/不在 PATH）时输出
		// `Can't stat 'bwrap'`，没有 "bwrap:" 前缀。
		strings.Contains(low, "can't stat 'bwrap'"),
		strings.Contains(low, "cannot stat 'bwrap'"):
		return "bwrap"
	case strings.Contains(low, "ptrace"),
		strings.Contains(low, "operation not permitted"),
		strings.Contains(low, "could not attach"),
		strings.Contains(low, "eperm"):
		return "ptrace"
	case strings.Contains(low, "invalid system call"),
		strings.Contains(low, "invalid argument"),
		strings.Contains(low, "unknown syscall"):
		return "expression"
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "executable file not found") {
		// 缺的是 strace/true，不是 bwrap；调用方据此走"读能力不可用"而不是
		// 硬错误（否则用户会被引导去装 bwrap）。
		return "strace"
	}
	return "unknown"
}

// capsHash 给缓存/测试用：列表顺序敏感，避免"同集合不同顺序"误命中。
func capsHash(list []string) string {
	h := sha256.Sum256([]byte(strings.Join(list, ",")))
	return hex.EncodeToString(h[:8])
}
