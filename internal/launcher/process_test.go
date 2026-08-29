package launcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/hairpin/internal/config"
	"github.com/rxbynerd/hairpin/internal/job"
)

// dumpEnvScript writes an executable shell script that records its
// environment and working directory, then exits successfully.
func dumpEnvScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-stirrup.sh")
	script := "#!/bin/sh\nenv > env.out\npwd > cwd.out\ntouch done\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// waitForFile polls until path exists (or a deadline passes) and
// returns its contents.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

func findWorkDir(t *testing.T, jobID string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "hairpin-"+jobID+"*"))
	if err != nil {
		t.Fatalf("glob temp dir: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one temp dir for %s, got %v", jobID, matches)
	}
	return matches[0]
}

func TestProcessLaunchUsesPerJobTempDir(t *testing.T) {
	script := dumpEnvScript(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := config.ProcessConfig{StirrupBin: script, InheritEnv: false}
	p := NewProcess(cfg, "127.0.0.1:8130", logger)

	j := &job.Job{ID: job.NewID()}
	if err := p.Launch(context.Background(), j); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	workDir := findWorkDir(t, j.ID)
	waitForFile(t, filepath.Join(workDir, "done"))
	env := waitForFile(t, filepath.Join(workDir, "env.out"))
	if !strings.Contains(env, "CONTROL_PLANE_ADDR=127.0.0.1:8130\n") {
		t.Errorf("env.out missing CONTROL_PLANE_ADDR:\n%s", env)
	}
	if !strings.Contains(env, "CONTROL_PLANE_SESSION_ID="+j.ID+"\n") {
		t.Errorf("env.out missing CONTROL_PLANE_SESSION_ID:\n%s", env)
	}

	cwd := waitForFile(t, filepath.Join(workDir, "cwd.out"))
	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatalf("resolve work dir: %v", err)
	}
	if strings.TrimSpace(cwd) != resolvedWorkDir {
		t.Errorf("cwd = %q, want %q", strings.TrimSpace(cwd), resolvedWorkDir)
	}

	for _, name := range []string{"harness.stdout.log", "harness.stderr.log"} {
		if _, err := os.Stat(filepath.Join(workDir, name)); err != nil {
			t.Errorf("expected log file %s: %v", name, err)
		}
	}
}

func TestProcessLaunchUsesExplicitWorkDir(t *testing.T) {
	script := dumpEnvScript(t)
	workDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := config.ProcessConfig{StirrupBin: script, WorkDir: workDir}
	p := NewProcess(cfg, "127.0.0.1:8130", logger)

	j := &job.Job{ID: job.NewID()}
	if err := p.Launch(context.Background(), j); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	waitForFile(t, filepath.Join(workDir, "done"))
	if _, err := os.Stat(filepath.Join(workDir, "harness.stdout.log")); err != nil {
		t.Errorf("expected stdout log in explicit work dir: %v", err)
	}
}

func TestProcessLaunchInheritsEnv(t *testing.T) {
	script := dumpEnvScript(t)
	t.Setenv("HAIRPIN_TEST_INHERIT_MARKER", "present")
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	t.Run("inherited", func(t *testing.T) {
		workDir := t.TempDir()
		cfg := config.ProcessConfig{StirrupBin: script, WorkDir: workDir, InheritEnv: true}
		p := NewProcess(cfg, "127.0.0.1:8130", logger)
		j := &job.Job{ID: job.NewID()}
		if err := p.Launch(context.Background(), j); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		waitForFile(t, filepath.Join(workDir, "done"))
		env := waitForFile(t, filepath.Join(workDir, "env.out"))
		if !strings.Contains(env, "HAIRPIN_TEST_INHERIT_MARKER=present\n") {
			t.Errorf("expected inherited env var, got:\n%s", env)
		}
	})

	t.Run("not inherited", func(t *testing.T) {
		workDir := t.TempDir()
		cfg := config.ProcessConfig{StirrupBin: script, WorkDir: workDir, InheritEnv: false}
		p := NewProcess(cfg, "127.0.0.1:8130", logger)
		j := &job.Job{ID: job.NewID()}
		if err := p.Launch(context.Background(), j); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		waitForFile(t, filepath.Join(workDir, "done"))
		env := waitForFile(t, filepath.Join(workDir, "env.out"))
		if strings.Contains(env, "HAIRPIN_TEST_INHERIT_MARKER") {
			t.Errorf("did not expect inherited env var, got:\n%s", env)
		}
	})
}
