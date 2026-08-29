// Package launcher abstracts how a stirrup harness is started for a
// job. The Kubernetes implementation creates one batch/v1 Job per run
// from the stirrup harness image; None covers harnesses started
// out-of-band.
package launcher

import (
	"context"

	"github.com/rxbynerd/hairpin/internal/job"
)

// Launcher starts one harness per job.
//
// Contract: the harness must be started with CONTROL_PLANE_ADDR set to
// hairpin's advertise address and CONTROL_PLANE_SESSION_ID set to j.ID
// — the harness echoes the session ID in its ready event, which is the
// only correlation between a launched workload and an inbound stream.
// Launch returns once the workload has been handed to the substrate
// (Job created / process started), not once the harness connects.
type Launcher interface {
	Launch(ctx context.Context, j *job.Job) error
}

// None is a Launcher that starts nothing. Used when harnesses are
// launched out-of-band (an operator running the stirrup harness image
// by hand against hairpin's address with the job ID as session ID).
type None struct{}

func (None) Launch(context.Context, *job.Job) error { return nil }
