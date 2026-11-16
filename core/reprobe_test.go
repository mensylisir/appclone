package core

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func reprobeResolver(t *testing.T) *PathResolver {
	t.Helper()
	home := t.TempDir()
	return newResolver(home, home, 1000, func(string) string { return "" })
}

func gateCand(path string, checked bool) Candidate {
	return Candidate{Path: path, Class: ClassConfig, Checked: checked}
}

func TestDroppedIsolatedPaths(t *testing.T) {
	r := reprobeResolver(t)
	home := r.Home

	t.Run("同路径仍被覆盖", func(t *testing.T) {
		got := DroppedIsolatedPaths(r, []string{"~/.config/app"}, []Candidate{gateCand("~/.config/app", true)})
		if len(got) != 0 {
			t.Fatalf("同路径不应算 dropped：%v", got)
		}
	})

	t.Run("候选是父目录仍被覆盖", func(t *testing.T) {
		old := []string{"~/.config/app/cache"}
		cands := []Candidate{gateCand("~/.config/app", true)}
		if got := DroppedIsolatedPaths(r, old, cands); len(got) != 0 {
			t.Fatalf("父目录候选应算覆盖：%v", got)
		}
	})

	t.Run("候选是子目录不算覆盖", func(t *testing.T) {
		got := DroppedIsolatedPaths(r, []string{"~/.config/app"}, []Candidate{gateCand("~/.config/app/private", true)})
		if !reflect.DeepEqual(got, []string{"~/.config/app"}) {
			t.Fatalf("旧路径整条失去隔离时必须报 dropped：%v", got)
		}
	})

	t.Run("相邻目录 app 与 app2 不互相覆盖", func(t *testing.T) {
		got := DroppedIsolatedPaths(r, []string{"~/.config/app"}, []Candidate{gateCand("~/.config/app2", true)})
		if len(got) != 1 {
			t.Fatalf("app2 不能覆盖 app：%v", got)
		}
	})

	t.Run("软链两侧按真实路径覆盖", func(t *testing.T) {
		realDir := filepath.Join(home, ".config", "real")
		if err := os.MkdirAll(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(home, ".config", "link")
		if err := os.Symlink(realDir, link); err != nil {
			t.Skipf("无法创建软链：%v", err)
		}
		// 旧路径经软链、候选是真实目录。
		if got := DroppedIsolatedPaths(r, []string{"~/.config/link/sub"},
			[]Candidate{gateCand("~/.config/real", true)}); len(got) != 0 {
			t.Fatalf("真实路径候选应覆盖软链旧路径：%v", got)
		}
		// 旧路径是真实目录、候选经软链。
		if got := DroppedIsolatedPaths(r, []string{"~/.config/real/sub"},
			[]Candidate{gateCand("~/.config/link", true)}); len(got) != 0 {
			t.Fatalf("软链候选应覆盖真实旧路径：%v", got)
		}
	})

	t.Run("旧公共路径不再隔离时必须报出", func(t *testing.T) {
		old := []string{"~/Downloads/export"}
		cands := []Candidate{{
			Path: "~/Downloads/export", Class: ClassConfig, DisplayClass: ClassPublic,
			Checked: false,
		}}
		got := DroppedIsolatedPaths(r, old, cands)
		if !reflect.DeepEqual(got, old) {
			t.Fatalf("公共候选不能算覆盖：%v", got)
		}
	})

	t.Run("未勾选候选不算覆盖", func(t *testing.T) {
		got := DroppedIsolatedPaths(r, []string{"~/.config/app"}, []Candidate{gateCand("~/.config/app", false)})
		if len(got) != 1 {
			t.Fatalf("未勾选候选不能让旧路径静默消失：%v", got)
		}
	})

	t.Run("无法展开的旧路径保守算 dropped", func(t *testing.T) {
		old := []string{"relative/app"}
		got := DroppedIsolatedPaths(r, old, []Candidate{gateCand(old[0], true)})
		if !reflect.DeepEqual(got, old) {
			t.Fatalf("无法展开时应保守报出：%v", got)
		}
	})
}

func TestBaselineIsolatedPaths(t *testing.T) {
	r := reprobeResolver(t)
	cands := []Candidate{
		gateCand("~/.config/app", true),
		{Path: "~/Downloads/export", Class: ClassConfig, DisplayClass: ClassPublic},
		gateCand("~/.cache/app", false),
	}
	got := BaselineIsolatedPaths(r, []string{"~/.config/app", "~/.local/share/app", "~/.config/app"}, cands)
	want := []string{"~/.config/app", "~/.local/share/app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("基线应为旧 paths + 已勾选可进门禁候选（去重）：got=%v want=%v", got, want)
	}

	// 档案 paths 缺失时，当前确认页已勾选的候选仍要进基线，避免误报。
	got = BaselineIsolatedPaths(r, nil, cands)
	if !reflect.DeepEqual(got, []string{"~/.config/app"}) {
		t.Fatalf("没有旧 paths 时应以已勾选候选为基线：%v", got)
	}
}

// 软链是常态（ADR 与 §6.4）：旧档案存 `~/...`，探测候选是解析后的真实路径。
// 同一目录的两种写法不得让基线/范围变化出现两条。
func TestReprobePathDedupAcrossSymlinks(t *testing.T) {
	home := t.TempDir()
	r := newResolver(home, home, 1000, func(string) string { return "" })
	cfgReal := filepath.Join(home, "cfg-real")
	if err := os.MkdirAll(cfgReal, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cfgReal, filepath.Join(home, ".config")); err != nil {
		t.Skipf("无法创建软链：%v", err)
	}
	realPath := filepath.Join(cfgReal, "app")

	t.Run("基线同一目录只保留一条", func(t *testing.T) {
		got := BaselineIsolatedPaths(r, []string{"~/.config/app"}, []Candidate{gateCand(realPath, true)})
		if len(got) != 1 || got[0] != "~/.config/app" {
			t.Fatalf("同一目录的两种写法应去重并保留档案形态：%v", got)
		}
	})

	t.Run("范围变化同一目录只报一条", func(t *testing.T) {
		got := DroppedIsolatedPaths(r, []string{"~/.config/app", realPath},
			[]Candidate{gateCand(realPath, false)})
		if len(got) != 1 || got[0] != "~/.config/app" {
			t.Fatalf("同一目录的两种写法只应报一条：%v", got)
		}
	})
}
