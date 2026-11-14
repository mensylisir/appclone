package core

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"
)

func TestProbeProgressWhileApplicationIsRunning(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			trace := &probeReadState{readOK: available}
			if available {
				trace.reader = &traceReader{res: StraceResult{Reads: []string{".config/App/a", ".config/App/b"}}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			heartbeat := make(chan struct{}, 1)
			var events []ProbeProgress
			opt := ProbeOptions{Progress: func(p ProbeProgress) {
				events = append(events, p)
				if p.Stage == "round_running" && p.Elapsed >= time.Second {
					select {
					case heartbeat <- struct{}{}:
					default:
					}
				}
			}}
			runner := &fakeRunner{step: func(int) (SessionResult, error) {
				select {
				case <-heartbeat:
					return SessionResult{Started: true}, nil
				case <-ctx.Done():
					return SessionResult{}, ctx.Err()
				}
			}}
			res, err := runProbeRound(ctx, runner, nil, nil, opt, trace, 1, 2)
			if err != nil || !res.Started || len(events) < 3 {
				t.Fatalf("application must receive live progress before returning: %v, %+v", err, events)
			}
			if events[0].Stage != "round_running" || events[len(events)-1].Stage != "round_stopped" {
				t.Fatalf("events out of order: %+v", events)
			}
			for _, p := range events {
				if p.Round != 2 || p.Rounds != 2 || p.ReadAvailable != available || p.Entries != 0 {
					t.Fatalf("round/evidence metadata: %+v", p)
				}
				if available && p.Reads != 2 {
					t.Fatalf("reads: %+v", p)
				}
			}
		})
	}
}

func TestProbeProgressCancellationAndNextRound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var events []ProbeProgress
	opt := ProbeOptions{Progress: func(p ProbeProgress) { events = append(events, p) }}
	runner := &fakeRunner{step: func(int) (SessionResult, error) {
		cancel()
		return SessionResult{Started: true}, ctx.Err()
	}}
	if _, err := runProbeRound(ctx, runner, nil, nil, opt, nil, 0, 2); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Stage != "round_running" {
		t.Fatal(events)
	}
	// A new round must begin at zero elapsed time and end after its final tick.
	_, err := runProbeRound(context.Background(), &fakeRunner{}, nil, nil, opt, nil, 1, 2)
	if err != nil || len(events) != 3 || events[1].Round != 2 || events[1].Elapsed >= time.Second || events[2].Stage != "round_stopped" {
		t.Fatalf("next round: %v, %+v", err, events)
	}
	before := len(events)
	// Already canceled means no app launch and no spurious progress.
	_, _ = runProbeRound(ctx, runner, nil, nil, opt, nil, 0, 2)
	if len(events) != before || runner.calls != 1 {
		t.Fatal("canceled round ran")
	}
}

var probeReadCountSink int

// A progress update must not copy and sort an application's complete trace.
func BenchmarkProbeReadProgress(b *testing.B) {
	r := &traceReader{}
	for i := 100000; i > 0; i-- {
		r.res.Reads = append(r.res.Reads, fmt.Sprintf(".config/App/file-%06d", i))
	}
	st := &probeReadState{reader: r}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		probeReadCountSink = st.readsSoFar()
	}
}

// Run in a separate process on the same host for before/after comparisons.
// Includes the benchmark's own 10 Hz timer and the Go runtime's wakeups.
func BenchmarkTraceReaderIdle(b *testing.B) {
	r, err := newTraceReader(b.TempDir(), b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if _, err := r.stopAndResult(); err != nil {
			b.Fatal(err)
		}
	}()
	var before, after syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	seconds := time.Since(start).Seconds()
	b.StopTimer()
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		b.Fatal(err)
	}
	cpu := after.Utime.Nano() + after.Stime.Nano() - before.Utime.Nano() - before.Stime.Nano()
	b.ReportMetric(float64(cpu)/1e6/seconds, "cpu-ms/s")
	b.ReportMetric(float64(after.Nvcsw-before.Nvcsw)/seconds, "switches/s")
}
