package launcher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rxbynerd/hairpin/internal/config"
	"github.com/rxbynerd/hairpin/internal/job"
)

// Process launches a local `stirrup job` subprocess per job, for
// development and end-to-end tests. It never runs on a real cluster.
type Process struct {
	cfg           config.ProcessConfig
	advertiseAddr string
	logger        *slog.Logger
}

// NewProcess builds a subprocess launcher. advertiseAddr becomes
// CONTROL_PLANE_ADDR for every launched harness.
func NewProcess(cfg config.ProcessConfig, advertiseAddr string, logger *slog.Logger) *Process {
	if logger == nil {
		logger = slog.Default()
	}
	return &Process{cfg: cfg, advertiseAddr: advertiseAddr, logger: logger}
}

// Launch starts `<StirrupBin> job` and returns once it has been
// started; it does not wait for the process to exit. The harness's own
// lifecycle (assignment timeout, RunConfig timeout) bounds its
// runtime — hairpin never kills it.
func (p *Process) Launch(ctx context.Context, j *job.Job) error {
	workDir := p.cfg.WorkDir
	if workDir == "" {
		dir, err := os.MkdirTemp("", "hairpin-"+j.ID)
		if err != nil {
			return fmt.Errorf("launcher: create work dir: %w", err)
		}
		workDir = dir
	} else if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("launcher: create work dir: %w", err)
	}

	stdout, err := os.OpenFile(filepath.Join(workDir, "harness.stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("launcher: open stdout log: %w", err)
	}
	stderr, err := os.OpenFile(filepath.Join(workDir, "harness.stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		stdout.Close()
		return fmt.Errorf("launcher: open stderr log: %w", err)
	}

	env := []string{
		"CONTROL_PLANE_ADDR=" + p.advertiseAddr,
		"CONTROL_PLANE_SESSION_ID=" + j.ID,
	}
	if p.cfg.InheritEnv {
		env = append(os.Environ(), env...)
	}

	cmd := exec.Command(p.cfg.StirrupBin, "job")
	cmd.Dir = workDir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		stdout.Close()
		stderr.Close()
		return fmt.Errorf("launcher: start harness process: %w", err)
	}

	go p.reap(cmd, j.ID, stdout, stderr)
	return nil
}

func (p *Process) reap(cmd *exec.Cmd, jobID string, stdout, stderr *os.File) {
	err := cmd.Wait()
	stdout.Close()
	stderr.Close()
	if err != nil {
		p.logger.Warn("launcher: harness process exited with error", "job_id", jobID, "error", err)
		return
	}
	p.logger.Debug("launcher: harness process exited", "job_id", jobID)
}
