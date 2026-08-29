package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/redis/go-redis/v9"

	"github.com/rxbynerd/hairpin/internal/store"
)

func (r *redisStore) PutPermission(ctx context.Context, jobID string, p store.PermissionRequest) error {
	ok, err := r.jobExists(ctx, jobID)
	if err != nil {
		return fmt.Errorf("put permission %s/%s: %w", jobID, p.RequestID, err)
	}
	if !ok {
		return notFoundf("job %s", jobID)
	}

	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("put permission %s/%s: %w", jobID, p.RequestID, err)
	}
	if err := r.client.HSet(ctx, permsKey(jobID), p.RequestID, data).Err(); err != nil {
		return fmt.Errorf("put permission %s/%s: %w", jobID, p.RequestID, err)
	}
	return nil
}

func (r *redisStore) GetPermission(ctx context.Context, jobID, requestID string) (store.PermissionRequest, error) {
	data, err := r.client.HGet(ctx, permsKey(jobID), requestID).Result()
	if errors.Is(err, redis.Nil) {
		return store.PermissionRequest{}, notFoundf("permission %s/%s", jobID, requestID)
	}
	if err != nil {
		return store.PermissionRequest{}, fmt.Errorf("get permission %s/%s: %w", jobID, requestID, err)
	}
	var p store.PermissionRequest
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return store.PermissionRequest{}, fmt.Errorf("get permission %s/%s: %w", jobID, requestID, err)
	}
	return p, nil
}

func (r *redisStore) ListPermissions(ctx context.Context, jobID string) ([]store.PermissionRequest, error) {
	fields, err := r.client.HGetAll(ctx, permsKey(jobID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list permissions job %s: %w", jobID, err)
	}
	out := make([]store.PermissionRequest, 0, len(fields))
	for _, data := range fields {
		var p store.PermissionRequest
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, fmt.Errorf("list permissions job %s: %w", jobID, err)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, k int) bool {
		if (out[i].State == store.PermissionPending) != (out[k].State == store.PermissionPending) {
			return out[i].State == store.PermissionPending
		}
		return out[i].RequestedAt.Before(out[k].RequestedAt)
	})
	return out, nil
}
