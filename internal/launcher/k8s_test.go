package launcher

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/config"
	"github.com/rxbynerd/hairpin/internal/job"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func runConfigJSON(t *testing.T, timeoutSeconds int32) string {
	t.Helper()
	rc := &harnessv1.RunConfig{
		RunId:   "hp-test",
		Mode:    "execution",
		Prompt:  "do the thing",
		Timeout: proto.Int32(timeoutSeconds),
	}
	data, err := protojson.Marshal(rc)
	if err != nil {
		t.Fatalf("marshal run config: %v", err)
	}
	return string(data)
}

func TestK8sLaunchCreatesJob(t *testing.T) {
	client := fake.NewSimpleClientset()
	cfg := config.K8sConfig{
		Namespace:               "hairpin",
		Image:                   "example.com/stirrup:latest",
		ServiceAccount:          "stirrup-harness",
		EnvFromSecrets:          []string{"provider-keys", "other-secret"},
		TTLSecondsAfterFinished: 3600,
		ActiveDeadlineSlack:     10 * time.Minute,
		Labels:                  map[string]string{"team": "agents"},
	}
	l := NewK8sWithClient(client, cfg, "hairpin.hairpin.svc:8130", testLogger())

	j := &job.Job{ID: job.NewID(), RunConfigJSON: runConfigJSON(t, 1800)}
	if err := l.Launch(context.Background(), j); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	created, err := client.BatchV1().Jobs("hairpin").Get(context.Background(), j.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created job: %v", err)
	}

	if created.Name != j.ID {
		t.Errorf("Name = %q, want %q", created.Name, j.ID)
	}

	wantLabels := map[string]string{
		"app.kubernetes.io/name":       "stirrup",
		"app.kubernetes.io/managed-by": "hairpin",
		"hairpin.rxbynerd.dev/job-id":  j.ID,
		"team":                         "agents",
	}
	for k, v := range wantLabels {
		if created.Labels[k] != v {
			t.Errorf("Job label %s = %q, want %q", k, created.Labels[k], v)
		}
		if created.Spec.Template.Labels[k] != v {
			t.Errorf("Pod label %s = %q, want %q", k, created.Spec.Template.Labels[k], v)
		}
	}

	if got := created.Spec.BackoffLimit; got == nil || *got != 0 {
		t.Errorf("BackoffLimit = %v, want 0", got)
	}
	if created.Spec.Template.Spec.RestartPolicy != "Never" {
		t.Errorf("RestartPolicy = %q, want Never", created.Spec.Template.Spec.RestartPolicy)
	}
	if got := created.Spec.TTLSecondsAfterFinished; got == nil || *got != 3600 {
		t.Errorf("TTLSecondsAfterFinished = %v, want 3600", got)
	}
	if got := created.Spec.ActiveDeadlineSeconds; got == nil || *got != 1800+600 {
		t.Errorf("ActiveDeadlineSeconds = %v, want %d", got, 1800+600)
	}

	if created.Spec.Template.Spec.ServiceAccountName != "stirrup-harness" {
		t.Errorf("ServiceAccountName = %q, want stirrup-harness", created.Spec.Template.Spec.ServiceAccountName)
	}
	sc := created.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("expected RunAsNonRoot true, got %+v", sc)
	}
	amt := created.Spec.Template.Spec.AutomountServiceAccountToken
	if amt == nil || *amt {
		t.Errorf("expected AutomountServiceAccountToken false, got %v", amt)
	}

	if len(created.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected exactly one container, got %d", len(created.Spec.Template.Spec.Containers))
	}
	container := created.Spec.Template.Spec.Containers[0]
	if container.Name != "stirrup" {
		t.Errorf("container name = %q, want stirrup", container.Name)
	}
	if container.Image != "example.com/stirrup:latest" {
		t.Errorf("container image = %q, want example.com/stirrup:latest", container.Image)
	}
	if len(container.Args) != 1 || container.Args[0] != "job" {
		t.Errorf("container args = %v, want [job]", container.Args)
	}

	wantEnv := map[string]string{
		"CONTROL_PLANE_ADDR":       "hairpin.hairpin.svc:8130",
		"CONTROL_PLANE_SESSION_ID": j.ID,
	}
	gotEnv := map[string]string{}
	for _, e := range container.Env {
		gotEnv[e.Name] = e.Value
	}
	for k, v := range wantEnv {
		if gotEnv[k] != v {
			t.Errorf("env %s = %q, want %q", k, gotEnv[k], v)
		}
	}

	if len(container.EnvFrom) != 2 {
		t.Fatalf("expected 2 envFrom entries, got %d", len(container.EnvFrom))
	}
	for i, name := range []string{"provider-keys", "other-secret"} {
		ref := container.EnvFrom[i].SecretRef
		if ref == nil || ref.Name != name {
			t.Errorf("envFrom[%d] = %+v, want secretRef %q", i, container.EnvFrom[i], name)
		}
	}
}

func TestK8sLaunchFallsBackOnUnparseableRunConfig(t *testing.T) {
	client := fake.NewSimpleClientset()
	cfg := config.K8sConfig{
		Namespace:           "hairpin",
		Image:               "example.com/stirrup:latest",
		ActiveDeadlineSlack: 10 * time.Minute,
	}
	l := NewK8sWithClient(client, cfg, "hairpin.hairpin.svc:8130", testLogger())

	j := &job.Job{ID: job.NewID(), RunConfigJSON: "not json"}
	if err := l.Launch(context.Background(), j); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	created, err := client.BatchV1().Jobs("hairpin").Get(context.Background(), j.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created job: %v", err)
	}
	want := int64(fallbackTimeoutSeconds + 600)
	if got := created.Spec.ActiveDeadlineSeconds; got == nil || *got != want {
		t.Errorf("ActiveDeadlineSeconds = %v, want %d", got, want)
	}
}

func TestK8sLaunchIsIdempotentOnAlreadyExists(t *testing.T) {
	cfg := config.K8sConfig{
		Namespace: "hairpin",
		Image:     "example.com/stirrup:latest",
	}
	jobID := job.NewID()
	existing := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobID, Namespace: "hairpin"},
	}
	client := fake.NewSimpleClientset(existing)
	l := NewK8sWithClient(client, cfg, "hairpin.hairpin.svc:8130", testLogger())

	j := &job.Job{ID: jobID, RunConfigJSON: runConfigJSON(t, 60)}
	if err := l.Launch(context.Background(), j); err != nil {
		t.Fatalf("Launch on already-existing job should succeed, got: %v", err)
	}
}
