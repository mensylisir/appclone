package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mensylisir/appclone/core"
)

// cliEnv 用环境变量搭一个隔离的 core.Env（CLI 层就是这么拿环境的），
// 再把档案与实例直接写到盘上——列表/删除这些命令只读盘，不需要跑 add。
func cliEnv(t *testing.T) (*app, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, d := range []string{
		home,
		filepath.Join(root, "xdg", "config"),
		filepath.Join(root, "xdg", "data", "applications"),
		filepath.Join(root, "xdg", "state"),
		filepath.Join(root, "xdg", "cache"),
		filepath.Join(root, "run"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg", "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg", "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg", "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "xdg", "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("AC_BASE", filepath.Join(root, "xdg", "appclone"))

	env, err := core.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{env: env, lang: "zh_CN", json: true}
	return a, root
}

// captureStdout 把命令的输出抓回来：a.ok 直接写 os.Stdout（脚本就是这么消费的）。
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	ferr := fn()
	os.Stdout = old
	_ = w.Close()
	return <-done, ferr
}

// `list --json` 的外形是脚本契约（§11）：成功与失败同一外形，脚本只读 code/key。
func TestListJSONShape(t *testing.T) {
	a, root := cliEnv(t)
	base := filepath.Join(root, "xdg", "appclone")
	// 只放档案 + 实例，不放 .desktop：孤儿也必须列出来。
	if err := os.MkdirAll(filepath.Join(base, "profiles.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "profiles.d", "demo.toml"),
		[]byte("version = 1\nexec = \"/usr/bin/true\"\npaths = [\"~/.config/Demo\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	instDir := filepath.Join(base, "instances", "demo", "main")
	if err := os.MkdirAll(filepath.Join(instDir, "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "instance.toml"),
		[]byte("version = 1\napp = \"demo\"\nid = \"main\"\ndisplay_name = \"工作号\"\ncreated_at = \"2026-11-07T00:00:00Z\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		cmd := newListCmd(a)
		cmd.SetArgs([]string{"--all"})
		return cmd.Execute()
	})
	if err != nil {
		t.Fatalf("list 失败：%v", err)
	}
	var payload struct {
		OK   bool `json:"ok"`
		Data struct {
			Apps []struct {
				App       string `json:"app"`
				Instances []struct {
					ID string `json:"id"`
				} `json:"instances"`
			} `json:"apps"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("输出不是合法 JSON：%v\n%s", err, out)
	}
	if !payload.OK {
		t.Fatalf("成功时 ok 必须为 true：%s", out)
	}
	found := false
	for _, ap := range payload.Data.Apps {
		if ap.App == "demo" && len(ap.Instances) == 1 && ap.Instances[0].ID == "main" {
			found = true
		}
	}
	if !found {
		t.Fatalf("孤儿实例应出现在 apps 里：%s", out)
	}
}

// 失败时也是 JSON，且必须带稳定的 code/key——脚本靠它分支，不许解析文案。
func TestRmUnknownInstanceJSONError(t *testing.T) {
	a, _ := cliEnv(t)
	out, err := captureStdout(t, func() error {
		cmd := newRmCmd(a)
		// 生产里由 root 的 SilenceUsage/SilenceErrors 兜住，单测直接执行子命令，
		// 不关掉的话 cobra 会往 stderr 打一份 usage，污染测试输出。
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetArgs([]string{"demo", "main"})
		return cmd.Execute()
	})
	if err == nil {
		t.Fatal("删除不存在的实例必须报错")
	}
	// 失败走 a.fail()，由 main 调用；这里直接验错误对象的分层。
	if code := core.ExitCode(err); code != core.CodeConflict {
		t.Fatalf("实例不存在应是冲突（5），得到 %d", code)
	}
	var ce *core.Error
	if !isCoreError(err, &ce) || ce.Key != "err.instance.not_found" {
		t.Fatalf("错误键应为 err.instance.not_found，得到 %v（输出 %q）", err, out)
	}
}

func isCoreError(err error, target **core.Error) bool {
	for err != nil {
		if e, ok := err.(*core.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// doctor 的 JSON 外形：checks 必须有 id/status/detail 三字段，脚本按 status 分支。
func TestDoctorJSONShape(t *testing.T) {
	a, _ := cliEnv(t)
	out, err := captureStdout(t, func() error {
		cmd := newDoctorCmd(a)
		cmd.SetArgs([]string{})
		return cmd.Execute()
	})
	if err != nil {
		t.Fatalf("doctor 失败：%v", err)
	}
	var payload struct {
		OK   bool `json:"ok"`
		Data struct {
			Checks []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Detail string `json:"detail"`
			} `json:"checks"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("输出不是合法 JSON：%v\n%s", err, out)
	}
	if !payload.OK || len(payload.Data.Checks) == 0 {
		t.Fatalf("doctor 应至少给出若干检查项：%s", out)
	}
	for _, c := range payload.Data.Checks {
		if c.ID == "" || c.Detail == "" {
			t.Fatalf("检查项缺字段：%+v", c)
		}
		switch c.Status {
		case "ok", "warn", "fail":
		default:
			t.Fatalf("status 只能是 ok/warn/fail，得到 %q", c.Status)
		}
	}
}

// add --json 的读证据字段是脚本契约：reads_warns 保持固定顺序，
// credential_scope_narrowed 携带被丢弃的旧凭据路径；空值不写键。
func TestAddProbeEvidenceShape(t *testing.T) {
	p := core.Profile{
		ReadTrace:         core.ReadTracePartial,
		ExecArgs:          []string{"--ozone-platform=x11"},
		CredentialPaths:   []string{"~/.config/App/credentials.json"},
		CredentialDropped: []string{"~/Downloads/token"},
		Warnings:          []string{"credential_scope_unsupported"},
	}
	payload := map[string]any{}
	addProbeEvidence(payload, []string{"parse_error", "no_fd_annotation"}, p, true, true, 2)
	warns, ok := payload["reads_warns"].([]string)
	if !ok || len(warns) != 2 || warns[0] != "parse_error" || warns[1] != "no_fd_annotation" {
		t.Fatalf("reads_warns 形状/顺序错：%#v", payload["reads_warns"])
	}
	dropped, ok := payload["credential_scope_narrowed"].([]string)
	if !ok || len(dropped) != 1 || dropped[0] != "~/Downloads/token" {
		t.Fatalf("credential_scope_narrowed 应携带丢弃路径：%#v", payload["credential_scope_narrowed"])
	}
	execArgs, ok := payload["exec_args"].([]string)
	if !ok || len(execArgs) != 1 || execArgs[0] != "--ozone-platform=x11" {
		t.Fatalf("exec_args 应携带自动预填参数：%#v", payload["exec_args"])
	}
	if unknown, ok := payload["self_relaunch_unknown"].(bool); !ok || !unknown {
		t.Fatalf("self_relaunch_unknown 应为 true：%#v", payload["self_relaunch_unknown"])
	}
	if amb, ok := payload["self_relaunch_ambiguous"].(bool); !ok || !amb {
		t.Fatalf("self_relaunch_ambiguous 应为 true：%#v", payload["self_relaunch_ambiguous"])
	}
	if dropped, ok := payload["round_marker_dropped"].(int); !ok || dropped != 2 {
		t.Fatalf("round_marker_dropped 应为 2：%#v", payload["round_marker_dropped"])
	}
	// 空值不写键：保持 --json 的 omit-empty 约定（ADR-036）。
	empty := map[string]any{}
	addProbeEvidence(empty, nil, core.Profile{}, false, false, 0)
	if len(empty) != 0 {
		t.Fatalf("空值不应写键：%#v", empty)
	}
}
