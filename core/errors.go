// Package core 是 appclone 的唯一事实核心：探测管线、bwrap 参数构造、档案
// 与实例的读写、状态一致性。CLI 与 GUI 都只是它的薄视图，不许在这里 import
// 任何视图层依赖（cobra、GTK）。
package core

import (
	"errors"
	"os"
)

// 退出码，与 docs/方案设计.md §6.3 一一对应。脚本与 GUI 都靠 Code 分支，
// 不许解析人类文案（文案走 i18n 键）。
const (
	CodeOK       = 0
	CodeInternal = 1 // 内部错误 / 用法错误，留给 appclone 自己
	CodeInput    = 2 // 输入 / 档案不合法
	CodeEnv      = 3 // 环境不可用（bwrap、userns、gio、写层嵌套）
	CodeProbe    = 4 // 探测无结论
	CodeConflict = 5 // 冲突（实例已存在、拒绝删除）
	CodeBusy     = 6 // 并发（全局写锁、目标应用正在运行）
	CodeState    = 7 // 状态不允许（实例正在运行）
	CodeLeak     = 8 // 隔离校验发现泄漏
)

// Error 是 core 唯一允许跨视图传播的错误形态。
// Error() 只返回 i18n 键：人类文案一律在 core 之外拼，否则同一个错误
// 会在 CLI 与 GUI 里变成两种说法。
type Error struct {
	Code int
	Key  string
	Args map[string]any
	Err  error
}

func (e *Error) Error() string { return e.Key }

// Unwrap 保留底层原因（os/exec 的 stderr、unix.Errno 等），
// 供 doctor 与日志使用；对用户可见的仍然是 Key。
func (e *Error) Unwrap() error { return e.Err }

func newErr(code int, key string, args map[string]any, err error) *Error {
	return &Error{Code: code, Key: key, Args: args, Err: err}
}

// errInput / errEnv / ... 是各分类的常用构造器，省掉到处写 Code 常量。
func errInput(key string, args map[string]any) *Error {
	return newErr(CodeInput, key, args, nil)
}

func errEnv(key string, args map[string]any, err error) *Error {
	return newErr(CodeEnv, key, args, err)
}

func errConflict(key string, args map[string]any) *Error {
	return newErr(CodeConflict, key, args, nil)
}

func errBusy(key string, args map[string]any) *Error {
	return newErr(CodeBusy, key, args, nil)
}

func errState(key string, args map[string]any) *Error {
	return newErr(CodeState, key, args, nil)
}

func errInternal(key string, args map[string]any, err error) *Error {
	return newErr(CodeInternal, key, args, err)
}

// ExitCode 把任意 error 归一成退出码：nil→0，*Error→Code，其他→1。
func ExitCode(err error) int {
	if err == nil {
		return CodeOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// ErrCanceled 是用户主动取消（探测中按「取消」）。run/add 收到它返回 0：
// 取消不是失败，给非零码会让脚本把"用户不想干了"当成故障。
var ErrCanceled = errors.New("core: canceled")

// ErrNotRunning 供 IsRunning 的调用方判空，不属于用户可见错误。
var ErrNotRunning = errors.New("core: not running")

// CheckNotRoot 是"免 root 红线"的守门人：run 与 add 都必须过它（§3.1 末段）。
// root 会话下 --bind / / 是 rw，误操作能改宿主系统；而且 root 下 bwrap 本来就能
// 跑通，"验证通过"会把非特权路径的问题全部藏起来。
func CheckNotRoot() error {
	if os.Geteuid() == 0 {
		return errEnv("err.env.root", nil, nil)
	}
	return nil
}

// NewUsage 是用法错误的统一形态：退出码 1（§6.3 里 1 留给 appclone 自己）。
func NewUsage(msg string) error {
	return newErr(CodeInternal, "err.internal.usage", map[string]any{"message": msg}, nil)
}

// NewAuditUnavailable：--audit 已在命令面上，但运行期审计层还没实现。
// 明确拒绝，而不是静默 no-op——静默会让用户以为开了审计，而 doctor --verify
// 永远输出 unverified，用户只会觉得校验坏了。
func NewAuditUnavailable() error {
	return newErr(CodeProbe, "err.audit.unavailable", nil, nil)
}

// NewProbeUnavailable 表示"这个应用还没有可用档案，而探测管线不可用"。
// 它必须是 CodeProbe 而不是 CodeInput：前者是"环境暂时给不出结论"，
// 后者是"你给的东西不合法"，脚本对这两类的处理完全不同。
func NewProbeUnavailable(app string) error {
	return newErr(CodeProbe, "err.probe.unavailable", map[string]any{"app": app}, nil)
}

// NewProbeNoWrites：探测跑完了，但没观测到任何值得隔离的写入。
// 必须拒绝生成空档案（§6.3）：空 paths 的档案能让开关亮着但什么都不隔离，
// 那是最难被用户发现的一种坏状态。退出码 4 = "环境给不出结论"，不是输入错误。
func NewProbeNoWrites(app string) error {
	return newErr(CodeProbe, "err.probe.no_writes", map[string]any{"app": app}, nil)
}
