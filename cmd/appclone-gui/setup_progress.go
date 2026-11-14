package main

import (
	"fmt"
	"time"

	"github.com/mensylisir/appclone/core"
)

// Called on the GTK thread. A completed page or an earlier probe must never
// receive a late heartbeat, including after retrying on the same page.
func (v *setupView) applyProbeProgress(seq uint64, pp core.ProbeProgress) {
	if v.done || v.state != "probing" || seq != v.probeSeq {
		return
	}
	switch pp.Stage {
	case "fallback":
		v.showDegraded()
	case "round_running":
		seconds := max(0, int(pp.Elapsed/time.Second))
		v.roundLabel.SetText(v.u.tr("ui.setup.probing.elapsed", map[string]any{
			"round": pp.Round, "rounds": pp.Rounds,
			"time": fmt.Sprintf("%02d:%02d", seconds/60, seconds%60),
		}))
		if pp.ReadAvailable {
			v.countLabel.SetText(v.u.tr("ui.setup.probing.reads_live", map[string]any{"r": pp.Reads}))
		} else {
			v.countLabel.SetText(v.u.tr("ui.setup.probing.writes_pending", nil))
		}
	case "round_stopped", "round_done":
		v.roundLabel.SetText(v.u.tr("ui.setup.probing.collecting", map[string]any{
			"round": pp.Round, "rounds": pp.Rounds,
		}))
		if pp.Stage == "round_done" {
			if pp.Reads > 0 {
				v.countLabel.SetText(v.u.tr("ui.setup.probing.count_reads",
					map[string]any{"n": pp.Entries, "r": pp.Reads}))
			} else {
				v.countLabel.SetText(v.u.tr("ui.setup.probing.count", map[string]any{"n": pp.Entries}))
			}
		}
	}
}
