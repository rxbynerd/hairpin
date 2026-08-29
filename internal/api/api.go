// Package api serves hairpin.v1.JobService over connect-go. It is a
// thin shim: request/response mapping and error codes only, with every
// operation delegated to internal/service.
package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	hairpinv1 "github.com/rxbynerd/hairpin/gen/hairpin/v1"
	"github.com/rxbynerd/hairpin/gen/hairpin/v1/hairpinv1connect"
	"github.com/rxbynerd/hairpin/internal/service"
	"github.com/rxbynerd/hairpin/internal/store"
)

// Default and maximum page sizes for ListJobs.
const (
	defaultListLimit = 50
	maxListLimit     = 100
)

// Server implements hairpinv1connect.JobServiceHandler.
type Server struct {
	svc *service.Service
}

var _ hairpinv1connect.JobServiceHandler = (*Server)(nil)

// New returns a JobService handler backed by svc.
func New(svc *service.Service) *Server { return &Server{svc: svc} }

func (s *Server) SubmitJob(ctx context.Context, req *connect.Request[hairpinv1.SubmitJobRequest]) (*connect.Response[hairpinv1.SubmitJobResponse], error) {
	j, err := s.svc.Submit(ctx, service.SubmitParams{
		Prompt:        req.Msg.GetPrompt(),
		Profile:       req.Msg.GetProfile(),
		RunConfigJSON: req.Msg.GetRunConfigJson(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&hairpinv1.SubmitJobResponse{
		Job: jobToProto(j),
		// The one place the bearer session leaves hairpin: the submitter
		// needs it only when running harnesses out-of-band (launcher
		// "none").
		HarnessSession: j.SessionString(),
	}), nil
}

func (s *Server) GetJob(ctx context.Context, req *connect.Request[hairpinv1.GetJobRequest]) (*connect.Response[hairpinv1.GetJobResponse], error) {
	j, err := s.svc.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&hairpinv1.GetJobResponse{Job: jobToProto(j)}), nil
}

func (s *Server) ListJobs(ctx context.Context, req *connect.Request[hairpinv1.ListJobsRequest]) (*connect.Response[hairpinv1.ListJobsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	jobs, next, err := s.svc.List(ctx, limit, req.Msg.GetPageToken())
	if err != nil {
		return nil, connectError(err)
	}
	resp := &hairpinv1.ListJobsResponse{NextPageToken: next}
	for _, j := range jobs {
		resp.Jobs = append(resp.Jobs, jobToProto(j))
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) WatchJob(ctx context.Context, req *connect.Request[hairpinv1.WatchJobRequest], stream *connect.ServerStream[hairpinv1.WatchJobResponse]) error {
	events, err := s.svc.Watch(ctx, req.Msg.GetId(), req.Msg.GetAfterId())
	if err != nil {
		return connectError(err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := stream.Send(&hairpinv1.WatchJobResponse{Event: eventToProto(ev)}); err != nil {
				return err
			}
		}
	}
}

func (s *Server) CancelJob(ctx context.Context, req *connect.Request[hairpinv1.CancelJobRequest]) (*connect.Response[hairpinv1.CancelJobResponse], error) {
	j, err := s.svc.Cancel(ctx, req.Msg.GetId())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&hairpinv1.CancelJobResponse{Job: jobToProto(j)}), nil
}

func (s *Server) ListPermissionRequests(ctx context.Context, req *connect.Request[hairpinv1.ListPermissionRequestsRequest]) (*connect.Response[hairpinv1.ListPermissionRequestsResponse], error) {
	perms, err := s.svc.ListPermissions(ctx, req.Msg.GetJobId())
	if err != nil {
		return nil, connectError(err)
	}
	resp := &hairpinv1.ListPermissionRequestsResponse{}
	for _, p := range perms {
		resp.PermissionRequests = append(resp.PermissionRequests, permissionToProto(p))
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) AnswerPermission(ctx context.Context, req *connect.Request[hairpinv1.AnswerPermissionRequest]) (*connect.Response[hairpinv1.AnswerPermissionResponse], error) {
	p, err := s.svc.AnswerPermission(ctx, req.Msg.GetJobId(), req.Msg.GetRequestId(), req.Msg.GetAllow(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&hairpinv1.AnswerPermissionResponse{PermissionRequest: permissionToProto(p)}), nil
}

// connectError maps service errors onto connect codes, preserving the
// original message.
func connectError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, service.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, service.ErrNotConnected):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
