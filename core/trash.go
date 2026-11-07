package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Trash 把路径移入系统回收站（§4 删除语义）。
//
// 绝不回退到 rm：实例目录里是聊天记录级别的用户数据，"回收站不可用"必须
// 以环境错误暴露，让用户自己决定下一步；静默 rm -rf 是不可逆的数据丢失。
func Trash(paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	gio, err := exec.LookPath("gio")
	if err != nil {
		return errEnv("err.env.no_gio", nil, err)
	}
	args := append([]string{"trash", "--"}, paths...)
	out, err := exec.Command(gio, args...).CombinedOutput()
	if err != nil {
		return errEnv("err.env.trash_failed", map[string]any{"output": strings.TrimSpace(string(out))}, err)
	}
	return nil
}

// LauncherDir 返回分身入口所在的目录。LauncherPath 与删除白名单共用它，
// 两处各写一遍 filepath.Join 迟早会有一处跟着环境变量改动漂走。
func (e *Env) LauncherDir() string {
	return filepath.Join(e.Resolver.XDG["DATA_HOME"], "applications")
}

// trashPrefixes 是"整棵目录都属于 appclone"的可删范围。只列这两个：
// instances/ 是分身数据，profiles.d/ 是 --purge-profile 的对象。
// cache/ 不在其中——缓存损坏的重建路径是"删文件"，不该让用户以为它进了回收站。
func (e *Env) trashPrefixes() []string {
	return []string{
		filepath.Join(e.Base, "instances"),
		filepath.Join(e.Base, "profiles.d"),
	}
}

// trashable 判定路径是否落在 appclone 自己拥有的范围内。
//
// 判定放在 core 而不是调用方：`rm` 的 app/id 由用户输入拼进路径，
// 校验漏一处就是把 AC_BASE 之外的目录交给 gio trash（§4 <app> 规则）。
func (e *Env) trashable(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	for _, pre := range e.trashPrefixes() {
		// 必须是前缀的**真子项**：`clean == pre` 意味着要删整个 instances/
		// 或整个 profiles.d/，那不是任何一次用户操作该产生的结果。
		// 注意参数序：IsUnder(parent, child)。
		if clean != pre && IsUnder(pre, clean) {
			return true
		}
	}
	// 桌面入口：只认 <应用目录>/appclone-*.desktop，且不接受空名字
	// （`appclone-.desktop` 这种不是本工具生成的，不加到白名单之外）。
	dir, base := filepath.Split(clean)
	if filepath.Clean(dir) != filepath.Clean(e.LauncherDir()) {
		return false
	}
	if !strings.HasPrefix(base, "appclone-") || !strings.HasSuffix(base, ".desktop") {
		return false
	}
	return base != "appclone-.desktop"
}

// Trash 是删除路径的唯一出口：先确认目标确属 appclone 自己的数据，再交给
// 系统回收站。目标越界一律 5 且**不调用 gio**——"拒绝并报错"比"交给 gio
// 碰运气"更可控，用户看到的是一句能照着改的报错，而不是 glib 的英文 stderr（§6.3）。
//
// 目标已不存在不算错误：并发两次删除、或用户先在文件管理器里删了，
// 结果都是"它已经不在那了"，报错只会制造假故障。
func (e *Env) Trash(paths ...string) error {
	keep := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !e.trashable(p) {
			return errConflict("err.trash.outside", map[string]any{"path": p})
		}
		if _, err := os.Lstat(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return errEnv("err.env.stat", map[string]any{"path": p}, err)
		}
		keep = append(keep, p)
	}
	if len(keep) == 0 {
		return nil
	}
	t := e.Trasher
	if t == nil {
		t = Trash
	}
	// 按调用方给的顺序逐个调用：错误能指出是哪一个失败了（gio 一次收多个路径
	// 时，中途失败只会给一句泛泛的 stderr）。顺序由调用方决定，`rm` 传的是
	// "入口在前、实例在后"——入口失败时实例还在，还能用 repair-desktop 补回来。
	for _, p := range keep {
		if err := t(p); err != nil {
			return err
		}
	}
	return nil
}

// pathDepth 数路径层数，用于"先子后父"的稳定排序。
func pathDepth(p string) int {
	return strings.Count(filepath.Clean(p), string(filepath.Separator))
}
