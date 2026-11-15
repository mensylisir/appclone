package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type startAwareTestRunner struct {
	run func(context.Context, func(SessionProcess) error) (SessionResult, error)
}

func (r startAwareTestRunner) Run(ctx context.Context, _ []string, _ io.Writer) (SessionResult, error) {
	return r.run(ctx, nil)
}
func (r startAwareTestRunner) RunWithStart(ctx context.Context, _ []string, _ io.Writer, start func(SessionProcess) error) (SessionResult, error) {
	return r.run(ctx, start)
}

func TestProbeControlTokenAndCancelPriority(t *testing.T) {
	for _, parentStops := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish", true: "cancel-wins"}[parentStops], func(t *testing.T) {
			control := &ProbeControl{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var old uint64
			for round := 0; round < 2; round++ {
				opt := ProbeOptions{Control: control, Progress: func(p ProbeProgress) {
					if p.CanFinish {
						if old != 0 && control.Finish(old) {
							t.Error("stale token accepted")
						}
						if !control.Finish(p.SessionToken) || control.Finish(p.SessionToken) {
							t.Error("finish not idempotent")
						}
						old = p.SessionToken
						if parentStops {
							cancel()
						}
					}
				}}
				r := startAwareTestRunner{run: func(ctx context.Context, start func(SessionProcess) error) (SessionResult, error) {
					if control.Finish(old) {
						t.Error("enabled before Start")
					}
					if err := start(SessionProcess{}); err != nil {
						return SessionResult{}, err
					}
					<-ctx.Done()
					return SessionResult{Started: true}, ctx.Err()
				}}
				res, err := runProbeRound(ctx, r, nil, nil, opt, nil, round, 2)
				if parentStops {
					if !errors.Is(err, context.Canceled) || res.ManualFinish {
						t.Fatalf("parent lost: %+v %v", res, err)
					}
					break
				}
				if err != nil || !res.ManualFinish {
					t.Fatalf("finish lost: %+v %v", res, err)
				}
				if control.Finish(old) {
					t.Fatal("token survived completion")
				}
			}
		})
	}
}

func TestProbeControlConcurrentDoubleClick(t *testing.T) {
	c := &ProbeControl{}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	token := c.begin(cancel)
	var wg sync.WaitGroup
	accepted := make(chan bool, 50)
	for range 50 {
		wg.Go(func() { accepted <- c.Finish(token) })
	}
	wg.Wait()
	close(accepted)
	n := 0
	for ok := range accepted {
		if ok {
			n++
		}
	}
	if n != 1 || !errors.Is(context.Cause(ctx), errFinishProbeRound) {
		t.Fatalf("accepted=%d", n)
	}
	c.end(token)
}

func TestManualFinishPreservesWrittenEvidence(t *testing.T) {
	root := t.TempDir()
	control := &ProbeControl{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	marker := filepath.Join(root, "synthetic-evidence")
	opt := ProbeOptions{Control: control, Progress: func(p ProbeProgress) {
		if p.CanFinish {
			go func(token uint64) {
				for ctx.Err() == nil {
					if _, err := os.Stat(marker); err == nil {
						control.Finish(token)
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}(p.SessionToken)
		}
	}}
	res, err := runProbeRound(ctx, setsidRunner{grace: 50 * time.Millisecond, waitLimit: time.Second}, []string{"/bin/sh", "-c", `printf synthetic > "$1"; sleep 20`, "test", marker}, io.Discard, opt, &probeReadState{}, 0, 1)
	if err != nil || !res.ManualFinish || res.Unreaped {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("written evidence lost", err)
	}
	if !sessionGroupGone(res.Process.PGID) {
		t.Fatal("orphan group")
	}
}

func TestProbeUnknownStartCannotFinish(t *testing.T) {
	c := &ProbeControl{}
	r := &fakeRunner{}
	opt := ProbeOptions{Control: c, Progress: func(p ProbeProgress) {
		if p.CanFinish || p.SessionToken != 0 {
			t.Error("non-start-aware runner enabled Finish")
		}
	}}
	if _, err := runProbeRound(context.Background(), r, nil, nil, opt, nil, 0, 1); err != nil {
		t.Fatal(err)
	}
}

func TestUnreapedProbePreservesSceneAndBlocksRetry(t *testing.T) {
	e := probeEnv(t)
	m := &fakeMounter{}
	runner := startAwareTestRunner{run: func(ctx context.Context, start func(SessionProcess) error) (SessionResult, error) {
		p, err := identifySessionProcess(os.Getpid(), os.Getpid())
		if err != nil {
			return SessionResult{}, err
		}
		if err := start(p); err != nil {
			return SessionResult{}, err
		}
		return SessionResult{Started: true, Unreaped: true, Process: p}, errors.New("synthetic wait failure")
	}}
	_, err := e.Probe(context.Background(), "held", ProbeOptions{Command: []string{"/synthetic/app"}, Mounter: m, Runner: runner})
	if ProbeErrorKey(err) != "err.probe.session_unreaped" {
		t.Fatal(err)
	}
	dirs, _ := filepath.Glob(filepath.Join(e.tmpRoot(), "appclone-probe-*"))
	t.Cleanup(func() {
		for _, d := range dirs {
			unreapedProbes.Delete(d)
		}
	})
	if len(dirs) != 1 || !m.mounted || !probeSceneActive(dirs[0]) {
		t.Fatalf("scene discarded: %v mounted=%v", dirs, m.mounted)
	}
	other := &fakeRunner{}
	_, err = e.Probe(context.Background(), "retry", ProbeOptions{Command: []string{"/synthetic/app"}, Mounter: m, Runner: other})
	if ProbeErrorKey(err) != "err.probe.session_unreaped" || other.calls != 0 {
		t.Fatal("retry bypassed live session")
	}
	if err := e.sweepStaleProbes(); ProbeErrorKey(err) != "err.probe.session_unreaped" {
		t.Fatalf("sweep bypassed record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "upper")); err != nil {
		t.Fatal("sweep removed active scene", err)
	}
}

func TestManualFinishEmptyReadsIsPartialWithoutRecovery(t *testing.T) {
	for _, writes := range []bool{false, true} {
		e := probeEnv(t)
		m := &fakeMounter{}
		if writes {
			m.entries = []string{".config/TestApp/state"}
		}
		r := &fifoRunner{res: SessionResult{Started: true, ManualFinish: true, ExitCode: 143}, lines: []string{`1 execve("/synthetic/app", ["app"], 0x0) = 0`}}
		res, err := e.Probe(context.Background(), "manual", ProbeOptions{Command: []string{"/synthetic/app"}, Runner: r, Mounter: m, Rounds: 1})
		if err != nil || r.calls != 1 || res.ReadTrace != ReadTracePartial || containsString(res.ReadsWarns, "strace_failed") || !containsString(res.ReadsWarns, "manual_finish") {
			t.Fatalf("writes=%v err=%v calls=%d trace=%s warns=%v", writes, err, r.calls, res.ReadTrace, res.ReadsWarns)
		}
	}
}

func TestProbeFinishLosesToTimeout(t *testing.T) {
	c := &ProbeControl{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r := startAwareTestRunner{run: func(ctx context.Context, start func(SessionProcess) error) (SessionResult, error) {
		if err := start(SessionProcess{}); err != nil {
			return SessionResult{}, err
		}
		<-ctx.Done()
		time.Sleep(60 * time.Millisecond)
		return SessionResult{Started: true}, ctx.Err()
	}}
	res, err := runProbeRound(ctx, r, nil, nil, ProbeOptions{Control: c, Progress: func(p ProbeProgress) {
		if p.CanFinish {
			c.Finish(p.SessionToken)
		}
	}}, nil, 0, 1)
	if !errors.Is(err, context.DeadlineExceeded) || res.ManualFinish {
		t.Fatalf("timeout lost: %+v %v", res, err)
	}
}
