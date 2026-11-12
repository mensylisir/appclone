package core

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"

	"golang.org/x/sys/unix"
)

// CreateInstance 建实例：写档案 + 建实例目录 + 生成桌面入口。
//
// 事务顺序（§11）不能改：中途崩溃会留下"有实例无档案""有入口无实例"三类半成品，
// 而三类半成品在 GUI 上表现为"开关是开的，点进去什么都没有"。
// CreateOptions 是建实例时的一次性选择（不进档案：它们描述"这个实例怎么来的"）。
type CreateOptions struct {
	// Fork 让新实例从主实例现有数据克隆起步（§8.1）：run 时以宿主真实数据为
	// 只读 lower、实例 root 为持久 upper。数据层在创建时定死，之后不可切换。
	Fork bool
}

func (e *Env) CreateInstance(ctx context.Context, app string, p Profile, id string) (Instance, error) {
	return e.CreateInstanceWith(ctx, app, p, id, CreateOptions{})
}

func (e *Env) CreateInstanceWith(ctx context.Context, app string, p Profile, id string, opt CreateOptions) (Instance, error) {
	if err := ValidateAppKey(app); err != nil {
		return Instance{}, err
	}
	if id != "" {
		if err := ValidateID(id); err != nil {
			return Instance{}, err
		}
	}
	p.App = app
	// 1. 全部校验在内存里做完，一个字节都不写。
	if err := ValidateProfile(p, e.BuildOptions()); err != nil {
		return Instance{}, err
	}
	// 独立密钥环是创建时就要兑现的承诺：能力缺失时直接失败，而不是建出
	// 一个每次 run 都报错的实例（方案 §6 的默认策略与 §5.1 一致）。
	if p.IsolateKeyring {
		if err := e.checkKeyringCapability(); err != nil {
			return Instance{}, err
		}
	}
	if err := ValidateCreatable(p); err != nil {
		return Instance{}, err
	}
	if err := e.ValidateBindable(p); err != nil {
		return Instance{}, err
	}

	existing, err := e.ListInstances(app)
	if err != nil {
		return Instance{}, err
	}
	names := make([]string, 0, len(existing))
	for _, in := range existing {
		names = append(names, in.DisplayName)
	}

	if id == "" {
		if id, err = e.AllocID(app); err != nil {
			return Instance{}, err
		}
	}
	display := AllocDisplayName(names)
	if err := ValidateDisplayName(display, names); err != nil {
		return Instance{}, err
	}

	now := e.Clock().UTC()
	in := Instance{
		Version:     ProfileVersion,
		App:         app,
		ID:          id,
		DisplayName: display,
		CreatedAt:   now,
		ProbeMethod: p.ProbeMethod,
		Root:        filepath.Join(e.InstancePath(app, id), "root"),
		Fork:        opt.Fork,
	}

	// 2. 档案写到临时文件，先不 rename（提交点是两个 rename，各原子）。
	profBytes, err := e.encodeProfile(p)
	if err != nil {
		return Instance{}, err
	}
	profTmp, err := stageFile(e.ProfilePath(app), profBytes, 0o600)
	if err != nil {
		return Instance{}, err
	}
	committedProfile := false
	var createdTargets []string
	cleanup := func() {
		os.Remove(profTmp)
		os.RemoveAll(e.InstancePath(app, id)) // 只删本次新建的实例目录
		// 宿主侧补建的占位目录/锁文件也要回收：留着会变成"用户没见过却存在"的空目录（§4）。
		removeCreated(createdTargets)
	}
	defer func() {
		if !committedProfile {
			os.Remove(profTmp)
		}
	}()

	// 3. Mkdir 的 EEXIST 是唯一判重依据：锁外先 stat 再 Mkdir 会 TOCTOU，
	// 两个进程都看到"不存在"，其中一个的数据被覆盖（§4.2）。
	if err := os.MkdirAll(filepath.Join(e.Base, "instances", app), 0o700); err != nil {
		return Instance{}, errEnv("err.env.mkdir", map[string]any{"path": e.Base}, err)
	}
	if err := os.Mkdir(e.InstancePath(app, id), 0o700); err != nil {
		if os.IsExist(err) {
			ids := make([]string, 0, len(existing))
			for _, o := range existing {
				ids = append(ids, o.ID)
			}
			return Instance{}, errConflict("err.instance.exists", map[string]any{
				"app": app, "id": id, "existing": ids,
			})
		}
		return Instance{}, errEnv("err.env.mkdir", map[string]any{"path": e.InstancePath(app, id)}, err)
	}

	// 4. 子目录与元数据。
	for _, sub := range []string{"root", "locks", "runtime", "dbus", "audit"} {
		if err := os.MkdirAll(filepath.Join(e.InstancePath(app, id), sub), 0o700); err != nil {
			cleanup()
			return Instance{}, errEnv("err.env.mkdir", map[string]any{"path": sub}, err)
		}
	}
	created, err := e.prepareBindTargets(p, in)
	if err != nil {
		cleanup()
		return Instance{}, err
	}
	createdTargets = created
	if err := e.saveInstance(in); err != nil {
		cleanup()
		return Instance{}, err
	}

	// 5. 桌面入口也先写临时文件。源 .desktop 必须在（§6.3：找不到应用/无 Exec 报 2）。
	d, err := e.LoadApp(app)
	if err != nil {
		cleanup()
		return Instance{}, err
	}
	launcherBytes, err := e.renderLauncher(in, d)
	if err != nil {
		cleanup()
		return Instance{}, err
	}
	launcherPath := e.LauncherPath(app, id)
	launchTmp, err := stageFile(launcherPath, launcherBytes, 0o644)
	if err != nil {
		cleanup()
		return Instance{}, err
	}

	// 6. 提交：档案 → 入口。两个 rename 各原子，中间失败最多留下"有档案无入口"，
	// repair-desktop 能补，不会丢数据。
	if err := os.Rename(profTmp, e.ProfilePath(app)); err != nil {
		os.Remove(launchTmp)
		cleanup()
		return Instance{}, errEnv("err.env.write", map[string]any{"path": e.ProfilePath(app)}, err)
	}
	committedProfile = true
	if err := os.Rename(launchTmp, launcherPath); err != nil {
		os.Remove(launchTmp)
		cleanup()
		return Instance{}, errEnv("err.env.write", map[string]any{"path": launcherPath}, err)
	}
	return in, nil
}

// prepareBindTargets 为 bind 目标补建占位物（§4 白名单的唯一显式例外）。
//
// 只增不删，并返回"本次新建清单"——失败回滚时只回收自己建的那些。
// 为什么必须自己建：bwrap 会在**宿主**为不存在的 bind 目标建占位物，
// 文件是 **0444**，那个权限会让应用 O_WRONLY 打开失败、也会让主实例
// 之后打不开同名锁文件（§3.2 第 2 条）。
func (e *Env) prepareBindTargets(p Profile, in Instance) (created []string, err error) {
	created = []string{}
	fail := func(err error) ([]string, error) {
		removeCreated(created) // 只删本次新建的，且都是空目录/空文件
		return nil, err
	}
	// 实例侧源必须存在：⑤⑧ 的源不存在是 core 的 bug，不许用 -try 掩盖。
	for _, raw := range p.Paths {
		abs, err := e.Resolver.Expand(raw)
		if err != nil {
			return fail(err)
		}
		src, err := instanceChild(in.Root, abs)
		if err != nil {
			return fail(err)
		}
		// 宿主侧目标：目录 0700，文件 0600。
		//
		// **必须按宿主的实际形态建**：`~/.config/mimeapps.list`、`~/.apprc` 这类
		// 直接写在 XDG 根或 $HOME 下的**单体文件**同样是应用状态（§3.2.4 明说
		// "不能只 find -type d"）。把它们在实例里建成目录，bwrap 就是
		// "目录 bind 到文件目标"，整条命令失败，而用户看到的是"应用起不来"。
		//
		// 已知边界：宿主路径**不存在**时形态无从判断，沿用实例侧已有形态。档案记的是
		// 路径而不是类型（§5 的 paths 是字符串数组），所以"探测时是文件、之后被删"
		// 这种竞态会退化成旧行为。根治要给档案加 `path_types`，属于 schema 变更。
		st, statErr := os.Stat(abs)
		hostExists := statErr == nil
		isFile := hostExists && !st.IsDir()
		// 实例侧占位必须是我们建的那个目录/普通文件。软链会把"工具自己写文件"
		// 变成"在宿主任意位置写文件"（越出 §4 的写入白名单）；FIFO/设备节点
		// 会让下面的 OpenFile 直接挂死（违反"禁止死等"）。
		//
		// 最后一段由 O_NOFOLLOW 兜住，**父级**兜不住：`~/.config` 被换成指向
		// 实例 root 之外的软链时，MkdirAll/OpenFile 会跟着它跑出去。所以先要求
		// "最近存在的祖先"解析后仍在 root 内。
		if !srcWithinRoot(in.Root, src) {
			return fail(errInput("err.profile.bind_target_type", map[string]any{
				"app": p.App, "path": abs,
			}))
		}
		srcSt, srcErr := os.Lstat(src)
		if srcErr == nil {
			if srcSt.Mode()&os.ModeSymlink != 0 || (!srcSt.IsDir() && !srcSt.Mode().IsRegular()) {
				return fail(errInput("err.profile.bind_target_type", map[string]any{
					"app": p.App, "path": abs,
				}))
			}
			// 宿主不存在时没有"正确形态"可依据：沿用实例侧已有的形态，
			// 而不是把文件悄悄当成目录重建。
			if !hostExists {
				isFile = srcSt.Mode().IsRegular()
			}
			// 宿主形态变了（文件↔目录）：bwrap 会"目录 bind 到文件目标"整条失败，
			// 用户看到的却是"应用起不来"。给一个指向"重新配置"的明确错误。
			if hostExists && isFile == srcSt.IsDir() {
				return fail(errInput("err.profile.bind_target_type", map[string]any{
					"app": p.App, "path": abs,
				}))
			}
		} else if !os.IsNotExist(srcErr) {
			// 读不了不是"不存在"：占位物状态未知，不能继续往里写。
			return fail(errInput("err.profile.bind_target_type", map[string]any{
				"app": p.App, "path": abs,
			}))
		}
		if isFile {
			if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
				return fail(errEnv("err.env.mkdir", map[string]any{"path": src}, err))
			}
			f, err := os.OpenFile(src, os.O_CREATE|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
			if err != nil {
				return fail(errEnv("err.env.write", map[string]any{"path": src}, err))
			}
			f.Close()
			continue
		}
		if err := os.MkdirAll(src, 0o700); err != nil {
			return fail(errEnv("err.env.mkdir", map[string]any{"path": src}, err))
		}
		if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
			made, err := mkdirAllTracked(abs, 0o700)
			created = append(created, made...)
			if err != nil {
				return fail(errEnv("err.env.mkdir", map[string]any{"path": abs}, err))
			}
		}
	}
	for _, lf := range p.LockFiles {
		abs, err := e.Resolver.Expand(lf)
		if err != nil {
			return fail(err)
		}
		lockSrc := filepath.Join(in.Root, "locks", lockFileName(abs))
		if _, statErr := os.Stat(lockSrc); os.IsNotExist(statErr) {
			if err := os.MkdirAll(filepath.Dir(lockSrc), 0o700); err != nil {
				return fail(errEnv("err.env.mkdir", map[string]any{"path": lockSrc}, err))
			}
			if f, err := os.OpenFile(lockSrc, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				f.Close()
			} else {
				return fail(errEnv("err.env.write", map[string]any{"path": lockSrc}, err))
			}
		}
		// 宿主侧锁文件必须预建 0600，理由见上（0444 会锁死主实例）。
		if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
			made, err := mkdirAllTracked(filepath.Dir(abs), 0o700)
			created = append(created, made...)
			if err != nil {
				return fail(errEnv("err.env.mkdir", map[string]any{"path": abs}, err))
			}
			f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return fail(errEnv("err.env.write", map[string]any{"path": abs}, err))
			}
			f.Close()
			created = append(created, abs)
		}
	}
	if p.IsolateKeyring {
		made, kerr := e.prepareKeyringTargets(in)
		created = append(created, made...)
		if kerr != nil {
			return fail(kerr)
		}
	}
	return created, nil
}

// prepareKeyringTargets 准备沙箱内两个 keyring bind 目标（方案 §4.3）。
//   - 目标不存在 → 建 0700 空目录（源由 EnsureKeyring 建）；
//   - 目标是普通文件 → 报错；
//   - 目标是软链：解析后等于另一个 keyring 目标就跳过（`~/.config/keyrings`
//     -> `~/.local/share/keyrings` 是常见布局，另一个 bind 已覆盖同一目录）；
//     解析到别处 → 报可操作错误，不静默跟随；
//   - 源是软链 → 报错（应由我们创建为实目录）。
func (e *Env) prepareKeyringTargets(in Instance) ([]string, error) {
	src := keyringDataDir(in)
	var created []string
	// 源目录属于实例数据（keyring.pw 与 keyring 文件在 run 时由 EnsureKeyring
	// 写入）；建实例时只把它建出来，不预造密码——保存/创建阶段替用户造状态，
	// 取消后会留下半截实例数据。
	if fi, err := os.Lstat(src); err != nil {
		if !os.IsNotExist(err) {
			return created, errEnv("err.env.mkdir", map[string]any{"path": src}, err)
		}
		made, merr := mkdirAllTracked(src, 0o700)
		created = append(created, made...)
		if merr != nil {
			return created, errEnv("err.env.mkdir", map[string]any{"path": src}, merr)
		}
	} else if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return created, errInput("err.profile.keyring_target", map[string]any{"path": src})
	}
	targets := e.BuildOptions().keyringTargets()
	for _, tgt := range targets {
		fi, err := os.Lstat(tgt)
		if err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				// 用 keyringTargetReal：双侧软链可能指向**尚未创建**的 vault
				// 目录（两个 keyring 目标都 -> ~/vault/keyrings），
				// EvalSymlinks/Real 会因末级不存在而解析不出目标。
				real := keyringTargetReal(tgt)
				// 解析后的真实目录同样不能落在 Base 下：悬空软链的词法目标
				// 可能绕过 ValidateProfile 对原始目标的检查，add 会在 Base 里
				// 建目录、run 才报错。这里按解析结果再查一次。
				if berr := checkBaseExposure(real, e.BuildOptions(), "isolate_keyring"); berr != nil {
					return created, berr
				}
				// 去重后只剩一个目标时没有"兄弟"可比：软链是该 keyring 目录的
				// 唯一入口，bind 解析后的真实目录即可（与双侧软链的兜底一致）。
				if keyringTargetCoveredBySibling(targets, tgt, real) ||
					siblingKeyringTarget(targets, tgt) == "" {
					// 被覆盖的真实目录也可能还不存在：由我们建出来（与建普通
					// 目标同一语义），回滚清单同样记录。
					if rfi, rerr := os.Lstat(real); os.IsNotExist(rerr) {
						made, merr := mkdirAllTracked(real, 0o700)
						created = append(created, made...)
						if merr != nil {
							return created, errEnv("err.env.mkdir", map[string]any{"path": real}, merr)
						}
					} else if rerr != nil || !rfi.IsDir() {
						return created, errInput("err.profile.keyring_target", map[string]any{"path": tgt})
					}
					continue // 另一个目标已经覆盖同一目录
				}
				return created, errInput("err.profile.keyring_target", map[string]any{"path": tgt})
			}
			if !fi.IsDir() {
				return created, errInput("err.profile.keyring_target", map[string]any{"path": tgt})
			}
			continue
		}
		if !os.IsNotExist(err) {
			return created, errInput("err.profile.keyring_target", map[string]any{"path": tgt})
		}
		made, merr := mkdirAllTracked(tgt, 0o700)
		created = append(created, made...)
		if merr != nil {
			return created, errEnv("err.env.mkdir", map[string]any{"path": tgt}, merr)
		}
	}
	return created, nil
}

// CheckKeyringTargets 只读校验两个 keyring bind 目标，给 doctor 这类排障
// 视图用：不建目录、不改状态，结论与 prepareKeyringTargets 一致。目标不存在
// 是正常情况（首次 run 才创建），存在但不是目录、软链解析后落在 Base 下、
// 或解析到没有兄弟目标覆盖的位置都报输入错误。
func (e *Env) CheckKeyringTargets() error {
	o := e.BuildOptions()
	targets := o.keyringTargets()
	for _, tgt := range targets {
		fi, err := os.Lstat(tgt)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return errInput("err.profile.keyring_target", map[string]any{"path": tgt})
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			real := keyringTargetReal(tgt)
			if berr := checkBaseExposure(real, o, "isolate_keyring"); berr != nil {
				return berr
			}
			if keyringTargetCoveredBySibling(targets, tgt, real) || siblingKeyringTarget(targets, tgt) == "" {
				continue
			}
			return errInput("err.profile.keyring_target", map[string]any{"path": tgt})
		}
		if !fi.IsDir() {
			return errInput("err.profile.keyring_target", map[string]any{"path": tgt})
		}
	}
	return nil
}

// keyringTargetReal 解析 keyring 目标的真实路径，**允许末级不存在**：
// `~/.config/keyrings -> ~/vault/keyrings` 在首次运行前指向一个还不存在的
// 目录，EvalSymlinks（以及 Real 的祖先回退）会停在软链本身，导致两个目标
// 被当成不同目录、bwrap 又拒绝在软链上挂载。逐级 Readlink 最多 8 层，
// 非软链/读不出时退回 Real（与项目其他路径解析同一口径）。
func keyringTargetReal(p string) string {
	for i := 0; i < 8; i++ {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return Real(p)
		}
		dst, rerr := os.Readlink(p)
		if rerr != nil {
			return Real(p)
		}
		if !filepath.IsAbs(dst) {
			dst = filepath.Join(filepath.Dir(p), dst)
		}
		p = filepath.Clean(dst)
	}
	return Real(p)
}

// siblingKeyringTarget 返回 targets 里除 tgt 之外的另一个目标。
func siblingKeyringTarget(targets []string, tgt string) string {
	for _, t := range targets {
		if t != tgt {
			return t
		}
	}
	return ""
}

// keyringTargetCoveredBySibling 报告软链目标 tgt（已解析为 real）是否被同组
// 另一个目标覆盖：两者解析到同一目录时，另一个 bind 已经把数据挂在那里，
// 再对软链目标 bind 只会让 bwrap 拒绝启动。
func keyringTargetCoveredBySibling(targets []string, tgt, real string) bool {
	other := siblingKeyringTarget(targets, tgt)
	if other == "" {
		return false
	}
	return keyringTargetReal(other) == real
}

// resolveKeyringTargets 返回实际可以 bind 的 keyring 目标：跳过被同组另一个
// 目标覆盖的软链（bwrap 不能在软链目标上挂载）。解析失败或指向别处的软链
// 原样保留——那两种情况由 prepareKeyringTargets 在 add 时给出可操作错误，
// 不在构造参数时静默吞掉。
func resolveKeyringTargets(targets []string) []string {
	out := make([]string, 0, len(targets))
	for _, tgt := range targets {
		fi, err := os.Lstat(tgt)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			out = append(out, tgt)
			continue
		}
		real := keyringTargetReal(tgt)
		if !keyringTargetCoveredBySibling(targets, tgt, real) {
			out = append(out, tgt)
		}
	}
	if len(out) > 0 {
		return out
	}
	// 两个目标都软链到**同一个**目录（如 data/conf 都指向 ~/vault/keyrings）：
	// 彼此都"被对方覆盖"，逐目标剔除会得到空列表，退回原始软链又会让 bwrap
	// 拒绝挂载。此时 bind 解析后的真实目录——应用侧软链仍能解析到它。
	seen := map[string]bool{}
	for _, tgt := range targets {
		real := keyringTargetReal(tgt)
		if real == tgt || seen[real] {
			continue // 解析不出软链目标：交给 prepare 报错
		}
		seen[real] = true
		out = append(out, real)
	}
	if len(out) == 0 {
		return targets // 解析失败：交给 prepareKeyringTargets 报可操作错误
	}
	return out
}

// srcWithinRoot 检查 src 的**最近存在祖先**解析软链后是否仍落在 root 内。
// 不存在的那几级一定是我们即将创建的；存在的祖先若指向 root 外（父级软链），
// 后面的 MkdirAll/OpenFile 就会跟着跑出去。
func srcWithinRoot(root, src string) bool {
	rootReal := Real(root)
	for p := src; ; {
		if _, err := os.Lstat(p); err == nil {
			rp, rerr := filepath.EvalSymlinks(p)
			if rerr != nil {
				return false
			}
			return rp == rootReal || IsUnder(rootReal, rp)
		} else if !os.IsNotExist(err) {
			return false
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// mkdirAllTracked 是 os.MkdirAll 的可回滚版本：返回本次真正新建的目录
// （由外到内），并**只创建缺的那几级**。
//
// 为什么要替 os.MkdirAll：`~/.config/foo/bar` 的 foo 也会被它顺手建出来，
// 但 MkdirAll 只把最内层交给调用方，回滚时 foo 就永远留在宿主上了——
// 用户看到的是一个自己从没建过的空目录，而 appclone 完全不知道它存在（§4 写入白名单）。
func mkdirAllTracked(path string, perm os.FileMode) ([]string, error) {
	var missing []string
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		missing = append(missing, p)
		if parent := filepath.Dir(p); parent == p {
			break // 已经走到根，理论上到不了（根一定存在）
		}
	}
	var created []string
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], perm); err != nil {
			// 并发创建（两个 run 同时起同一实例）会让某一级得到 EEXIST：
			// 对方已经建好的目录就是目标状态，不算失败，也不计入回滚清单
			// （回滚只删"本次真的建出来的"）。
			if os.IsExist(err) {
				if fi, serr := os.Lstat(missing[i]); serr == nil && fi.IsDir() {
					continue
				}
			}
			return created, err
		}
		created = append(created, missing[i])
	}
	return created, nil
}

// removeCreated 逆序回收 mkdirAllTracked 建出的目录：先删最内层，父目录
// 才可能变空、才有可能被删掉。顺序反了会静默留下整条空目录链。
func removeCreated(created []string) {
	sorted := append([]string(nil), created...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return pathDepth(sorted[i]) > pathDepth(sorted[j])
	})
	for _, c := range sorted {
		os.Remove(c)
	}
}

// ListInstances 读某个应用下全部实例。目录不存在返回空列表（不是错误：
// "从没开过分身"是最正常的状态）。
func (e *Env) ListInstances(app string) ([]Instance, error) {
	dir := filepath.Join(e.Base, "instances", app)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errEnv("err.env.read_dir", map[string]any{"path": dir}, err)
	}
	out := make([]Instance, 0, len(ents))
	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		in, err := e.loadInstance(app, ent.Name())
		if err != nil {
			// 单个实例元数据损坏不该让整页消失：跳过并保留其余，
			// GUI 上表现为"少一个分身"，CLI 上由 doctor 指出。
			continue
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetInstance 读单个实例。
func (e *Env) GetInstance(app, id string) (Instance, error) {
	// app 与 id 都会拼进路径，两个都必须校验：删一个 `app="../../x"` 的实例
	// 等于把 AC_BASE 之外的目录交给 gio trash（§4 <app> 规则）。
	if err := ValidateAppKey(app); err != nil {
		return Instance{}, err
	}
	if err := ValidateID(id); err != nil {
		return Instance{}, err
	}
	return e.loadInstance(app, id)
}

// RenameInstance 只改 display_name，并原子重写同一个 .desktop。
// 不换文件名：入口名只由 app/id 决定，改 display_name 换文件名会往回收站
// 里堆过期入口，还在删与建之间留一个"入口消失"的窗口（§4）。
func (e *Env) RenameInstance(app, id, newName string) error {
	in, err := e.GetInstance(app, id)
	if err != nil {
		return err
	}
	list, err := e.ListInstances(app)
	if err != nil {
		return err
	}
	existing := make([]string, 0, len(list))
	for _, o := range list {
		if o.ID == id {
			continue
		}
		existing = append(existing, o.DisplayName)
	}
	if err := ValidateDisplayName(newName, existing); err != nil {
		return err
	}
	in.DisplayName = norm.NFC.String(strings.TrimSpace(newName))
	if err := e.saveInstance(in); err != nil {
		return err
	}
	if d, err := e.LoadApp(in.App); err == nil {
		return e.WriteLauncher(in, d)
	}
	// 源 .desktop 不在了（应用被卸载）：实例元数据已改，入口保留旧名，
	// 由 repair-desktop 或删除路径处理。这里不退化成"删掉入口"。
	return nil
}

// DeleteInstance 删实例 + 入口，一律进回收站。
//
// 自己以 LOCK_NB 打开并持有 run.lock 直到 gio trash 返回：检查"没在跑"与
// trash 之间存在竞态，另一个进程可以在中间 run 起来，随后目录被搬走（§4.2）。
func (e *Env) DeleteInstance(ctx context.Context, app, id string) error {
	in, err := e.GetInstance(app, id)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(e.InstancePath(app, id), "run.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return errEnv("err.env.open_lock", map[string]any{"path": lockPath}, err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errState("err.instance.running", map[string]any{"app": app, "id": id})
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)

	// fork 实例上一轮被 SIGKILL 时可能留下仍挂着的 .fork/<hash>/merged。
	// 带着挂载把实例目录搬进回收站，等于把挂载点（及其 lower 视图）一起
	// 交出去；跨文件系统时 gio 的递归拷贝还会穿过挂载点。先卸后删，清不
	// 干净就拒绝删除——用户重试一次比丢数据好。
	forkBase := forkDir(in)
	if err := resetForkBase(forkBase); err != nil {
		return errEnv("err.instance.fork_cleanup", map[string]any{"path": forkBase}, err)
	}

	// 先删入口、再删实例：反过来的话，一旦入口这一步失败，用户就在概览里
	// 留着一个指向不存在实例的死图标——而 repair-desktop 只重建"有实例"的入口，
	// 修不了它。反过来（入口先没、实例还在）还能靠 repair-desktop 补回来。
	paths := make([]string, 0, 2)
	if lp := e.LauncherPath(app, id); lp != "" {
		if _, err := os.Stat(lp); err == nil {
			paths = append(paths, lp)
		}
	}
	paths = append(paths, e.InstancePath(app, id))
	// 走 Env.Trash 而不是直接 Trasher：它会先断言路径都落在 appclone
	// 自己的范围内（§6.3 第 775 行），越界报 5 而不是交给 gio 碰运气。
	return e.Trash(paths...)
}

// IsRunning 用 flock 试探运行态：拿到 = 没在跑（立刻释放），拿不到 = 在跑。
// 判据里没有 PID 文件，也没有 stale 状态——内核持有锁，进程怎么死都会释放
// （§4.3）。run.pid 只给 StopRun 当信号目标，绝不参与这里的判断（ADR-028）。
func (e *Env) IsRunning(app, id string) (bool, error) {
	path := filepath.Join(e.InstancePath(app, id), "run.lock")
	// 不带 O_CREATE：`list` 是只读命令，它不该在实例目录里创建文件（§4 写入白名单）。
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, errEnv("err.env.open_lock", map[string]any{"path": path}, err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			return true, nil
		}
		return false, errEnv("err.env.flock", map[string]any{"path": path}, err)
	}
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false, nil
}

// HoldRunLock 给 run 用：拿到锁就一直持有到进程退出（内核在退出时释放）。
// 拿不到立刻返回 7，不许阻塞——桌面图标被点两次是常态（§4.3）。
func (e *Env) HoldRunLock(app, id string) (*os.File, error) {
	path := filepath.Join(e.InstancePath(app, id), "run.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errEnv("err.env.open_lock", map[string]any{"path": path}, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errState("err.instance.running", map[string]any{"app": app, "id": id})
	}
	return f, nil
}

// TouchLastUsed 记录本次 run 的开始时间（instance.toml 的 last_used_at）。
func (e *Env) TouchLastUsed(app, id string) error {
	in, err := e.GetInstance(app, id)
	if err != nil {
		return err
	}
	now := e.Clock().UTC()
	in.LastUsedAt = &now
	return e.saveInstance(in)
}

// stageFile 写同目录临时文件但不提交，返回临时路径。提交由调用方 rename——
// CreateInstance 的两个 rename 才是事务的提交点。
func stageFile(path string, data []byte, perm os.FileMode) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errEnv("err.env.mkdir", map[string]any{"path": dir}, err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp."+strconvItoa(os.Getpid()))
	if err := writeFileAtomic(tmp, data, perm); err != nil {
		return "", err
	}
	// rename 到最终提交点是一次 rename；这里把 tmp 再挪到一个不含时间戳的名字，
	// 保证 stageFile 的结果可以被 rename 到目标。
	return tmp, nil
}

func strconvItoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
