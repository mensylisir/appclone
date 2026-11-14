package core

import (
	"context"
	"io"
	"time"
)

// runProbeRound reports activity without walking the write layer or copying the
// trace. Joining the ticker before returning keeps callbacks ordered across
// rounds, cancellation and automatic retries. CLI callers without a progress
// callback keep the direct runner path.
func runProbeRound(ctx context.Context, runner SessionRunner, argv []string, stderr io.Writer,
	opt ProbeOptions, trace *probeReadState, round, rounds int) (SessionResult, error) {
	if opt.Progress == nil {
		return runner.Run(ctx, argv, stderr)
	}
	if err := ctx.Err(); err != nil {
		return SessionResult{}, err
	}
	start := time.Now()
	report := func(stage string) {
		opt.Progress(ProbeProgress{
			Stage: stage, Round: round + 1, Rounds: rounds,
			Elapsed: time.Since(start), Reads: trace.readsSoFar(),
			ReadAvailable: trace != nil && trace.readOK,
		})
	}
	report("round_running")
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() == nil {
					report("round_running")
				}
			}
		}
	}()
	res, err := runner.Run(ctx, argv, stderr)
	close(stop)
	<-done
	if err == nil && ctx.Err() == nil {
		report("round_stopped")
	}
	return res, err
}
