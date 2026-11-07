package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/mensylisir/appclone/core"
)

type check struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
}

func newDoctorCmd(a *app) *cobra.Command {
	var strict, verify bool
	c := &cobra.Command{
		Use:   "doctor [<app> [<id>]]",
		Short: "环境检查（bwrap / userns / AppArmor / fuse / dbus / gio / GTK）",
		Long: `环境检查，排障用、只读。

退出码约定：报告完成即 0（结论在输出里），参数错误 2，
只有 --strict 才在必需项失败时返回 3——否则脚本无法区分
"doctor 自己失败"和"检查结论是环境坏"。`,
		Args: cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.applyOverrides(cmd); err != nil {
				return err
			}
			if verify {
				return a.runVerify(cmd, args)
			}
			checks := runChecks(a)
			data := map[string]any{"checks": checks}

			var command string
			if len(args) >= 1 {
				appKey := args[0]
				id := "main"
				if len(args) == 2 {
					id = args[1]
				}
				p, err := a.env.LoadProfile(appKey)
				if err != nil {
					return err
				}
				in, err := a.env.GetInstance(appKey, id)
				if err != nil {
					return err
				}
				o := a.env.BuildOptions()
				o.RuntimeSocks = core.FilterExisting(core.RuntimeSocketCandidates(a.env.Resolver.XDG["RUNTIME_DIR"]))
				if p.IsolateDBus {
					o.DBusAddress = "unix:path=" + filepath.Join(in.Dir(), "dbus", "bus")
				}
				argv, err := core.BwrapCommand(p, in, o)
				if err != nil {
					return err
				}
				command = strings.Join(argv, " ")
				data["command"] = command
			}

			a.ok(data, func() {
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				for _, c := range checks {
					mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[c.Status]
					fmt.Fprintf(w, "%s\t%s\t%s\n", mark, c.ID, c.Detail)
				}
				w.Flush()
				if command != "" {
					fmt.Println("\n将执行的命令：")
					fmt.Println("  " + command)
				}
			})

			if strict {
				for _, c := range checks {
					if c.Status == "fail" && required(c.ID) {
						os.Exit(core.CodeEnv)
					}
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&strict, "strict", false, "必需项失败时返回退出码 3")
	c.Flags().BoolVar(&verify, "verify", false, "对账审计证据给出隔离结论（无证据时只给 unverified，不是通过）")
	return c
}

// runVerify 是 doctor --verify 的实现口径（§6.5）。
//
// 判定本身在 core（`Env.Verify`/`Env.Judge`），这里只负责把结论翻成人话与退出码。
// 本版本没有审计写入器，正常情况下结论必然是 unverified——**"没有证据"不等于
// "没有泄漏"**，把它显示成通过会让用户以为隔离被验证过（本产品最不该犯的错）。
func (a *app) runVerify(cmd *cobra.Command, args []string) error {
	if len(args) < 2 {
		return core.NewUsage("doctor --verify 需要 <app> <id>")
	}
	appKey, id := args[0], args[1]
	v, err := a.env.Verify(appKey, id)
	if err != nil {
		return err
	}
	// coverage 按 §11 的 schema 给字符串（脚本与测试都读它）：单一路径就是
	// `$HOME`，多路径用「、」拼接。数组形态另放 coverage_paths，方便排障。
	coverage := ""
	switch {
	case len(v.Coverage) == 1 && v.Coverage[0] == a.env.Resolver.Home:
		coverage = "$HOME"
	case len(v.Coverage) > 0:
		coverage = strings.Join(v.Coverage, "、")
	}
	// 覆盖范围为空（摘要损坏）时不打印"覆盖范围： 之内的写入"这种半句话。
	coverageLine := func() {
		if coverage != "" {
			fmt.Printf("覆盖范围：%s 之内的写入\n", coverage)
		}
	}
	a.ok(map[string]any{
		"conclusion":     v.Conclusion,
		"coverage":       coverage,
		"coverage_paths": v.Coverage,
		"violations":     v.Violations,
		"ignored":        v.Ignored,
		"retained":       v.Retained,
		"reason":         v.Reason,
		"generated_at":   v.GeneratedAt,
	}, func() {
		switch {
		case v.Conclusion == core.VerdictVerified:
			fmt.Println("结论：verified（本次审计覆盖到了写入，且都在声明范围内）")
			coverageLine()
		case v.Conclusion == core.VerdictLeak:
			fmt.Printf("结论：leak-detected（%d 处写到了声明范围之外）\n", len(v.Violations))
			for _, p := range v.Violations {
				fmt.Println("  " + p)
			}
			// 摘要自己说"证据没保留"时必须照实说：那意味着越界文件可能已经被删了。
			if v.Retained {
				fmt.Println("证据保留在实例子目录 audit/ 里，没有删除。建议重新探测后再运行。")
			} else {
				fmt.Println("注意：这份摘要记录为「证据未保留」——如果这不是你有意为之，越界文件可能已经无法从 audit/ 取回。")
			}
			coverageLine()
		default:
			// unverified 的下一步取决于原因：跑过一次崩掉的和从没跑过的
			// 是完全不同的两件事，共用一句"未开启审计"等于什么都没说。
			fmt.Printf("结论：unverified（%s）\n", unverifiedWhy(v.Reason))
			switch v.Reason {
			case "audit_incomplete":
				fmt.Println("上次审计没有正常结束（缺 run.toml 或 upper 被删），证据不完整，因此不下结论。")
				fmt.Println("证据已保留，没有删除；可以重新跑一次 --audit。")
			case "no_writes":
				fmt.Println("审计跑了，但这次没有观测到任何写入，无法判断隔离是否有效。")
				fmt.Println("空集不算通过。请把应用真正用一遍再审计一次。")
			case "audit_unreadable":
				fmt.Println("已存的审计摘要读不出来（文件损坏），因此不下结论；摘要已保留。")
			case "extract_failed":
				fmt.Println("审计层本该把公共目录 / 数据目录 / 实例目录 bind 回真实位置，但它们的路径出现在隔离层里——")
				fmt.Println("说明 bind 没有生效，本次审计作废。**先别清理 quarantine**，检查 ~/Downloads 等目录是否缺文件，")
				fmt.Println("处理完之后重新跑一次 --audit + --verify 才是有效结论。")
				for _, p := range v.Violations {
					fmt.Println("  " + p)
				}
			default:
				fmt.Println("本版本还没有审计写入器（run --audit），没有证据就无法归因。")
				fmt.Println("这不是通过：没有证据不等于没有泄漏。")
			}
			coverageLine()
		}
		if v.GeneratedAt != "" {
			fmt.Printf("结论时间：%s（重放的已存摘要）\n", v.GeneratedAt)
		}
		if v.Retained {
			fmt.Println("证据保留：是（audit/ 目录未清理）")
		}
		if len(v.Ignored) > 0 {
			// §5：审计输出必须列出被忽略的类别，不许悄悄忽略。
			fmt.Printf("已忽略的类别：%s\n", strings.Join(v.Ignored, "、"))
		}
	})
	if code := v.ExitCode(); code != core.CodeOK {
		os.Exit(code)
	}
	return nil
}

// unverifiedWhy 把 reason 代号翻成一句话。没有 case 落到"未开启审计"。
func unverifiedWhy(reason string) string {
	switch reason {
	case "audit_incomplete":
		return "上次审计未正常结束"
	case "no_writes":
		return "审计跑了但没有观测到写入"
	case "audit_unreadable":
		return "已存的审计摘要无法解析"
	case "extract_failed":
		return "审计层的目录摘出没有生效，本次审计作废"
	}
	return "未开启运行期审计，无法判定"
}

// required 区分"没有它产品起不来"和"没有它只是降级"。
// fuse 缺失只是探测降级到 strace，不该让 --strict 红。
func required(id string) bool {
	switch id {
	case "bwrap", "userns", "apparmor", "state-dir":
		return true
	}
	return false
}

func runChecks(a *app) []check {
	var out []check
	add := func(id, status, detail string) { out = append(out, check{ID: id, Status: status, Detail: detail}) }

	if p, err := exec.LookPath("bwrap"); err == nil {
		v := strings.TrimSpace(runOut(p, "--version"))
		add("bwrap", "ok", v+"（"+p+"）")
	} else {
		add("bwrap", "fail", "未安装 bubblewrap，分身功能不可用")
	}

	maxNS := strings.TrimSpace(readFile("/proc/sys/user/max_user_namespaces"))
	clone := strings.TrimSpace(readFile("/proc/sys/kernel/unprivileged_userns_clone"))
	switch {
	case maxNS == "0":
		add("userns", "fail", "max_user_namespaces=0，非特权用户命名空间被禁用")
	case clone != "" && clone != "1":
		add("userns", "fail", "unprivileged_userns_clone="+clone+"，非特权用户命名空间被禁用")
	case maxNS == "" && clone == "":
		// 两个 sysctl 都读不到（容器/受限 /proc）时判 ok 是假通过：
		// doctor 的全部价值就是"能确认"，确认不了要给 warn。
		add("userns", "warn", "读不到 userns 相关 sysctl（/proc 受限），无法确认；实际能否建 user ns 需实测")
	default:
		add("userns", "ok", "可用（max_user_namespaces="+orDefault(maxNS, "?")+"）")
	}

	restrict := strings.TrimSpace(readFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"))
	if restrict == "1" {
		if _, err := os.Stat("/etc/apparmor.d/bwrap"); err == nil {
			add("apparmor", "ok", "限制已开启，但存在 /etc/apparmor.d/bwrap（bwrap 已放行）")
		} else {
			add("apparmor", "fail", "apparmor 限制非特权 userns 且缺少 /etc/apparmor.d/bwrap，bwrap 会报 setting up uid map: Permission denied")
		}
	} else {
		add("apparmor", "ok", "未限制非特权 userns")
	}

	if p, err := exec.LookPath("fuse-overlayfs"); err == nil {
		// 用 Access(R_OK) 而不是 Stat：设备节点存在但打不开（权限/被 mask）
		// 是最常见的形态，Stat 会说"有"，实际挂载立刻失败。
		if err := unix.Access("/dev/fuse", unix.R_OK|unix.W_OK); err == nil {
			add("fuse", "ok", "fuse-overlayfs 可用（"+p+"）")
		} else {
			add("fuse", "warn", "fuse-overlayfs 在，但 /dev/fuse 打不开；探测将降级到 strace")
		}
	} else {
		add("fuse", "warn", "缺少 fuse-overlayfs：探测将降级到 strace，结果可能不完整（不影响运行分身）")
	}

	if p, err := exec.LookPath("dbus-daemon"); err == nil {
		add("dbus", "ok", p)
	} else {
		add("dbus", "warn", "缺少 dbus-daemon：需要私有会话总线的应用可能无法双开（会降级为共享总线）")
	}
	if p, err := exec.LookPath("gio"); err == nil {
		add("gio", "ok", p)
	} else {
		add("gio", "fail", "缺少 gio（glib2）：删除操作无法移入回收站，appclone 不会用 rm 代替")
	}

	if st, dt := gtkCheck("gtk4", "libgtk-4.so.1", "4.14", "GTK4"); true {
		add("gtk4", st, dt)
	}
	if st, dt := gtkCheck("libadwaita-1", "libadwaita-1.so.0", "1.5", "libadwaita"); true {
		add("libadwaita", st, dt)
	}

	// 数据目录可写性：doctor 是**只读**命令，不创建目录、不写探针文件——
	// 那超出了 §4 的写入白名单，而且一个"检查环境"的命令顺手改环境会让人不敢用。
	if fi, err := os.Stat(a.env.Base); err == nil {
		if !fi.IsDir() {
			// 同名普通文件：Access(W_OK) 会说"可写"，然后第一次 add 就报错。
			add("state-dir", "fail", "数据目录的位置被一个普通文件占着："+a.env.Base)
		} else if err := unix.Access(a.env.Base, unix.W_OK); err != nil {
			add("state-dir", "fail", "数据目录不可写："+a.env.Base)
		} else {
			add("state-dir", "ok", a.env.Base)
		}
	} else if !os.IsNotExist(err) {
		add("state-dir", "fail", "数据目录不可访问："+a.env.Base)
	} else if anc, ok := existingAncestor(filepath.Dir(a.env.Base)); !ok {
		add("state-dir", "fail", "数据目录的上级都不存在："+filepath.Dir(a.env.Base))
	} else if err := unix.Access(anc, unix.W_OK); err != nil {
		add("state-dir", "fail", "数据目录的上级不可写："+anc)
	} else {
		// 全新会话里 xdg 目录链常常还没建；这是正常状态，不是错误。
		add("state-dir", "warn", "数据目录还不存在（首次运行时会创建）："+a.env.Base)
	}
	return out
}

// existingAncestor 往上找到第一个存在的目录：新建会话里
// `$XDG_DATA_HOME/appclone` 的整条链可能都不存在，"能不能写"要问最上层那个存在的。
func existingAncestor(dir string) (string, bool) {
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		if parent := filepath.Dir(p); parent == p {
			return "", false
		}
	}
}

// gtkCheck 查 GUI 两个运行库的版本，CLI 二进制绝不 import GUI 包：
// 纯 CLI 机器上 import 会让 init 阶段 panic（§6 doctor 条目）。
//
// 探测顺序按可信度递减，并且**把来源写进结论**——版本号来自哪里决定了它能不能
// 当证据用：
//  1. `pkg-config --modversion`：有开发包时精确，但运行库装、开发包不装（普通用户
//     的常态）就拿不到；
//  2. 库文件名的 libtool 版本段：GTK 用 `-version-info` 生成
//     `libgtk-4.so.1.<minor*100>.<micro>`（4.24.1 → 2400.1），发行版一致，但它是
//     文件名约定，不是 ABI 里的事实；
//  3. 都拿不到 → `warn` + 明确说"无法判定版本"，**不冒充通过**（§6.3 的同一条纪律）。
//
// 真正的 dlopen 探测（`gtk_get_minor_version()`）要等 GUI 增量引入 purego 依赖后
// 才接入：现在为它单独引一个模块不划算，而"不知道"比"猜一个数"诚实。
func gtkCheck(pkg, soname, min, label string) (string, string) {
	if p, err := exec.LookPath("pkg-config"); err == nil {
		if v := strings.TrimSpace(runOut(p, "--modversion", pkg)); v != "" {
			if versionLess(v, min) {
				return "warn", label + " " + v + " 低于要求的 " + min + "（pkg-config）"
			}
			return "ok", label + " " + v + "（pkg-config）"
		}
	}
	lib := findLib(soname)
	if lib == "" {
		// 库不存在 ≠ 版本太低：CLI 完全不需要它，所以是 warn 而不是 fail。
		return "warn", label + " 未安装（只影响 GUI）"
	}
	v, src := libVersion(lib, soname)
	if v == "" {
		return "warn", label + " 无法判定版本（缺 pkg-config，且 " + filepath.Base(lib) +
			" 的库文件名不带版本号）；GUI 需要 ≥ " + min
	}
	if versionLess(v, min) {
		return "warn", label + " " + v + " 低于要求的 " + min + "（" + src + "）"
	}
	return "ok", label + " " + v + "（" + src + "）"
}

// findLib 在常规库目录里找 soname。多架构目录（Debian 系的
// /usr/lib/x86_64-linux-gnu）用扫一层子目录的办法覆盖，不去解析 ld.so.conf：
// doctor 是排障口，认得发行版的事实来源比认得 ld.so 的全部语法更实用。
func findLib(soname string) string {
	for _, d := range []string{"/usr/lib", "/usr/lib64", "/lib", "/lib64", "/usr/local/lib"} {
		p := filepath.Join(d, soname)
		if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	ents, err := os.ReadDir("/usr/lib")
	if err != nil {
		return ""
	}
	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		p := filepath.Join("/usr/lib", ent.Name(), soname)
		if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// libVersion 从库路径推版本。只有 `libfoo.so.N.<编码>.<micro>` 这种带版本段的
// 命名能给出结果（GTK 是；libadwaita 在多数发行版上是平的
// `libadwaita-1.so.0`，拿不到）。
func libVersion(path, soname string) (version, source string) {
	// 只对 GTK 解码：`libgtk-4.so.1.<minor*100>.<micro>` 是 GTK 自己的 libtool
	// 约定（4.24.1 → 2400.1）。libadwaita 用的是另一套编号，套同一个公式会把
	// `1.5.0` 读成 `1.15.0` —— 宁可返回"不知道"，也不能给一个看起来很像的错版本。
	if !strings.HasPrefix(soname, "libgtk-") {
		return "", ""
	}
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	}
	base := filepath.Base(real)
	rest, ok := strings.CutPrefix(base, soname)
	if !ok {
		return "", ""
	}
	parts := strings.Split(strings.TrimPrefix(rest, "."), ".")
	if len(parts) < 2 {
		return "", ""
	}
	enc, err := strconv.Atoi(parts[0])
	if err != nil || enc < 100 || enc > 9999 {
		return "", ""
	}
	micro := parts[1]
	major := sonameMajor(soname)
	if major == "" {
		return "", ""
	}
	// 编码是 minor*100：1400 → 4.14，2400 → 4.24；主版本取自 soname
	// （libgtk-4.so.1 → 4，libadwaita-1.so.0 → 1），不靠猜。
	return major + "." + strconv.Itoa(enc/100) + "." + micro, "库文件名"
}

// sonameMajor 从 soname 取主版本：libgtk-4.so.1 → "4"，libadwaita-1.so.0 → "1"，
// libgobject-2.0.so.0 → "2"（glib 系把主版本写成 "2.0"）。
func sonameMajor(soname string) string {
	head, _, _ := strings.Cut(soname, ".so")
	i := strings.LastIndex(head, "-")
	if i < 0 || i+1 >= len(head) {
		return ""
	}
	suffix := head[i+1:]
	if j := strings.Index(suffix, "."); j >= 0 {
		suffix = suffix[:j]
	}
	if _, err := strconv.Atoi(suffix); err != nil {
		return ""
	}
	return suffix
}

// versionLess 比较点分版本号：a < b 返回 true。非数字段按 0 处理。
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func runOut(name string, args ...string) string {
	b, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(b)
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

var _ = context.Background
