package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

func workingMemoryMapKey(s WorkingMemoryScope, key string) string {
	return s.Namespace + "\x00" + s.OwnerID + "\x00" + key
}

func validateWorkingMemoryScope(ctx context.Context, s WorkingMemoryScope) error {
	if s.Namespace == "" || s.OwnerID == "" {
		return errors.New("lebro: working memory namespace and owner ID are required")
	}
	if identity, ok := IdentityFromContext(ctx); ok && (identity.Tenant != s.Namespace || identity.Subject != s.OwnerID) {
		return &PolicyDenial{Subject: identity.Subject, Tenant: identity.Tenant, Action: ActionStorageWrite, Resource: Resource{Kind: ResourceKindWorkingMemory, ID: s.OwnerID, Tenant: s.Namespace}, Reason: "working memory scope does not match caller identity"}
	}
	return nil
}

func validateWorkingMemoryFact(ctx context.Context, v WorkingMemoryFact) error {
	if v.ID == "" || v.Key == "" {
		return errors.New("lebro: working memory fact ID and key are required")
	}
	if err := validateWorkingMemoryScope(ctx, WorkingMemoryScope{v.Namespace, v.OwnerID}); err != nil {
		return err
	}
	if err := validateJSON(v.Value); err != nil {
		return fmt.Errorf("lebro: working memory fact value: %w", err)
	}
	return nil
}

func (s *MemoryStore) UpsertWorkingMemoryFact(ctx context.Context, v WorkingMemoryFact, expected int64) (WorkingMemoryFact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, err := upsertWorkingMemoryFact(ctx, &s.state, v, expected)
	if err == nil {
		s.version++
	}
	return got, err
}
func (s *MemoryStore) GetWorkingMemoryFact(ctx context.Context, scope WorkingMemoryScope, key string) (WorkingMemoryFact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getWorkingMemoryFact(ctx, s.state, scope, key)
}
func (s *MemoryStore) ListWorkingMemoryFacts(ctx context.Context, scope WorkingMemoryScope, page PageRequest) (Page[WorkingMemoryFact], error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listWorkingMemoryFacts(ctx, s.state, scope, page)
}
func (s *MemoryStore) DeleteWorkingMemoryFact(ctx context.Context, scope WorkingMemoryScope, key string, expected int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := deleteWorkingMemoryFact(ctx, &s.state, scope, key, expected)
	if err == nil {
		s.version++
	}
	return err
}
func (s *MemoryStore) ClearWorkingMemory(ctx context.Context, scope WorkingMemoryScope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := clearWorkingMemory(ctx, &s.state, scope)
	if err == nil {
		s.version++
	}
	return err
}

func (r *memoryRepositories) UpsertWorkingMemoryFact(ctx context.Context, v WorkingMemoryFact, expected int64) (WorkingMemoryFact, error) {
	got, err := upsertWorkingMemoryFact(ctx, &r.state, v, expected)
	if err == nil {
		r.dirty = true
	}
	return got, err
}
func (r *memoryRepositories) GetWorkingMemoryFact(ctx context.Context, s WorkingMemoryScope, k string) (WorkingMemoryFact, error) {
	return getWorkingMemoryFact(ctx, r.state, s, k)
}
func (r *memoryRepositories) ListWorkingMemoryFacts(ctx context.Context, s WorkingMemoryScope, p PageRequest) (Page[WorkingMemoryFact], error) {
	return listWorkingMemoryFacts(ctx, r.state, s, p)
}
func (r *memoryRepositories) DeleteWorkingMemoryFact(ctx context.Context, s WorkingMemoryScope, k string, e int64) error {
	err := deleteWorkingMemoryFact(ctx, &r.state, s, k, e)
	if err == nil {
		r.dirty = true
	}
	return err
}
func (r *memoryRepositories) ClearWorkingMemory(ctx context.Context, s WorkingMemoryScope) error {
	err := clearWorkingMemory(ctx, &r.state, s)
	if err == nil {
		r.dirty = true
	}
	return err
}

func upsertWorkingMemoryFact(ctx context.Context, s *memoryState, v WorkingMemoryFact, expected int64) (WorkingMemoryFact, error) {
	if err := ctx.Err(); err != nil {
		return WorkingMemoryFact{}, err
	}
	if err := validateWorkingMemoryFact(ctx, v); err != nil {
		return WorkingMemoryFact{}, err
	}
	k := workingMemoryMapKey(WorkingMemoryScope{v.Namespace, v.OwnerID}, v.Key)
	old, ok := s.workingMemory[k]
	if ok {
		if expected == 0 || old.Version != expected {
			return WorkingMemoryFact{}, ErrConflict
		}
		v.Version = old.Version + 1
		v.CreatedAt = old.CreatedAt
	} else {
		if expected != 0 {
			return WorkingMemoryFact{}, ErrConflict
		}
		v.Version = 1
	}
	v.Value = cloneJSON(v.Value)
	s.workingMemory[k] = v
	return cloneWorkingMemoryFact(v), nil
}
func getWorkingMemoryFact(ctx context.Context, s memoryState, scope WorkingMemoryScope, key string) (WorkingMemoryFact, error) {
	if err := ctx.Err(); err != nil {
		return WorkingMemoryFact{}, err
	}
	if err := validateWorkingMemoryScope(ctx, scope); err != nil {
		return WorkingMemoryFact{}, err
	}
	v, ok := s.workingMemory[workingMemoryMapKey(scope, key)]
	if !ok {
		return WorkingMemoryFact{}, ErrNotFound
	}
	return cloneWorkingMemoryFact(v), nil
}
func listWorkingMemoryFacts(ctx context.Context, s memoryState, scope WorkingMemoryScope, p PageRequest) (Page[WorkingMemoryFact], error) {
	if err := ctx.Err(); err != nil {
		return Page[WorkingMemoryFact]{}, err
	}
	if err := validateWorkingMemoryScope(ctx, scope); err != nil {
		return Page[WorkingMemoryFact]{}, err
	}
	out := []WorkingMemoryFact{}
	for _, v := range s.workingMemory {
		if v.Namespace == scope.Namespace && v.OwnerID == scope.OwnerID {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return paginate(out, p, cloneWorkingMemoryFact)
}
func deleteWorkingMemoryFact(ctx context.Context, s *memoryState, scope WorkingMemoryScope, key string, expected int64) error {
	v, err := getWorkingMemoryFact(ctx, *s, scope, key)
	if err != nil {
		return err
	}
	if expected == 0 || v.Version != expected {
		return ErrConflict
	}
	delete(s.workingMemory, workingMemoryMapKey(scope, key))
	return nil
}
func clearWorkingMemory(ctx context.Context, s *memoryState, scope WorkingMemoryScope) error {
	if err := validateWorkingMemoryScope(ctx, scope); err != nil {
		return err
	}
	for k, v := range s.workingMemory {
		if v.Namespace == scope.Namespace && v.OwnerID == scope.OwnerID {
			delete(s.workingMemory, k)
		}
	}
	return nil
}
func cloneWorkingMemoryFact(v WorkingMemoryFact) WorkingMemoryFact {
	v.Value = cloneJSON(v.Value)
	return v
}

func scanWorkingMemoryFact(row interface{ Scan(...any) error }) (WorkingMemoryFact, error) {
	var v WorkingMemoryFact
	var raw string
	var created, updated any
	err := row.Scan(&v.ID, &v.Namespace, &v.OwnerID, &v.Key, &raw, &v.Version, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkingMemoryFact{}, ErrNotFound
	}
	if err != nil {
		return WorkingMemoryFact{}, err
	}
	v.Value = json.RawMessage(raw)
	var ok bool
	if v.CreatedAt, ok = workingMemoryTime(created); !ok {
		return WorkingMemoryFact{}, fmt.Errorf("lebro: scan working memory created_at")
	}
	if v.UpdatedAt, ok = workingMemoryTime(updated); !ok {
		return WorkingMemoryFact{}, fmt.Errorf("lebro: scan working memory updated_at")
	}
	return v, nil
}
func workingMemoryTime(v any) (time.Time, bool) {
	switch value := v.(type) {
	case time.Time:
		return value, true
	case string:
		t, err := sqliteParseTime(value)
		return t, err == nil
	case []byte:
		t, err := sqliteParseTime(string(value))
		return t, err == nil
	}
	return time.Time{}, false
}
func (r *sqliteRepositories) UpsertWorkingMemoryFact(ctx context.Context, v WorkingMemoryFact, e int64) (WorkingMemoryFact, error) {
	if err := validateWorkingMemoryFact(ctx, v); err != nil {
		return WorkingMemoryFact{}, err
	}
	q := `INSERT INTO working_memory_facts (id,namespace,owner_id,key,value,version,created_at,updated_at) VALUES (?,?,?,?,?,1,?,?) ON CONFLICT(namespace,owner_id,key) DO UPDATE SET id=excluded.id,value=excluded.value,version=working_memory_facts.version+1,updated_at=excluded.updated_at WHERE working_memory_facts.version=? RETURNING id,namespace,owner_id,key,value,version,created_at,updated_at`
	if e == 0 {
		q = `INSERT INTO working_memory_facts (id,namespace,owner_id,key,value,version,created_at,updated_at) VALUES (?,?,?,?,?,1,?,?) RETURNING id,namespace,owner_id,key,value,version,created_at,updated_at`
		got, err := scanWorkingMemoryFact(r.q.QueryRowContext(ctx, q, v.ID, v.Namespace, v.OwnerID, v.Key, string(v.Value), sqliteTime(v.CreatedAt), sqliteTime(v.UpdatedAt)))
		if err != nil && isSQLiteUniqueConstraint(err) {
			return WorkingMemoryFact{}, ErrConflict
		}
		if err != nil {
			return WorkingMemoryFact{}, fmt.Errorf("lebro: create working memory fact: %w", sqliteError(err))
		}
		return got, nil
	}
	got, err := scanWorkingMemoryFact(r.q.QueryRowContext(ctx, q, v.ID, v.Namespace, v.OwnerID, v.Key, string(v.Value), sqliteTime(v.CreatedAt), sqliteTime(v.UpdatedAt), e))
	if errors.Is(err, ErrNotFound) {
		return WorkingMemoryFact{}, ErrConflict
	}
	return got, err
}
func (r *sqliteRepositories) GetWorkingMemoryFact(ctx context.Context, s WorkingMemoryScope, k string) (WorkingMemoryFact, error) {
	if err := validateWorkingMemoryScope(ctx, s); err != nil {
		return WorkingMemoryFact{}, err
	}
	return scanWorkingMemoryFact(r.q.QueryRowContext(ctx, `SELECT id,namespace,owner_id,key,value,version,created_at,updated_at FROM working_memory_facts WHERE namespace=? AND owner_id=? AND key=?`, s.Namespace, s.OwnerID, k))
}
func (r *sqliteRepositories) ListWorkingMemoryFacts(ctx context.Context, s WorkingMemoryScope, p PageRequest) (Page[WorkingMemoryFact], error) {
	if err := validateWorkingMemoryScope(ctx, s); err != nil {
		return Page[WorkingMemoryFact]{}, err
	}
	off, lim, err := sqlPageBounds(p)
	if err != nil {
		return Page[WorkingMemoryFact]{}, err
	}
	rows, err := r.q.QueryContext(ctx, `SELECT id,namespace,owner_id,key,value,version,created_at,updated_at FROM working_memory_facts WHERE namespace=? AND owner_id=? ORDER BY key LIMIT ? OFFSET ?`, s.Namespace, s.OwnerID, lim+1, off)
	if err != nil {
		return Page[WorkingMemoryFact]{}, err
	}
	defer func() { _ = rows.Close() }()
	out := Page[WorkingMemoryFact]{Records: []WorkingMemoryFact{}}
	for rows.Next() {
		v, err := scanWorkingMemoryFact(rows)
		if err != nil {
			return out, err
		}
		out.Records = append(out.Records, v)
	}
	if len(out.Records) > lim {
		out.Records = out.Records[:lim]
		out.NextCursor = fmt.Sprint(off + lim)
	}
	return out, rows.Err()
}
func (r *sqliteRepositories) DeleteWorkingMemoryFact(ctx context.Context, s WorkingMemoryScope, k string, e int64) error {
	if err := validateWorkingMemoryScope(ctx, s); err != nil {
		return err
	}
	res, err := r.q.ExecContext(ctx, `DELETE FROM working_memory_facts WHERE namespace=? AND owner_id=? AND key=? AND version=?`, s.Namespace, s.OwnerID, k, e)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrConflict
	}
	return nil
}
func (r *sqliteRepositories) ClearWorkingMemory(ctx context.Context, s WorkingMemoryScope) error {
	if err := validateWorkingMemoryScope(ctx, s); err != nil {
		return err
	}
	_, err := r.q.ExecContext(ctx, `DELETE FROM working_memory_facts WHERE namespace=? AND owner_id=?`, s.Namespace, s.OwnerID)
	return err
}

func (r *postgresRepositories) UpsertWorkingMemoryFact(ctx context.Context, v WorkingMemoryFact, e int64) (WorkingMemoryFact, error) {
	if err := validateWorkingMemoryFact(ctx, v); err != nil {
		return WorkingMemoryFact{}, err
	}
	if e == 0 {
		got, err := scanWorkingMemoryFact(r.q.QueryRowContext(ctx, `INSERT INTO working_memory_facts (id,namespace,owner_id,key,value,version,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,1,$6,$7) RETURNING id,namespace,owner_id,key,value,version,created_at,updated_at`, v.ID, v.Namespace, v.OwnerID, v.Key, string(v.Value), v.CreatedAt.UTC(), v.UpdatedAt.UTC()))
		if err != nil && isPostgresUniqueViolation(err) {
			return WorkingMemoryFact{}, ErrConflict
		}
		return got, err
	}
	got, err := scanWorkingMemoryFact(r.q.QueryRowContext(ctx, `UPDATE working_memory_facts SET id=$1,value=$2,version=version+1,updated_at=$3 WHERE namespace=$4 AND owner_id=$5 AND key=$6 AND version=$7 RETURNING id,namespace,owner_id,key,value,version,created_at,updated_at`, v.ID, string(v.Value), v.UpdatedAt.UTC(), v.Namespace, v.OwnerID, v.Key, e))
	if errors.Is(err, ErrNotFound) {
		return WorkingMemoryFact{}, ErrConflict
	}
	return got, err
}
func (r *postgresRepositories) GetWorkingMemoryFact(ctx context.Context, s WorkingMemoryScope, k string) (WorkingMemoryFact, error) {
	if err := validateWorkingMemoryScope(ctx, s); err != nil {
		return WorkingMemoryFact{}, err
	}
	return scanWorkingMemoryFact(r.q.QueryRowContext(ctx, `SELECT id,namespace,owner_id,key,value,version,created_at,updated_at FROM working_memory_facts WHERE namespace=$1 AND owner_id=$2 AND key=$3`, s.Namespace, s.OwnerID, k))
}
func (r *postgresRepositories) ListWorkingMemoryFacts(ctx context.Context, s WorkingMemoryScope, p PageRequest) (Page[WorkingMemoryFact], error) {
	if err := validateWorkingMemoryScope(ctx, s); err != nil {
		return Page[WorkingMemoryFact]{}, err
	}
	off, lim, err := sqlPageBounds(p)
	if err != nil {
		return Page[WorkingMemoryFact]{}, err
	}
	rows, err := r.q.QueryContext(ctx, `SELECT id,namespace,owner_id,key,value,version,created_at,updated_at FROM working_memory_facts WHERE namespace=$1 AND owner_id=$2 ORDER BY key LIMIT $3 OFFSET $4`, s.Namespace, s.OwnerID, lim+1, off)
	if err != nil {
		return Page[WorkingMemoryFact]{}, err
	}
	defer func() { _ = rows.Close() }()
	out := Page[WorkingMemoryFact]{Records: []WorkingMemoryFact{}}
	for rows.Next() {
		v, err := scanWorkingMemoryFact(rows)
		if err != nil {
			return out, err
		}
		out.Records = append(out.Records, v)
	}
	if len(out.Records) > lim {
		out.Records = out.Records[:lim]
		out.NextCursor = fmt.Sprint(off + lim)
	}
	return out, rows.Err()
}
func (r *postgresRepositories) DeleteWorkingMemoryFact(ctx context.Context, s WorkingMemoryScope, k string, e int64) error {
	if err := validateWorkingMemoryScope(ctx, s); err != nil {
		return err
	}
	res, err := r.q.ExecContext(ctx, `DELETE FROM working_memory_facts WHERE namespace=$1 AND owner_id=$2 AND key=$3 AND version=$4`, s.Namespace, s.OwnerID, k, e)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrConflict
	}
	return nil
}
func (r *postgresRepositories) ClearWorkingMemory(ctx context.Context, s WorkingMemoryScope) error {
	if err := validateWorkingMemoryScope(ctx, s); err != nil {
		return err
	}
	_, err := r.q.ExecContext(ctx, `DELETE FROM working_memory_facts WHERE namespace=$1 AND owner_id=$2`, s.Namespace, s.OwnerID)
	return err
}
