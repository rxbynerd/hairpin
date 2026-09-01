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
// hairpin's advertise address and CONTROL_PLANE_SESSION_ID set to
// j.SessionString(). The harness echoes that bearer session in its ready
// event, allowing hairpin to correlate and authenticate the stream.
// Launch returns once the workload has been handed to the substrate
// (the Kubernetes Job has been created), not once the harness connects.
type Launcher interface {
	Launch(ctx context.Context, j *job.Job) error
}

// None is a Launcher that starts nothing. It supports out-of-band
// harnesses started with the harness_session value returned by SubmitJob.
type None struct{}

func (None) Launch(context.Context, *job.Job) error { return nil }
