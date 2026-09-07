package service

import harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"

// applyTraceEmitterDefault points a submitted RunConfig at the
// collector the harness should export its run trace to, when the config
// names no emitter of its own. A profile that sets trace_emitter keeps
// it. The protocol is left unset so the harness applies its own default
// of grpc.
func applyTraceEmitterDefault(cfg *harnessv1.RunConfig, endpoint string) {
	if endpoint == "" || cfg.GetTraceEmitter() != nil {
		return
	}
	cfg.TraceEmitter = &harnessv1.TraceEmitterConfig{
		Type:     "otel",
		Endpoint: endpoint,
	}
}
