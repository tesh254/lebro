package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

const mediaJobsMigration = `CREATE TABLE IF NOT EXISTS media_jobs (
 namespace TEXT NOT NULL, owner_id TEXT NOT NULL, id TEXT NOT NULL,
 revision BIGINT NOT NULL, terminal BOOLEAN NOT NULL, record TEXT NOT NULL,
 PRIMARY KEY (namespace, owner_id, id)
)`

func mediaJobKey(scope RuntimeScope, id string) string {
	b, _ := json.Marshal([]string{scope.Namespace, scope.OwnerID, id})
	return string(b)
}
func validateVideoRecord(ctx context.Context, j VideoJob, expected int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := j.Info.Operation.Validate(); err != nil {
		return nil, err
	}
	if scope, ok := RuntimeScopeFromContext(ctx); ok && scope != j.Info.Operation.Scope {
		return nil, &MediaError{Kind: MediaErrorAuthorization, Message: "media scope mismatch"}
	}
	if expected < 0 || j.Revision != expected+1 || j.CreatedAt.IsZero() || j.UpdatedAt.Before(j.CreatedAt) {
		return nil, mediaInvalid("invalid job revision or timestamps")
	}
	switch j.State {
	case MediaJobSubmitting, MediaJobAmbiguous, MediaJobQueued, MediaJobRunning, MediaJobSucceeded, MediaJobFailed, MediaJobCancelled, MediaJobExpired:
	default:
		return nil, mediaInvalid("invalid job state")
	}
	if len(j.ProviderJobID) > 256 || strings.ContainsAny(j.ProviderJobID, "/?#\r\n") {
		return nil, mediaInvalid("invalid provider job identity")
	}
	for _, a := range j.Assets {
		if a.ID == "" || a.Locator == "" || strings.Contains(a.Locator, "://") || strings.ContainsAny(a.Locator, "?#") {
			return nil, mediaInvalid("persist only durable asset keys")
		}
	}
	b, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<10 {
		return nil, mediaInvalid("job metadata exceeds 64 KiB")
	}
	return b, nil
}
func checkMediaRead(ctx context.Context, scope RuntimeScope, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := (MediaOperation{ID: id, Scope: scope}).Validate(); err != nil {
		return err
	}
	if trusted, ok := RuntimeScopeFromContext(ctx); ok && trusted != scope {
		return &MediaError{Kind: MediaErrorAuthorization, Message: "media scope mismatch"}
	}
	return nil
}
func (s *MemoryStore) MediaJobs() MediaJobRepository { return s }
func (s *MemoryStore) GetVideoJob(ctx context.Context, scope RuntimeScope, id string) (VideoJob, error) {
	if err := checkMediaRead(ctx, scope, id); err != nil {
		return VideoJob{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.mediaJobs[mediaJobKey(scope, id)]
	if !ok {
		return VideoJob{}, ErrNotFound
	}
	var j VideoJob
	err := json.Unmarshal(b, &j)
	return j, err
}
func (s *MemoryStore) SaveVideoJob(ctx context.Context, j VideoJob, expected int64) error {
	b, err := validateVideoRecord(ctx, j, expected)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := mediaJobKey(j.Info.Operation.Scope, j.Info.Operation.ID)
	if old, ok := s.mediaJobs[key]; ok {
		var prior VideoJob
		if err = json.Unmarshal(old, &prior); err != nil {
			return err
		}
		if prior.Revision != expected || prior.State.Terminal() {
			return ErrConflict
		}
	} else if expected != 0 {
		return ErrConflict
	}
	if s.mediaJobs == nil {
		s.mediaJobs = map[string][]byte{}
	}
	s.mediaJobs[key] = b
	return nil
}

type sqlMediaJobs struct {
	db       *sql.DB
	postgres bool
}

func (s *SQLiteStore) MediaJobs() MediaJobRepository { return &sqlMediaJobs{db: s.db} }
func (s *PostgresStore) MediaJobs() MediaJobRepository {
	return &sqlMediaJobs{db: s.db, postgres: true}
}
func (s *sqlMediaJobs) bind(query string) string {
	if !s.postgres {
		return query
	}
	var out strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(n))
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}
func (s *sqlMediaJobs) GetVideoJob(ctx context.Context, scope RuntimeScope, id string) (VideoJob, error) {
	if err := checkMediaRead(ctx, scope, id); err != nil {
		return VideoJob{}, err
	}
	var b string
	err := s.db.QueryRowContext(ctx, s.bind("SELECT record FROM media_jobs WHERE namespace=? AND owner_id=? AND id=?"), scope.Namespace, scope.OwnerID, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return VideoJob{}, ErrNotFound
	}
	if err != nil {
		return VideoJob{}, err
	}
	var j VideoJob
	err = json.Unmarshal([]byte(b), &j)
	return j, err
}
func (s *sqlMediaJobs) SaveVideoJob(ctx context.Context, j VideoJob, expected int64) error {
	b, err := validateVideoRecord(ctx, j, expected)
	if err != nil {
		return err
	}
	o := j.Info.Operation
	var result sql.Result
	if expected == 0 {
		result, err = s.db.ExecContext(ctx, s.bind("INSERT INTO media_jobs(namespace,owner_id,id,revision,terminal,record) VALUES(?,?,?,?,?,?) ON CONFLICT(namespace,owner_id,id) DO NOTHING"), o.Scope.Namespace, o.Scope.OwnerID, o.ID, j.Revision, j.State.Terminal(), string(b))
	} else {
		result, err = s.db.ExecContext(ctx, s.bind("UPDATE media_jobs SET revision=?,terminal=?,record=? WHERE namespace=? AND owner_id=? AND id=? AND revision=? AND terminal=?"), j.Revision, j.State.Terminal(), string(b), o.Scope.Namespace, o.Scope.OwnerID, o.ID, expected, false)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}
