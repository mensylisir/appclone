//go:build linux && gtk_e2e

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
)

// TestGtkSoakRenderList 是长稳用例：反复整页重建列表（旧实现每轮泄漏 C 侧
// 对象，Go 堆看不出来），持续采样进程 RSS。默认 SKIP，只有显式给出
// APPCLONE_GTK_SOAK_MIN 才运行：
//
//	APPCLONE_GTK_TEST=1 APPCLONE_GTK_SOAK_MIN=30 \
//	  go test -tags gtk_e2e -count=1 -timeout 35m -run TestGtkSoakRenderList \
//	  ./cmd/appclone-gui -v
//
// -count=1 与 -timeout 必须显式给：默认 10 分钟超时撑不住长稳，缓存命中则会
// 让重复运行拿到旧的 PASS。日常手动入口用 design/soak.sh（它已带好参数）。
//
// 预热 60 秒后取基线；结束时的 RSS 增长必须不超过
// APPCLONE_GTK_SOAK_MAX_MB（默认 64 MiB）。阈值是防"无界增长"的护栏，
// 不是精确内存配额——不同发行版的 malloc/GPU 驱动会有正常波动。判据只看
// 最终稳态相对基线的增长；峰值只作诊断（短时尖峰不等于泄漏）。
// 假 HOME 与隔离 AC_BASE 由 gtkTestEnv 提供，不碰真实主目录。
func TestGtkSoakRenderList(t *testing.T) {
	mins, _ := strconv.Atoi(os.Getenv("APPCLONE_GTK_SOAK_MIN"))
	if mins <= 0 {
		t.Skip("长稳用例需要 APPCLONE_GTK_SOAK_MIN（分钟）显式开启")
	}
	maxMB, _ := strconv.ParseInt(os.Getenv("APPCLONE_GTK_SOAK_MAX_MB"), 10, 64)
	if maxMB <= 0 {
		maxMB = 64
	}

	env, appsDir := gtkTestEnv(t, "soak")
	// 60 个应用：让每轮重建真的创建/释放大量控件，而不是只跑两次循环。
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("app%02d.desktop", i)
		body := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=App %02d\nExec=/usr/bin/true\n", i)
		if err := os.WriteFile(filepath.Join(appsDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	app := adw.NewApplication("io.github.mensylisir.Appclone.GtkSoak",
		gio.GApplicationNonUniqueValue)
	defer app.Unref()

	u := &ui{app: app, env: env, lang: "zh_CN"}
	var runErr error
	activated := false
	started := time.Now()
	warmup := 60 * time.Second
	// 配置的分钟数是"预热之后"的测量时长；预热本身不计入。
	deadline := warmup + time.Duration(mins)*time.Minute
	var baseline, last, peak int64
	iter, lastLog := 0, time.Time{}
	fail := func(format string, a ...any) {
		if runErr == nil {
			runErr = fmt.Errorf(format, a...)
		}
		app.Quit()
	}

	actcb := func(_ gio.Application) {
		if u.win != nil {
			return
		}
		activated = true
		u.buildWindow()
		u.win.Present()
		var tick glib.SourceFunc
		tick = func(uintptr) bool {
			if runErr != nil {
				return false
			}
			if !u.loaded || !u.win.GetMapped() {
				if time.Since(started) > 30*time.Second {
					fail("首轮加载 30 秒内没有完成（loaded=%t mapped=%t）", u.loaded, u.win.GetMapped())
					return false
				}
				return true
			}
			// 交替空/非空查询：搜索过滤走的是另一条渲染分支，也要一起长稳。
			if iter%2 == 0 {
				u.query = "app0"
			} else {
				u.query = ""
			}
			u.renderList()
			iter++
			now := time.Now()
			// 先采样再判截止：否则最后一拍可能正好越过 deadline，基线永远建不起来。
			// 采样失败必须直接失败：把读不到当成 0 会让"最终 0 - 基线"变成负增长，
			// 泄漏被静默判成通过。
			sample, serr := rssBytes()
			if serr != nil {
				fail("RSS 采样失败，无法判断内存是否稳定：%v", serr)
				return false
			}
			last = sample
			if now.Sub(started) >= warmup {
				if baseline == 0 {
					baseline = last
					lastLog = now // 基线这一拍已经打印过，别让同拍再打一条"进度"。
					t.Logf("预热结束：iter=%d rss=%d MiB", iter, baseline>>20)
				}
				if last > peak {
					peak = last
				}
			}
			if now.Sub(lastLog) >= time.Minute && baseline > 0 {
				lastLog = now
				t.Logf("进度 %s：iter=%d rss=%d MiB（基线 %d MiB，峰值 %d MiB）",
					now.Sub(started).Round(time.Second), iter, last>>20, baseline>>20, peak>>20)
			}
			if now.Sub(started) > deadline {
				app.Quit()
				return false
			}
			return true
		}
		glib.TimeoutAdd(20, &tick, 0)
	}
	app.ConnectActivate(&actcb)
	u.hold(&actcb)

	if code := app.Run(1, os.Args[:1]); code != 0 {
		t.Fatalf("GTK 主循环退出码 %d", code)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !activated || iter == 0 {
		t.Fatalf("长稳用例没有跑起来：activated=%t iter=%d", activated, iter)
	}
	if baseline == 0 {
		t.Fatalf("拿不到 RSS 采样（/proc/self/statm），无法判断内存是否稳定")
	}
	growth := last - baseline
	t.Logf("长稳结束：%s，iter=%d，基线 %d MiB，最终 %d MiB，峰值 %d MiB，增长 %d MiB",
		time.Since(started).Round(time.Second), iter, baseline>>20, last>>20, peak>>20, growth>>20)
	if growth > maxMB<<20 {
		t.Fatalf("RSS 增长 %d MiB 超过 %d MiB：列表重建可能在泄漏 C 侧对象", growth>>20, maxMB)
	}
}

// rssBytes 读 /proc/self/statm 的常驻页数。读不到/格式不对返回 error：
// 调用方必须把采样失败当成测试失败，而不是当成 0 字节。
func rssBytes() (int64, error) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, fmt.Errorf("/proc/self/statm 字段不足：%q", string(b))
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/self/statm 解析失败：%w", err)
	}
	return pages * int64(os.Getpagesize()), nil
}
