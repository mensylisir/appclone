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

	"github.com/mensylisir/appclone/core"
)

type check struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
}

func newDoctorCmd(a *app) *cobra.Command {
	var strict bool
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
	return c
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
		if _, err := os.Stat("/dev/fuse"); err == nil {
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

	if st, dt := gtkCheck("gtk4", "4.14"); true {
		add("gtk4", st, dt)
	}
	if st, dt := gtkCheck("libadwaita-1", "1.5"); true {
		add("libadwaita", st, dt)
	}

	// 数据目录可写：MkdirAll 成功但真正写不进去（只读挂载）也要在这里暴露。
	probe := filepath.Join(a.env.Base, ".doctor-write-test")
	if err := os.MkdirAll(a.env.Base, 0o700); err != nil {
		add("state-dir", "fail", "数据目录不可创建："+a.env.Base)
	} else if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		add("state-dir", "fail", "数据目录不可写："+a.env.Base)
	} else {
		_ = os.Remove(probe)
		add("state-dir", "ok", a.env.Base)
	}
	return out
}

// gtkCheck 用 pkg-config 查版本，CLI 二进制绝不 import GUI 包：
// 纯 CLI 机器上 import 会让 init 阶段 panic（§6 doctor 条目）。
func gtkCheck(pkg, min string) (string, string) {
	p, err := exec.LookPath("pkg-config")
	if err != nil {
		return "warn", "没有 pkg-config，无法确认 " + pkg + " 版本（GUI 才需要）"
	}
	v := strings.TrimSpace(runOut(p, "--modversion", pkg))
	if v == "" {
		return "warn", pkg + " 未安装（只影响 GUI）"
	}
	// 版本必须按数字段比较：字典序下 "1.10.0" < "1.5" 会误报低版本。
	if versionLess(v, min) {
		return "warn", pkg + " " + v + " 低于要求的 " + min
	}
	return "ok", pkg + " " + v
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
