package core

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// All progress callbacks are serialized, including Start acknowledgement and the
// heartbeat. The token is valid only while this exact round owns the process.
func runProbeRound(ctx context.Context, runner SessionRunner, argv []string, stderr io.Writer,
	opt ProbeOptions, trace *probeReadState, round, rounds int) (SessionResult, error) {
	if err := ctx.Err(); err != nil {
		return SessionResult{}, err
	}
	roundCtx, finish := context.WithCancelCause(ctx)
	defer finish(nil)
	start := time.Now()
	var reportMu sync.Mutex
	report := func(stage string) {
		if opt.Progress == nil {
			return
		}
		reportMu.Lock()
		defer reportMu.Unlock()
		token, enabled := opt.Control.state()
		opt.Progress(ProbeProgress{
			Stage: stage, Round: round + 1, Rounds: rounds,
			Elapsed: time.Since(start), Reads: trace.readsSoFar(),
			ReadAvailable: trace != nil && trace.readOK,
			SessionToken:  token, CanFinish: enabled,
		})
	}
	if err := writeProbeActive(opt.scene, true, nil); err != nil {
		return SessionResult{}, errEnv("err.probe.session_state", nil, err)
	}
	var token uint64
	defer func() { opt.Control.end(token) }()
	onStart := func(p SessionProcess) error {
		if err := writeProbeActive(opt.scene, false, &p); err != nil {
			return errEnv("err.probe.session_state", nil, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		token = opt.Control.begin(finish)
		report("round_running")
		return nil
	}
	report("round_running")
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		if opt.Progress == nil {
			return
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-roundCtx.Done():
				return
			case <-ticker.C:
				if roundCtx.Err() == nil {
					report("round_running")
				}
			}
		}
	}()
	var res SessionResult
	var err error
	if aware, ok := runner.(StartAwareSessionRunner); ok {
		res, err = aware.RunWithStart(roundCtx, argv, stderr, onStart)
	} else {
		res, err = runner.Run(roundCtx, argv, stderr)
	}
	close(stop)
	<-done
	opt.Control.end(token)
	if res.Unreaped {
		if opt.work != "" {
			unreapedProbes.Store(opt.work, true)
		}
		return res, errEnv("err.probe.session_unreaped", map[string]any{"path": opt.work}, err)
	}
	if werr := writeProbeActive(opt.scene, false, nil); werr != nil {
		return res, errEnv("err.probe.session_state", nil, werr)
	}
	// Parent cancellation and timeout win even if the Finish click arrived first.
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if res.Started && errors.Is(context.Cause(roundCtx), errFinishProbeRound) && errors.Is(err, context.Canceled) {
		res.ManualFinish = true
		err = nil
	}
	if err == nil {
		report("round_stopped")
	}
	return res, err
}
