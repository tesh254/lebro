package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file implements the durable observability repositories for
// SQLiteStore: run events, model attempts, and tool executions. Records are
// intentionally independent of the threads table so failed or cancelled runs
// can persist diagnostics before any transcript exists.

func (r *sqliteRepositories) RunEvents() RunEventRepository         { return r }
func (r *sqliteRepositories) ModelAttempts() ModelAttemptRepository { return r }
func (r *sqliteRepositories) ToolExecutions() ToolExecutionRepository {
	return r
}

func (s *SQLiteStore) RunEvents() RunEventRepository         { return &sqliteRepositories{q: s.db} }
func (s *SQLiteStore) ModelAttempts() ModelAttemptRepository { return &sqliteRepositories{q: s.db} }
func (s *SQLiteStore) ToolExecutions() ToolExecutionRepository {
	return &sqliteRepositories{q: s.db}
}

func (r *sqliteRepositories) AppendRunEvents(ctx context.Context, vs []RunEventRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vs) == 0 {
		return nil
	}
	for _, v := range vs {
		if err := validateRunEventRecord(v); err != nil {
			return err
		}
	}
	return r.withAutoTx(ctx, func(q sqlQueryer) error {
		const chunkSize = 500
		for start := 0; start < len(vs); start += chunkSize {
			end := start + chunkSize
			if end > len(vs) {
				end = len(vs)
			}
			chunk := vs[start:end]
			placeholders := make([]string, 0, len(chunk))
			args := make([]any, 0, len(chunk)*35)
			for _, v := range chunk {
				payload, pluginID, pluginVersion, pluginAction, pluginOutcome := obsEventExtras(v)
				placeholders = append(placeholders, "("+strings.Repeat("?, ", 34)+"?)")
				args = append(args,
					v.ID, v.RunID, v.ThreadID, v.Namespace, v.OwnerID, v.Sequence, v.Type, sqliteTime(v.Timestamp),
					v.Step, v.StepID, v.ParentRunID, v.ParentStepID, v.Branch,
					v.ToolCallID, string(v.ToolID), string(v.Provider), v.ProviderModel,
					string(v.AttemptStatus), string(v.ProcessorPhase), string(v.ProcessorAction),
					string(v.Status), string(v.FinishReason),
					v.Usage.InputTokens, v.Usage.OutputTokens, v.Usage.ReasoningTokens, v.Usage.TotalTokens,
					v.DurationNanos, v.ErrorKind, v.ErrorMessage, payload,
					pluginID, pluginVersion, pluginAction, pluginOutcome,
					obsMetadataJSON(v.Metadata),
				)
			}
			query := `INSERT INTO run_events (
				id, run_id, thread_id, namespace, owner_id, seq, type, timestamp,
				step, step_id, parent_run_id, parent_step_id, branch,
				tool_call_id, tool_id, provider, provider_model,
				attempt_status, processor_phase, processor_action,
				status, finish_reason, input_tokens, output_tokens, reasoning_tokens, total_tokens,
				duration_ns, error_kind, error_message, payload, plugin_id, plugin_version, plugin_action, plugin_outcome, annotations
			) VALUES ` + strings.Join(placeholders, ", ") + ` ON CONFLICT (run_id, id) DO NOTHING`
			if _, err := q.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("lebro: append run events: %w", sqliteError(err))
			}
		}
		return nil
	})
}

func (r *sqliteRepositories) ListRunEvents(ctx context.Context, filter RunEventFilter, p PageRequest) (Page[RunEventRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[RunEventRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[RunEventRecord]{}, err
	}
	where, args := sqliteRunEventFilter(filter)
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, run_id, thread_id, namespace, owner_id, seq, type, timestamp, step, step_id, parent_run_id, parent_step_id,
		 branch, tool_call_id, tool_id, provider, provider_model, attempt_status, processor_phase,
		 processor_action, status, finish_reason, input_tokens, output_tokens, reasoning_tokens,
		 total_tokens, duration_ns, error_kind, error_message, payload, plugin_id, plugin_version, plugin_action, plugin_outcome, annotations
		 FROM run_events `+where+` ORDER BY run_id, seq LIMIT ? OFFSET ?`,
		append(args, limit+1, offset)...)
	if err != nil {
		return Page[RunEventRecord]{}, fmt.Errorf("lebro: list run events: %w", sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	page := Page[RunEventRecord]{Records: []RunEventRecord{}}
	for rows.Next() {
		var (
			v           RunEventRecord
			timestamp   string
			payload     sql.NullString
			pluginID    sql.NullString
			pluginVers  sql.NullString
			pluginAct   sql.NullString
			pluginOut   sql.NullString
			annotations sql.NullString
		)
		if err := rows.Scan(
			&v.ID, &v.RunID, &v.ThreadID, &v.Namespace, &v.OwnerID, &v.Sequence, &v.Type, &timestamp,
			&v.Step, &v.StepID, &v.ParentRunID, &v.ParentStepID, &v.Branch,
			&v.ToolCallID, &v.ToolID, &v.Provider, &v.ProviderModel,
			&v.AttemptStatus, &v.ProcessorPhase, &v.ProcessorAction,
			&v.Status, &v.FinishReason,
			&v.Usage.InputTokens, &v.Usage.OutputTokens, &v.Usage.ReasoningTokens, &v.Usage.TotalTokens,
			&v.DurationNanos, &v.ErrorKind, &v.ErrorMessage, &payload,
			&pluginID, &pluginVers, &pluginAct, &pluginOut, &annotations,
		); err != nil {
			return Page[RunEventRecord]{}, fmt.Errorf("lebro: scan run event: %w", sqliteError(err))
		}
		parsed, err := sqliteParseTime(timestamp)
		if err != nil {
			return Page[RunEventRecord]{}, err
		}
		v.Timestamp = parsed
		v.Payload = sqliteRawJSON(payload)
		if pluginID.Valid && pluginID.String != "" {
			v.Plugin = &PluginAttribution{
				ID:      pluginID.String,
				Version: pluginVers.String,
				Action:  pluginAct.String,
				Outcome: pluginOut.String,
			}
		}
		metadata, err := obsParseMetadata(annotations)
		if err != nil {
			return Page[RunEventRecord]{}, err
		}
		v.Metadata = metadata
		page.Records = append(page.Records, v)
	}
	if err := rows.Err(); err != nil {
		return Page[RunEventRecord]{}, fmt.Errorf("lebro: list run events: %w", sqliteError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func sqliteRunEventFilter(filter RunEventFilter) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.ThreadID != "" {
		clauses = append(clauses, "thread_id = ?")
		args = append(args, filter.ThreadID)
	}
	if filter.Namespace != "" {
		clauses = append(clauses, "namespace = ?")
		args = append(args, filter.Namespace)
	}
	if filter.OwnerID != "" {
		clauses = append(clauses, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}
	if filter.Type != "" {
		clauses = append(clauses, "type = ?")
		args = append(args, filter.Type)
	}
	if !filter.From.IsZero() {
		clauses = append(clauses, "timestamp >= ?")
		args = append(args, sqliteTime(filter.From))
	}
	if !filter.To.IsZero() {
		clauses = append(clauses, "timestamp < ?")
		args = append(args, sqliteTime(filter.To))
	}
	if filter.Provider != "" {
		clauses = append(clauses, "provider = ?")
		args = append(args, filter.Provider)
	}
	if filter.ToolID != "" {
		clauses = append(clauses, "tool_id = ?")
		args = append(args, filter.ToolID)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func (r *sqliteRepositories) SaveModelAttempts(ctx context.Context, vs []ModelAttemptRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vs) == 0 {
		return nil
	}
	for _, v := range vs {
		if err := validateModelAttemptRecord(v); err != nil {
			return err
		}
	}
	return r.withAutoTx(ctx, func(q sqlQueryer) error {
		const chunkSize = 500
		for start := 0; start < len(vs); start += chunkSize {
			end := start + chunkSize
			if end > len(vs) {
				end = len(vs)
			}
			chunk := vs[start:end]
			placeholders := make([]string, 0, len(chunk))
			args := make([]any, 0, len(chunk)*26)
			for _, v := range chunk {
				messageIDs := obsStringArray(v.ProducedMessageIDs)
				placeholders = append(placeholders, "("+strings.Repeat("?, ", 25)+"?)")
				args = append(args,
					v.ID, v.RunID, v.ThreadID, v.Namespace, v.OwnerID, v.Step, v.StepID, v.Index,
					string(v.Provider), v.Model, v.RoutedModel, string(v.Status), string(v.FinishReason),
					v.Usage.InputTokens, v.Usage.OutputTokens, v.Usage.ReasoningTokens, v.Usage.TotalTokens,
					sqliteTime(v.StartedAt), sqliteTime(v.FinishedAt), messageIDs,
					v.ErrorKind, v.ErrorMessage, v.ProviderRequestID, v.CostMicros,
					v.Currency, obsMetadataJSON(v.Metadata),
				)
			}
			query := `INSERT INTO model_attempts (
				id, run_id, thread_id, namespace, owner_id, step, step_id, idx,
				provider, model, routed_model, status, finish_reason,
				input_tokens, output_tokens, reasoning_tokens, total_tokens,
				started_at, finished_at, message_ids,
				error_kind, error_message, provider_request_id, cost_micros, currency, annotations
			) VALUES ` + strings.Join(placeholders, ", ") + ` ON CONFLICT (run_id, id) DO NOTHING`
			if _, err := q.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("lebro: save model attempts: %w", sqliteError(err))
			}
		}
		return nil
	})
}

func (r *sqliteRepositories) ListModelAttempts(ctx context.Context, filter ModelAttemptFilter, p PageRequest) (Page[ModelAttemptRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ModelAttemptRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[ModelAttemptRecord]{}, err
	}
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.ThreadID != "" {
		clauses = append(clauses, "thread_id = ?")
		args = append(args, filter.ThreadID)
	}
	if filter.Namespace != "" {
		clauses = append(clauses, "namespace = ?")
		args = append(args, filter.Namespace)
	}
	if filter.OwnerID != "" {
		clauses = append(clauses, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}
	if filter.Provider != "" {
		clauses = append(clauses, "provider = ?")
		args = append(args, filter.Provider)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, filter.Status)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, run_id, thread_id, namespace, owner_id, step, step_id, idx, provider, model, routed_model, status,
		 finish_reason, input_tokens, output_tokens, reasoning_tokens, total_tokens, started_at,
		 finished_at, message_ids, error_kind, error_message, provider_request_id, cost_micros,
		 currency, annotations FROM model_attempts `+where+` ORDER BY run_id, seq LIMIT ? OFFSET ?`,
		append(args, limit+1, offset)...)
	if err != nil {
		return Page[ModelAttemptRecord]{}, fmt.Errorf("lebro: list model attempts: %w", sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	page := Page[ModelAttemptRecord]{Records: []ModelAttemptRecord{}}
	for rows.Next() {
		var (
			v           ModelAttemptRecord
			startedAt   string
			finishedAt  string
			messageIDs  sql.NullString
			annotations sql.NullString
		)
		if err := rows.Scan(
			&v.ID, &v.RunID, &v.ThreadID, &v.Namespace, &v.OwnerID, &v.Step, &v.StepID, &v.Index,
			&v.Provider, &v.Model, &v.RoutedModel, &v.Status, &v.FinishReason,
			&v.Usage.InputTokens, &v.Usage.OutputTokens, &v.Usage.ReasoningTokens, &v.Usage.TotalTokens,
			&startedAt, &finishedAt, &messageIDs,
			&v.ErrorKind, &v.ErrorMessage, &v.ProviderRequestID, &v.CostMicros,
			&v.Currency, &annotations,
		); err != nil {
			return Page[ModelAttemptRecord]{}, fmt.Errorf("lebro: scan model attempt: %w", sqliteError(err))
		}
		started, err := sqliteParseTime(startedAt)
		if err != nil {
			return Page[ModelAttemptRecord]{}, err
		}
		finished, err := sqliteParseTime(finishedAt)
		if err != nil {
			return Page[ModelAttemptRecord]{}, err
		}
		v.StartedAt, v.FinishedAt = started, finished
		if messageIDs.Valid && messageIDs.String != "" {
			if err := json.Unmarshal([]byte(messageIDs.String), &v.ProducedMessageIDs); err != nil {
				return Page[ModelAttemptRecord]{}, fmt.Errorf("lebro: decode attempt message IDs: %w", err)
			}
		}
		metadata, err := obsParseMetadata(annotations)
		if err != nil {
			return Page[ModelAttemptRecord]{}, err
		}
		v.Metadata = metadata
		page.Records = append(page.Records, v)
	}
	if err := rows.Err(); err != nil {
		return Page[ModelAttemptRecord]{}, fmt.Errorf("lebro: list model attempts: %w", sqliteError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func (r *sqliteRepositories) SaveToolExecutions(ctx context.Context, vs []ToolExecutionRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vs) == 0 {
		return nil
	}
	for _, v := range vs {
		if err := validateToolExecutionRecord(v); err != nil {
			return err
		}
	}
	return r.withAutoTx(ctx, func(q sqlQueryer) error {
		const chunkSize = 500
		for start := 0; start < len(vs); start += chunkSize {
			end := start + chunkSize
			if end > len(vs) {
				end = len(vs)
			}
			chunk := vs[start:end]
			placeholders := make([]string, 0, len(chunk))
			args := make([]any, 0, len(chunk)*15)
			for _, v := range chunk {
				placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
				args = append(args,
					v.ID, v.RunID, v.ThreadID, v.Namespace, v.OwnerID, v.Step, v.StepID,
					v.ToolCallID, string(v.ToolID), string(v.State),
					sqliteTime(v.StartedAt), sqliteNullableTime(timePointer(v.FinishedAt)),
					v.ErrorKind, v.ErrorMessage, obsMetadataJSON(v.Metadata),
				)
			}
			query := `INSERT INTO tool_executions (
				id, run_id, thread_id, namespace, owner_id, step, step_id,
				tool_call_id, tool_id, state, started_at, finished_at,
				error_kind, error_message, annotations
			) VALUES ` + strings.Join(placeholders, ", ") + ` ON CONFLICT (run_id, id) DO NOTHING`
			if _, err := q.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("lebro: save tool executions: %w", sqliteError(err))
			}
		}
		return nil
	})
}

func (r *sqliteRepositories) ListToolExecutions(ctx context.Context, filter ToolExecutionFilter, p PageRequest) (Page[ToolExecutionRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ToolExecutionRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[ToolExecutionRecord]{}, err
	}
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.ThreadID != "" {
		clauses = append(clauses, "thread_id = ?")
		args = append(args, filter.ThreadID)
	}
	if filter.Namespace != "" {
		clauses = append(clauses, "namespace = ?")
		args = append(args, filter.Namespace)
	}
	if filter.OwnerID != "" {
		clauses = append(clauses, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}
	if filter.ToolID != "" {
		clauses = append(clauses, "tool_id = ?")
		args = append(args, filter.ToolID)
	}
	if filter.State != "" {
		clauses = append(clauses, "state = ?")
		args = append(args, filter.State)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, run_id, thread_id, namespace, owner_id, step, step_id, tool_call_id, tool_id, state, started_at,
		 finished_at, error_kind, error_message, annotations FROM tool_executions `+where+` ORDER BY run_id, seq LIMIT ? OFFSET ?`,
		append(args, limit+1, offset)...)
	if err != nil {
		return Page[ToolExecutionRecord]{}, fmt.Errorf("lebro: list tool executions: %w", sqliteError(err))
	}
	defer func() { _ = rows.Close() }()
	page := Page[ToolExecutionRecord]{Records: []ToolExecutionRecord{}}
	for rows.Next() {
		var (
			v           ToolExecutionRecord
			startedAt   string
			finishedAt  sql.NullString
			annotations sql.NullString
		)
		if err := rows.Scan(
			&v.ID, &v.RunID, &v.ThreadID, &v.Namespace, &v.OwnerID, &v.Step, &v.StepID,
			&v.ToolCallID, &v.ToolID, &v.State, &startedAt, &finishedAt,
			&v.ErrorKind, &v.ErrorMessage, &annotations,
		); err != nil {
			return Page[ToolExecutionRecord]{}, fmt.Errorf("lebro: scan tool execution: %w", sqliteError(err))
		}
		started, err := sqliteParseTime(startedAt)
		if err != nil {
			return Page[ToolExecutionRecord]{}, err
		}
		v.StartedAt = started
		parsedFinished, err := sqliteParseNullableTime(finishedAt)
		if err != nil {
			return Page[ToolExecutionRecord]{}, err
		}
		if parsedFinished != nil {
			v.FinishedAt = *parsedFinished
		}
		metadata, err := obsParseMetadata(annotations)
		if err != nil {
			return Page[ToolExecutionRecord]{}, err
		}
		v.Metadata = metadata
		page.Records = append(page.Records, v)
	}
	if err := rows.Err(); err != nil {
		return Page[ToolExecutionRecord]{}, fmt.Errorf("lebro: list tool executions: %w", sqliteError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

// timePointer returns a pointer to t, or nil when t is the zero time.
func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	value := t
	return &value
}
