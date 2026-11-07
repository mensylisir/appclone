package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/mensylisir/appclone/core/i18n"
)

// CLIPath 可被构建期 -ldflags -X 覆盖。解析顺序固定为：
// 同目录 sibling `appclone` → 注入值 → PATH 查找（§4.1）。
//
// 绝不用 os.Executable()：生成入口的调用者通常是 appclone-gui，
// 照抄会把入口写成 `appclone-gui run …`，点击只会再弹一个设置窗口。
var CLIPath string

// nameSuffixMax 是 .desktop 的 Name 总长上限（含后缀，rune）。
// GNOME 概览标签在小图标下约 12 个全角字符后截断，超长会让两个分身
// 看起来一模一样（后缀被切掉），比截断显示名更糟（§4.1）。
const nameSuffixMax = 40

// ResolveCLIPath 返回 CLI 二进制绝对路径；三者都失败时报 2，
// 绝不回退到 GUI 二进制。
func ResolveCLIPath() (string, error) {
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "appclone")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			if abs, err := filepath.Abs(cand); err == nil {
				return abs, nil
			}
		}
	}
	if CLIPath != "" {
		if abs, err := filepath.Abs(CLIPath); err == nil {
			return abs, nil
		}
	}
	if p, err := exec.LookPath("appclone"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
	}
	return "", errInput("err.launcher.no_cli", nil)
}

// LauncherPath 返回分身入口的路径。文件名只由 app/id 决定：
// 重命名 display_name 不换文件名（§4）。
func (e *Env) LauncherPath(app, id string) string {
	dir := filepath.Join(e.Resolver.XDG["DATA_HOME"], "applications")
	return filepath.Join(dir, "appclone-"+app+"-"+id+".desktop")
}

// launcherName 拼「应用名（分身 显示名）」，超长时截断显示名并补省略号。
//
// 后缀模板来自 i18n（desktop.clone_name_suffix），不写死在代码里：
// 本地化只翻模板、不翻用户数据——display_name 原样嵌入。
func launcherName(appName, display, lang string) string {
	tmpl := i18n.T(lang, "desktop.clone_name_suffix", map[string]any{"name": display})
	head, tail, _ := strings.Cut(tmpl, display)
	full := appName + head + display + tail
	if utf8.RuneCountInString(full) <= nameSuffixMax {
		return full
	}
	// 超限时截断显示名部分并补省略号：GNOME 概览里后缀被切掉，
	// 两个分身会看起来一模一样（§4.1）。
	keep := nameSuffixMax - utf8.RuneCountInString(appName+head+tail) - 1
	if keep < 1 {
		keep = 1
	}
	r := []rune(display)
	if len(r) > keep {
		r = r[:keep]
	}
	return appName + head + string(r) + "…" + tail
}

// WriteLauncher 原子重写同一个 .desktop（§4.1 全部规则）。
func (e *Env) WriteLauncher(in Instance, d App) error {
	data, err := e.renderLauncher(in, d)
	if err != nil {
		return err
	}
	return writeFileAtomic(e.LauncherPath(in.App, in.ID), data, 0o644)
}

// renderLauncher 只渲染，不写盘：CreateInstance 需要先拿到临时文件内容
// 参与事务，不能在这里直接落盘。
func (e *Env) renderLauncher(in Instance, d App) ([]byte, error) {
	cli := e.CliPath
	if cli == "" {
		var err error
		if cli, err = ResolveCLIPath(); err != nil {
			return nil, err
		}
	}
	appName := d.Name
	if appName == "" {
		appName = in.App
	}
	icon := d.Icon
	if icon == "" {
		icon = "application-x-executable"
	}
	categories := d.Categories
	if categories == "" {
		categories = "Utility;"
	}
	// 继承是白名单，不是"抄一遍"：Actions/MimeType/DBusActivatable 必须丢弃，
	// 否则分身会重复注册 MIME 处理与 DBus 名，右键菜单与文件关联直接错乱。
	var b strings.Builder
	b.WriteString("[Desktop Entry]\n")
	b.WriteString("Version=1.0\n") // Desktop Entry 规范版本，与被分身应用的版本无关
	b.WriteString("Type=Application\n")
	// 三行都写：Name 是兜底（无 [lang] 时桌面环境用它），另两行是本地化。
	fmt.Fprintf(&b, "Name=%s\n", escapeDesktopValue(launcherName(appName, in.DisplayName, "en")))
	fmt.Fprintf(&b, "Name[zh_CN]=%s\n", escapeDesktopValue(launcherName(appName, in.DisplayName, "zh_CN")))
	fmt.Fprintf(&b, "Name[en]=%s\n", escapeDesktopValue(launcherName(appName, in.DisplayName, "en")))
	exec := cli + " run " + quoteExecArg(in.App) + " " + quoteExecArg(in.ID)
	if d.FieldCodes != "" {
		// 保留源 Exec 的字段码（%U/%F），文件管理器"用分身打开"依赖它；
		// 源没有字段码时不要自己补（补了应用会收到不认识的参数）。
		exec += " " + d.FieldCodes
	}
	fmt.Fprintf(&b, "Exec=%s\n", exec)
	fmt.Fprintf(&b, "TryExec=%s\n", escapeDesktopValue(cli))
	fmt.Fprintf(&b, "Icon=%s\n", escapeDesktopValue(icon))
	b.WriteString("Terminal=false\n") // 分身不启终端
	b.WriteString("StartupNotify=true\n")
	if d.StartupWMClass != "" {
		// 缺省就整行省略，不写空值：空 StartupWMClass 会让窗口分组错乱。
		fmt.Fprintf(&b, "StartupWMClass=%s\n", escapeDesktopValue(d.StartupWMClass))
	}
	fmt.Fprintf(&b, "Categories=%s\n", escapeDesktopValue(categories))
	fmt.Fprintf(&b, "X-Appclone-App=%s\n", escapeDesktopValue(in.App))
	fmt.Fprintf(&b, "X-Appclone-Id=%s\n", escapeDesktopValue(in.ID))
	return []byte(b.String()), nil
}

// RemoveLauncher 把入口移入回收站（与实例同一套删除语义）。
func (e *Env) RemoveLauncher(app, id string) error {
	p := e.LauncherPath(app, id)
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	return e.Trasher(p)
}

// escapeDesktopValue 转义 .desktop 值里的 \、换行、制表符。
// 第三方 Name= 里一个换行就能把后续任意键注入进来。
func escapeDesktopValue(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		"\n", `\n`,
		"\t", `\t`,
		"\r", `\r`,
	)
	return r.Replace(s)
}

// quoteExecArg 给 Exec 里的单个参数加引号（Desktop Entry 规范的 Exec 语法）。
// 含空格、引号、特殊字符时必须整体加双引号并转义内部引号，否则
// `Exec=/path/appclone run my app work` 会被拆成两个参数。
func quoteExecArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\><~|&;$*?#()`") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '`', '$', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
