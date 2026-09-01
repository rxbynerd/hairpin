package service

import (
	"fmt"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/memory"
)

// validateControlPlaneTools refuses a RunConfig declaring a
// control-plane tool hairpin cannot answer. The harness registers each
// entry as an async tool and blocks on tool_result_response for the
// per-call timeout, so an undeclared name would cost the model that
// wait on every call. Stirrup remains the authority on everything else
// about an entry: name syntax, schema shape, and timeouts.
func validateControlPlaneTools(tools []*harnessv1.ControlPlaneToolConfig, memoryEnabled bool) error {
	for _, t := range tools {
		name := t.GetName()
		if !memory.IsMemoryTool(name) {
			return fmt.Errorf("hairpin does not fulfil control-plane tool %q; only %s and %s are supported: %w",
				name, memory.ToolSearch, memory.ToolSave, ErrInvalidArgument)
		}
		if !memoryEnabled {
			return fmt.Errorf("control-plane tool %q needs a memory backend, but hairpin was started without -billet-addr: %w",
				name, ErrInvalidArgument)
		}
	}
	return nil
}
