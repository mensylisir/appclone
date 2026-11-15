package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var errFinishProbeRound = errors.New("finish current probe round")
var probeTokenSequence atomic.Uint64

// ProbeControl ends only the currently started round. Tokens are never reused,
// including across retries and different configuration pages.
type ProbeControl struct {
	mu     sync.Mutex
	token  uint64
	finish context.CancelCauseFunc
}

func (c *ProbeControl) begin(finish context.CancelCauseFunc) uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = probeTokenSequence.Add(1)
	c.finish = finish
	return c.token
}

// Finish requests collection of the evidence already written in this round.
// False means the token is stale, the application has not started, or a request
// was already accepted. It never cancels the parent probe context.
func (c *ProbeControl) Finish(token uint64) bool {
	if c == nil || token == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != token || c.finish == nil {
		return false
	}
	c.finish(errFinishProbeRound)
	c.finish = nil
	return true
}

func (c *ProbeControl) state() (uint64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, c.finish != nil
}

func (c *ProbeControl) end(token uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token, c.finish = 0, nil
	}
}

// CanFinish lets views reject queued progress from an already ended round.
func (c *ProbeControl) CanFinish(token uint64) bool {
	current, enabled := c.state()
	return token != 0 && current == token && enabled
}
