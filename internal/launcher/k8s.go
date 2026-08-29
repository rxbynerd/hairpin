package launcher

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/protobuf/encoding/protojson"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/config"
	"github.com/rxbynerd/hairpin/internal/job"
)

// fallbackTimeoutSeconds bounds activeDeadlineSeconds when the stored
// RunConfig's timeout can't be read (should not happen for jobs that
// passed submission validation, but a Job must still get a deadline).
const fallbackTimeoutSeconds = 3600

// K8s launches one batch/v1 Job per hairpin job.
type K8s struct {
	client        kubernetes.Interface
	cfg           config.K8sConfig
	advertiseAddr string
	logger        *slog.Logger
}

// NewK8s builds a K8s launcher, resolving a client from in-cluster
// config, then cfg.Kubeconfig, then $KUBECONFIG / the default
// kubeconfig loading rules. It errors if none of those produce a
// usable client.
func NewK8s(cfg config.K8sConfig, advertiseAddr string, logger *slog.Logger) (*K8s, error) {
	restCfg, err := buildRestConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("launcher: build kubernetes client config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("launcher: build kubernetes clientset: %w", err)
	}
	return NewK8sWithClient(client, cfg, advertiseAddr, logger), nil
}

// NewK8sWithClient builds a K8s launcher around an existing client,
// bypassing kubeconfig resolution — for tests, using a fake clientset.
func NewK8sWithClient(client kubernetes.Interface, cfg config.K8sConfig, advertiseAddr string, logger *slog.Logger) *K8s {
	if logger == nil {
		logger = slog.Default()
	}
	return &K8s{client: client, cfg: cfg, advertiseAddr: advertiseAddr, logger: logger}
}

func buildRestConfig(cfg config.K8sConfig) (*rest.Config, error) {
	if rc, err := rest.InClusterConfig(); err == nil {
		return rc, nil
	}
	if cfg.Kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// Launch creates a batch/v1 Job running `stirrup job` for j. A
// create that fails with AlreadyExists (a retried launch racing a Job
// that already landed) is treated as success.
func (l *K8s) Launch(ctx context.Context, j *job.Job) error {
	labels := l.jobLabels(j.ID)
	ttl := l.cfg.TTLSecondsAfterFinished
	activeDeadline := l.activeDeadlineSeconds(j)
	backoffLimit := int32(0)
	automountToken := false
	runAsNonRoot := true

	k8sJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      j.ID,
			Namespace: l.cfg.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &activeDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           l.cfg.ServiceAccount,
					AutomountServiceAccountToken: &automountToken,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
					},
					Containers: []corev1.Container{
						{
							Name:  "stirrup",
							Image: l.cfg.Image,
							Args:  []string{"job"},
							Env: []corev1.EnvVar{
								{Name: "CONTROL_PLANE_ADDR", Value: l.advertiseAddr},
								{Name: "CONTROL_PLANE_SESSION_ID", Value: j.ID},
							},
							EnvFrom: envFromSecrets(l.cfg.EnvFromSecrets),
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := l.client.BatchV1().Jobs(l.cfg.Namespace).Create(ctx, k8sJob, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			l.logger.Debug("launcher: k8s job already exists, treating launch as successful", "job_id", j.ID)
			return nil
		}
		return fmt.Errorf("launcher: create k8s job: %w", err)
	}
	return nil
}

func (l *K8s) jobLabels(jobID string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/name":       "stirrup",
		"app.kubernetes.io/managed-by": "hairpin",
		"hairpin.rxbynerd.dev/job-id":  jobID,
	}
	for k, v := range l.cfg.Labels {
		labels[k] = v
	}
	return labels
}

// activeDeadlineSeconds derives the Job's activeDeadlineSeconds from
// the RunConfig's wall-clock timeout plus configured slack, falling
// back to fallbackTimeoutSeconds + slack when the timeout can't be
// read from j.RunConfigJSON.
func (l *K8s) activeDeadlineSeconds(j *job.Job) int64 {
	slackSeconds := int64(l.cfg.ActiveDeadlineSlack.Seconds())
	timeoutSeconds, err := runConfigTimeoutSeconds(j.RunConfigJSON)
	if err != nil {
		l.logger.Warn("launcher: could not read RunConfig timeout, using fallback deadline",
			"job_id", j.ID, "error", err)
		timeoutSeconds = fallbackTimeoutSeconds
	}
	return timeoutSeconds + slackSeconds
}

func runConfigTimeoutSeconds(runConfigJSON string) (int64, error) {
	if runConfigJSON == "" {
		return 0, fmt.Errorf("empty run config")
	}
	var rc harnessv1.RunConfig
	if err := protojson.Unmarshal([]byte(runConfigJSON), &rc); err != nil {
		return 0, fmt.Errorf("unmarshal run config: %w", err)
	}
	if rc.Timeout == nil {
		return 0, fmt.Errorf("run config has no timeout")
	}
	return int64(rc.GetTimeout()), nil
}

func envFromSecrets(names []string) []corev1.EnvFromSource {
	if len(names) == 0 {
		return nil
	}
	out := make([]corev1.EnvFromSource, len(names))
	for i, name := range names {
		out[i] = corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
			},
		}
	}
	return out
}
