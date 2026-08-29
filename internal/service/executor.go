package service

import (
	"fmt"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/config"
)

// isK8sExecutor reports whether an executor type runs the agent in a
// sandbox Pod. "k8s" manages the Pod directly; "k8s-sandbox" provisions
// it through the Agent Sandbox CRD. Both consume the same K8s* fields.
func isK8sExecutor(t string) bool { return t == "k8s" || t == "k8s-sandbox" }

func validK8sRuntime(runtime string) bool {
	switch runtime {
	case "runc", "gvisor", "kata-qemu", "kata-fc", "kata-clh":
		return true
	default:
		return false
	}
}

// applyExecutorDefaults fills in the cluster coordinates of a sandbox
// executor from the server's configuration, so a caller names only the
// isolation it wants. A RunConfig that sets a field of its own keeps
// it.
func applyExecutorDefaults(cfg *harnessv1.RunConfig, d config.ExecutorDefaults) {
	ex := cfg.GetExecutor()
	if ex == nil || !isK8sExecutor(ex.GetType()) {
		return
	}
	if ex.Image == "" {
		ex.Image = d.Image
	}
	if ex.K8SNamespace == "" {
		ex.K8SNamespace = d.Namespace
	}
	if ex.K8SServiceAccount == "" {
		ex.K8SServiceAccount = d.ServiceAccount
	}
	if ex.Runtime == "" {
		ex.Runtime = d.Runtime
	}
}

// validateExecutor mirrors the cross-field rules stirrup applies to a
// sandbox executor, so a doomed Job is refused at submit rather than
// failing after the Pod has been scheduled. Called after defaults are
// applied.
func validateExecutor(ex *harnessv1.ExecutorConfig) error {
	if !isK8sExecutor(ex.GetType()) {
		return nil
	}
	kind := ex.GetType()
	if ex.GetImage() == "" {
		return fmt.Errorf("executor.image is required for executor.type=%q and no sandbox image default is configured: %w", kind, ErrInvalidArgument)
	}
	if ex.GetK8SNamespace() == "" {
		return fmt.Errorf("executor.k8sNamespace is required for executor.type=%q and no sandbox namespace default is configured: %w", kind, ErrInvalidArgument)
	}
	if ex.GetWorkspace() != "" {
		return fmt.Errorf("executor.workspace is not valid for executor.type=%q (the Pod workspace is fixed at /workspace): %w", kind, ErrInvalidArgument)
	}
	runtime := ex.GetRuntime()
	if kind == "k8s-sandbox" {
		if runtime != "" && runtime != "gvisor" {
			return fmt.Errorf("executor.runtime must be %q or empty for executor.type=%q, got %q: %w", "gvisor", kind, runtime, ErrInvalidArgument)
		}
	} else if runtime != "" && !validK8sRuntime(runtime) {
		return fmt.Errorf("unsupported executor.runtime %q for executor.type=%q: %w", runtime, kind, ErrInvalidArgument)
	}
	if ex.GetNetwork() == nil {
		return fmt.Errorf("executor.network is required for executor.type=%q (set mode to \"none\" or \"allowlist\"): %w", kind, ErrInvalidArgument)
	}
	switch ex.GetNetwork().GetMode() {
	case "allowlist":
		if ex.GetK8SEgressProxyUrl() == "" {
			return fmt.Errorf("executor.k8sEgressProxyUrl is required when executor.network.mode is \"allowlist\": %w", ErrInvalidArgument)
		}
	case "none":
		if ex.GetK8SEgressProxyUrl() != "" {
			return fmt.Errorf("executor.k8sEgressProxyUrl is only valid when executor.network.mode is \"allowlist\": %w", ErrInvalidArgument)
		}
	default:
		return fmt.Errorf("executor.network.mode must be \"none\" or \"allowlist\", got %q: %w", ex.GetNetwork().GetMode(), ErrInvalidArgument)
	}
	return nil
}
