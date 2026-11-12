package core

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestMergeExecArgsEvidenceWins(t *testing.T) {
	base := Profile{Exec: "/opt/qoder", ExecArgs: []string{"--old"}}
	p := Profile{Exec: "/opt/qoder", ExecArgs: []string{"--new"}, execArgsFromEvidence: true}
	got := MergeNonPathFieldsWith(nil, p, base)
	if !slices.Equal(got.ExecArgs, []string{"--new"}) || !got.execArgsFromEvidence {
		t.Fatalf("证据参数必须优先：args=%q flag=%v", got.ExecArgs, got.execArgsFromEvidence)
	}
}

func TestMergeExecArgsPreservedWithoutEvidence(t *testing.T) {
	base := Profile{Exec: "/opt/qoder", ExecArgs: []string{"--old"}}
	p := Profile{Exec: "/opt/qoder", ExecArgs: []string{"--desktop"}}
	got := MergeNonPathFieldsWith(nil, p, base)
	if !slices.Equal(got.ExecArgs, []string{"--old"}) {
		t.Fatalf("无证据且同 exec 应沿用旧参数：%q", got.ExecArgs)
	}
	if got.execArgsFromEvidence {
		t.Fatal("沿用旧参数不能标成证据来源")
	}
}

func TestMergeExecArgsDifferentExec(t *testing.T) {
	base := Profile{Exec: "/opt/old", ExecArgs: []string{"--old"}}
	p := Profile{Exec: "/opt/new", ExecArgs: []string{"--desktop"}}
	got := MergeNonPathFieldsWith(nil, p, base)
	if !slices.Equal(got.ExecArgs, []string{"--desktop"}) {
		t.Fatalf("exec 变化时不能继承旧参数：%q", got.ExecArgs)
	}
}

func TestMergeExecArgsEmptyProbeExec(t *testing.T) {
	base := Profile{Exec: "/opt/qoder", ExecArgs: []string{"--old"}, execArgsFromEvidence: true}
	p := Profile{}
	got := MergeNonPathFieldsWith(nil, p, base)
	if got.Exec != "/opt/qoder" || !slices.Equal(got.ExecArgs, []string{"--old"}) || !got.execArgsFromEvidence {
		t.Fatalf("探测没给 exec 时应整体继承并带标志：%#v", got)
	}
}

func TestDeriveProfileKeepsExecArgsEvidenceThroughSecondMerge(t *testing.T) {
	e := testEnv(t)
	cfg := filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "Demo")
	cands := []Candidate{{Path: cfg, Class: ClassConfig, Checked: true}}
	probe := Profile{
		App: "demo", Exec: "/opt/qoder", ExecArgs: []string{"--ozone-platform=x11"},
		execArgsFromEvidence: true,
	}
	base := Profile{Exec: "/opt/qoder", ExecArgs: []string{"--old"}}
	p := e.Resolver.DeriveProfile(probe, &base, "overlay", ReadTraceOK, cands)
	if !slices.Equal(p.ExecArgs, []string{"--ozone-platform=x11"}) || !p.execArgsFromEvidence {
		t.Fatalf("二次 merge 后证据参数/标志丢失：args=%q flag=%v", p.ExecArgs, p.execArgsFromEvidence)
	}
}

func TestFinalizeProbeExecArgsPreservedWarning(t *testing.T) {
	e := testEnv(t)
	cfg := filepath.Join(e.Resolver.XDG["CONFIG_HOME"], "Demo")
	cands := []Candidate{{Path: cfg, Class: ClassConfig, Checked: true}}
	old := Profile{Exec: "/opt/qoder", ExecArgs: []string{"--ozone-platform=x11"}}

	// 无证据：旧参数被沿用，必须告警。
	res := ProbeResult{
		Profile:    Profile{App: "demo", Exec: "/opt/qoder", ExecArgs: []string{"--desktop"}},
		Candidates: cands, ReadTrace: ReadTraceOK,
	}
	p, _, _, err := e.FinalizeProbe("demo", res, ProbeFinalize{Existing: old, HasExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.Warnings, "exec_args_preserved") {
		t.Fatalf("无证据沿用旧参数应告警：%v", p.Warnings)
	}
	if !slices.Equal(p.ExecArgs, old.ExecArgs) {
		t.Fatalf("旧参数应被保留：%q", p.ExecArgs)
	}

	// 有证据：不能告警。
	res.Profile.execArgsFromEvidence = true
	res.Profile.ExecArgs = []string{"--ozone-platform=x11", "--new"}
	p, _, _, err = e.FinalizeProbe("demo", res, ProbeFinalize{Existing: old, HasExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.Warnings, "exec_args_preserved") {
		t.Fatalf("有证据时不应告警：%v", p.Warnings)
	}
	if !slices.Equal(p.ExecArgs, []string{"--ozone-platform=x11", "--new"}) {
		t.Fatalf("证据参数应胜出：%q", p.ExecArgs)
	}
}
