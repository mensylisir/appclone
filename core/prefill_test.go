package core

import (
	"path/filepath"
	"testing"
)

// 内置模板只作预填：路径补进候选，分类仍由 Classify 决定；已有候选不重复，
// 公共目录与凭证不塞进可勾选候选（它们各有归宿）。
func TestPrefillCandidates(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	// 测试环境把 XDG 根放在假 HOME 之外，`~/.config` 会落到"顶层点目录"灰区；
	// 用 resolver 真实的 XDG 根构造路径，才能测到"配置类默认勾选"这条规则。
	cfg := filepath.Join(r.XDG["CONFIG_HOME"], "PrefillApp")
	keyring := filepath.Join(r.XDG["DATA_HOME"], "keyrings")
	tmpl := Profile{Paths: []string{cfg, keyring}}

	got := r.PrefillCandidates(tmpl, nil)
	if len(got) != 1 {
		t.Fatalf("只应补出配置类路径（keyring 不进口径），得到 %+v", got)
	}
	if got[0].Path != cfg {
		t.Fatalf("候选路径应为展开后的绝对路径，得到 %q", got[0].Path)
	}
	if got[0].Class != ClassConfig || !got[0].Checked {
		t.Fatalf("配置类路径应默认勾选：%+v", got[0])
	}

	// 探测已经看到的路径不能重复补一遍（否则确认页会出现两行同一个目录）。
	if extra := r.PrefillCandidates(tmpl, got); len(extra) != 0 {
		t.Fatalf("已有候选不应重复预填：%+v", extra)
	}
}

// 模板路径落在公共目录上时，预填不能绕过分类把它塞成可勾选项。
func TestPrefillCandidatesSkipsPublicDirs(t *testing.T) {
	e := testEnv(t)
	r := e.Resolver
	pubs := r.PublicDirs()
	if len(pubs) == 0 {
		t.Skip("该环境没有公共目录定义")
	}
	got := r.PrefillCandidates(Profile{Paths: pubs}, nil)
	if len(got) != 0 {
		t.Fatalf("公共目录不能进预填候选：%+v", got)
	}
}
