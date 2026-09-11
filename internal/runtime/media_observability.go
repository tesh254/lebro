package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// RecordMediaAttempt writes bounded media provenance into the existing attempt
// repository. IDs deduplicate terminal deliveries. No prompt or transcript is
// stored. Direct calls get a synthetic run ID scoped to the operation.
func RecordMediaAttempt(ctx context.Context, repo ModelAttemptRepository, info MediaResultInfo, kind string, started, finished time.Time, outcome error) error {
	if repo == nil {
		return nil
	}
	if err := info.Operation.Validate(); err != nil {
		return err
	}
	run := info.Operation.RunID
	if run == "" {
		key, _ := json.Marshal(info.Operation)
		sum := sha256.Sum256(key)
		run = RunID("media-" + hex.EncodeToString(sum[:]))
	}
	identityJSON, _ := json.Marshal([]string{info.Operation.Scope.Namespace, info.Operation.Scope.OwnerID, info.Operation.ID, kind})
	identityHash := sha256.Sum256(identityJSON)
	status := ModelAttemptSuccess
	errorKind := ""
	if outcome != nil {
		status = ModelAttemptFailed
		errorKind = "media_failure"
		if errors.Is(outcome, context.Canceled) || errors.Is(outcome, context.DeadlineExceeded) {
			status = ModelAttemptCancelled
			errorKind = "cancelled"
		}
		var me *MediaError
		if errors.As(outcome, &me) {
			errorKind = string(me.Kind)
			if me.Kind == MediaErrorCancelled {
				status = ModelAttemptCancelled
			}
		}
	}
	payload, err := json.Marshal(struct {
		Kind string          `json:"kind"`
		Info MediaResultInfo `json:"info"`
	}{kind, info})
	if err != nil {
		return err
	}
	return repo.SaveModelAttempts(ctx, []ModelAttemptRecord{{ID: "media-" + hex.EncodeToString(identityHash[:]), RunID: run, ThreadID: info.Operation.ThreadID, Namespace: info.Operation.Scope.Namespace, OwnerID: info.Operation.Scope.OwnerID, Index: 1, Provider: ProviderID(info.Provider), Model: info.Model, Status: status, StartedAt: started, FinishedAt: finished, ProviderRequestID: info.ProviderRequestID, ErrorKind: errorKind, Metadata: Metadata{"media.operation": payload}}})
}
