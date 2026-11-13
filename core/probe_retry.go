package core

import (
	"context"
	"errors"
)

// ProbeRetryCommand 返回零写入失败后应重试的命令；nil 表示不该重试。
//
// 触发条件（ADR-036 ⑧）：
//   - 本次探测接受了唯一的自重启增量，且证据返回值已知（unknown 证据只做
//     run-scoped 诊断，不足以驱动一次真实启动）；
//   - 原命令非空，增量非空。
//
// 返回新切片，调用方修改不会污染原命令或 ProbeResult。
func ProbeRetryCommand(cmdline []string, res ProbeResult) []string {
	if len(cmdline) == 0 || len(res.SelfRelaunchArgs) == 0 || res.SelfRelaunchUnknown {
		return nil
	}
	out := make([]string, 0, len(cmdline)+len(res.SelfRelaunchArgs))
	out = append(out, cmdline...)
	out = append(out, res.SelfRelaunchArgs...)
	return out
}

// ProbeWithRetry 跑一次探测并落案；若落案因零写入失败、而本次已识别到
// 自重启追加参数，则用该参数再探一次（仅一次）。CLI `add` 与 GUI 确认页
// 共用这一份流程，避免两端对同一失败给出不同行为。
//
// 为什么需要重探：Electron 应用在 Wayland 下 detached 自重启时，直接子进程
// 立刻退出，bwrap 拆毁 PID namespace，应用还没写出可隔离的路径就被杀；
// 此时证据已经够判断"应用需要 --ozone-platform=x11"，但零写入门禁会丢弃
// 整个结果。带上该参数重探，应用才能真的跑起来并写出配置。
//
// 第二次探测失败时原样返回第二次的错误，不再重试——避免"每次失败都多
// 启动一轮"的循环。
func (e *Env) ProbeWithRetry(ctx context.Context, app string, opt ProbeOptions, fin ProbeFinalize) (ProbeResult, Profile, []Candidate, int, error) {
	res, err := e.Probe(ctx, app, opt)
	if err != nil {
		return res, Profile{}, nil, 0, err
	}
	p, cands, prefilled, ferr := e.FinalizeProbe(app, res, fin)
	if ferr == nil {
		return res, p, cands, prefilled, nil
	}
	retry := ProbeRetryCommand(opt.Command, res)
	if retry == nil || !isNoWritesErr(ferr) {
		return res, p, cands, prefilled, ferr
	}
	if opt.OnRetry != nil {
		opt.OnRetry(append([]string(nil), res.SelfRelaunchArgs...))
	}
	opt.Command = retry
	res, err = e.Probe(ctx, app, opt)
	if err != nil {
		return res, Profile{}, nil, 0, err
	}
	p, cands, prefilled, ferr = e.FinalizeProbe(app, res, fin)
	return res, p, cands, prefilled, ferr
}

// isNoWritesErr 只认 err.probe.no_writes：其他失败（环境、取消、档案冲突）
// 重探没有意义，多启动一轮只会更糟。
func isNoWritesErr(err error) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.Key == "err.probe.no_writes"
}
