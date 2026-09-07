package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

// maxUpdateRetries bounds UpdateJob's optimistic-CAS retry loop.
const maxUpdateRetries = 30

// casBackoff is the per-attempt jittered delay between lost CAS races,
// keeping contended retries from immediately colliding again.
const casBackoff = 2 * time.Millisecond

// jobSetFields returns the hash fields to HSET for j: every scalar field
// plus any non-zero time field.
func jobSetFields(j *job.Job) map[string]any {
	f := map[string]any{
		"id":               j.ID,
		"status":           string(j.Status),
		"prompt":           j.Prompt,
		"profile":          j.Profile,
		"run_config_json":  j.RunConfigJSON,
		"stop_reason":      j.StopReason,
		"error":            j.Error,
		"final_text":       j.FinalText,
		"cancel_requested": boolField(j.CancelRequested),
		"harness_token":    j.HarnessToken,
		"trace_parent":     j.TraceParent,
		"repo_scope":       repoScopeField(j.RepoScope),
	}
	if !j.CreatedAt.IsZero() {
		f["created_at"] = j.CreatedAt.Format(time.RFC3339Nano)
	}
	if !j.StartedAt.IsZero() {
		f["started_at"] = j.StartedAt.Format(time.RFC3339Nano)
	}
	if !j.FinishedAt.IsZero() {
		f["finished_at"] = j.FinishedAt.Format(time.RFC3339Nano)
	}
	if !j.LastEventAt.IsZero() {
		f["last_event_at"] = j.LastEventAt.Format(time.RFC3339Nano)
	}
	return f
}

// jobClearFields returns the time fields to HDEL because j's value is
// zero (HSET has no way to represent absence).
func jobClearFields(j *job.Job) []string {
	var clear []string
	if j.CreatedAt.IsZero() {
		clear = append(clear, "created_at")
	}
	if j.StartedAt.IsZero() {
		clear = append(clear, "started_at")
	}
	if j.FinishedAt.IsZero() {
		clear = append(clear, "finished_at")
	}
	if j.LastEventAt.IsZero() {
		clear = append(clear, "last_event_at")
	}
	return clear
}

func boolField(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// repoScopeField encodes a job's repo scope as a JSON array, or "" for
// an empty scope so the hash field stays absent-looking.
func repoScopeField(scope []string) string {
	if len(scope) == 0 {
		return ""
	}
	// []string of already-validated entries: encoding cannot fail.
	b, _ := json.Marshal(scope)
	return string(b)
}

func jobFromFields(fields map[string]string) (*job.Job, error) {
	j := &job.Job{
		ID:              fields["id"],
		Status:          job.Status(fields["status"]),
		Prompt:          fields["prompt"],
		Profile:         fields["profile"],
		RunConfigJSON:   fields["run_config_json"],
		StopReason:      fields["stop_reason"],
		Error:           fields["error"],
		FinalText:       fields["final_text"],
		CancelRequested: fields["cancel_requested"] == "1",
		HarnessToken:    fields["harness_token"],
		TraceParent:     fields["trace_parent"],
	}
	if v := fields["repo_scope"]; v != "" {
		if err := json.Unmarshal([]byte(v), &j.RepoScope); err != nil {
			return nil, fmt.Errorf("field repo_scope: %w", err)
		}
	}
	var err error
	for name, dst := range map[string]*time.Time{
		"created_at":    &j.CreatedAt,
		"started_at":    &j.StartedAt,
		"finished_at":   &j.FinishedAt,
		"last_event_at": &j.LastEventAt,
	} {
		if *dst, err = parseTimeField(fields, name); err != nil {
			return nil, err
		}
	}
	return j, nil
}

func parseTimeField(fields map[string]string, name string) (time.Time, error) {
	v := fields[name]
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("field %s: %w", name, err)
	}
	return t, nil
}

func (r *redisStore) CreateJob(ctx context.Context, j *job.Job) error {
	key := jobKey(j.ID)
	txErr := r.client.Watch(ctx, func(tx *redis.Tx) error {
		exists, err := tx.Exists(ctx, key).Result()
		if err != nil {
			return err
		}
		if exists != 0 {
			return store.ErrConflict
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, key, jobSetFields(j))
			pipe.ZAdd(ctx, jobsIndexKey, redis.Z{Score: 0, Member: j.ID})
			return nil
		})
		return err
	}, key)
	switch {
	case txErr == nil:
		return nil
	case errors.Is(txErr, store.ErrConflict), errors.Is(txErr, redis.TxFailedErr):
		return conflictf("job %s", j.ID)
	default:
		return fmt.Errorf("create job %s: %w", j.ID, txErr)
	}
}

func (r *redisStore) GetJob(ctx context.Context, id string) (*job.Job, error) {
	fields, err := r.client.HGetAll(ctx, jobKey(id)).Result()
	if err != nil {
		return nil, fmt.Errorf("job %s: %w", id, err)
	}
	if len(fields) == 0 {
		return nil, notFoundf("job %s", id)
	}
	return jobFromFields(fields)
}

func (r *redisStore) ListJobs(ctx context.Context, limit int, pageToken string) ([]*job.Job, string, error) {
	if limit <= 0 {
		limit = 50
	}
	start := "+"
	if pageToken != "" {
		start = "(" + pageToken
	}
	ids, err := r.client.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key:   jobsIndexKey,
		Start: start,
		Stop:  "-",
		ByLex: true,
		Rev:   true,
		Count: int64(limit) + 1,
	}).Result()
	if err != nil {
		return nil, "", fmt.Errorf("list jobs: %w", err)
	}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}

	out := make([]*job.Job, 0, len(ids))
	for _, id := range ids {
		j, err := r.GetJob(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			// Index and job hash raced (e.g. concurrent create); skip.
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("list jobs: %w", err)
		}
		out = append(out, j)
	}

	next := ""
	if hasMore {
		next = ids[len(ids)-1]
	}
	return out, next, nil
}

func (r *redisStore) UpdateJob(ctx context.Context, id string, fn func(*job.Job) error) (*job.Job, error) {
	key := jobKey(id)
	for attempt := 0; attempt < maxUpdateRetries; attempt++ {
		var result *job.Job
		txErr := r.client.Watch(ctx, func(tx *redis.Tx) error {
			fields, err := tx.HGetAll(ctx, key).Result()
			if err != nil {
				return err
			}
			if len(fields) == 0 {
				return store.ErrNotFound
			}
			j, err := jobFromFields(fields)
			if err != nil {
				return err
			}
			if err := fn(j); err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.HSet(ctx, key, jobSetFields(j))
				if clear := jobClearFields(j); len(clear) > 0 {
					pipe.HDel(ctx, key, clear...)
				}
				return nil
			})
			if err != nil {
				return err
			}
			result = j
			return nil
		}, key)
		if txErr == nil {
			return result, nil
		}
		if errors.Is(txErr, redis.TxFailedErr) {
			time.Sleep(time.Duration(rand.Int63n(int64(casBackoff))) + casBackoff/2)
			continue
		}
		if errors.Is(txErr, store.ErrNotFound) {
			return nil, notFoundf("job %s", id)
		}
		return nil, txErr
	}
	return nil, conflictf("job %s: update lost the race %d times", id, maxUpdateRetries)
}

// DeleteJob removes the job hash, its event stream, its permissions
// hash, and its index entry in one pipeline.
func (r *redisStore) DeleteJob(ctx context.Context, id string) error {
	ok, err := r.jobExists(ctx, id)
	if err != nil {
		return fmt.Errorf("delete job %s: %w", id, err)
	}
	if !ok {
		return notFoundf("job %s", id)
	}
	if _, err := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, jobKey(id), eventsKey(id), permsKey(id))
		pipe.ZRem(ctx, jobsIndexKey, id)
		return nil
	}); err != nil {
		return fmt.Errorf("delete job %s: %w", id, err)
	}
	return nil
}
