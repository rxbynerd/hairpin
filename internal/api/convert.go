package api

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	hairpinv1 "github.com/rxbynerd/hairpin/gen/hairpin/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

func jobToProto(j *job.Job) *hairpinv1.Job {
	if j == nil {
		return nil
	}
	return &hairpinv1.Job{
		Id:          j.ID,
		Status:      statusToProto(j.Status),
		Prompt:      j.Prompt,
		Profile:     j.Profile,
		StopReason:  j.StopReason,
		Error:       j.Error,
		FinalText:   j.FinalText,
		CreatedAt:   timestampOrNil(j.CreatedAt),
		StartedAt:   timestampOrNil(j.StartedAt),
		FinishedAt:  timestampOrNil(j.FinishedAt),
		LastEventAt: timestampOrNil(j.LastEventAt),
	}
}

func statusToProto(s job.Status) hairpinv1.JobStatus {
	switch s {
	case job.StatusQueued:
		return hairpinv1.JobStatus_JOB_STATUS_QUEUED
	case job.StatusLaunching:
		return hairpinv1.JobStatus_JOB_STATUS_LAUNCHING
	case job.StatusAwaitingHarness:
		return hairpinv1.JobStatus_JOB_STATUS_AWAITING_HARNESS
	case job.StatusRunning:
		return hairpinv1.JobStatus_JOB_STATUS_RUNNING
	case job.StatusSucceeded:
		return hairpinv1.JobStatus_JOB_STATUS_SUCCEEDED
	case job.StatusFailed:
		return hairpinv1.JobStatus_JOB_STATUS_FAILED
	case job.StatusCancelled:
		return hairpinv1.JobStatus_JOB_STATUS_CANCELLED
	default:
		return hairpinv1.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}

func eventToProto(ev store.Event) *hairpinv1.JobEvent {
	return &hairpinv1.JobEvent{
		Id:          ev.ID,
		Type:        ev.Type,
		PayloadJson: ev.PayloadJSON,
		At:          timestampOrNil(ev.At),
	}
}

func permissionToProto(p store.PermissionRequest) *hairpinv1.PermissionRequest {
	return &hairpinv1.PermissionRequest{
		RequestId:   p.RequestID,
		ToolName:    p.ToolName,
		InputJson:   p.InputJSON,
		State:       string(p.State),
		Reason:      p.Reason,
		RequestedAt: timestampOrNil(p.RequestedAt),
		AnsweredAt:  timestampOrNil(p.AnsweredAt),
	}
}

// timestampOrNil leaves unset times absent on the wire rather than
// sending the zero instant.
func timestampOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
