package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// This file implements the durable observability repositories for
// PostgresStore: run events, model attempts, and tool executions. Records are
// intentionally independent of the threads table so failed or cancelled runs
// can persist diagnostics before any transcript exists.

func (r *postgresRepositories) RunEvents() RunEventRepository         { return r }
func (r *postgresRepositories) ModelAttempts() ModelAttemptRepository { return r }
func (r *postgresRepositories) ToolExecutions() ToolExecutionRepository {
	return r
}

func (s *PostgresStore) RunEvents() RunEventRepository { return &postgresRepositories{q: s.db} }
func (s *PostgresStore) ModelAttempts() ModelAttemptRepository {
	return &postgresRepositories{q: s.db}
}
func (s *PostgresStore) ToolExecutions() ToolExecutionRepository {
	return &postgresRepositories{q: s.db}
}

func (r *postgresRepositories) AppendRunEvents(ctx context.Context, vs []RunEventRecord) error {
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
		for _, v := range vs {
			payload, pluginID, pluginVersion, pluginAction, pluginOutcome := obsEventExtras(v)
			if _, err := q.ExecContext(ctx, `INSERT INTO run_events (
				id, run_id, thread_id, namespace, owner_id, seq, type, timestamp,
				step, step_id, parent_run_id, parent_step_id, branch,
				tool_call_id, tool_id, provider, provider_model,
				attempt_status, processor_phase, processor_action,
				status, finish_reason, input_tokens, output_tokens, reasoning_tokens, total_tokens,
				duration_ns, error_kind, error_message, payload, plugin_id, plugin_version, plugin_action, plugin_outcome, annotations
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35) ON CONFLICT (run_id, id) DO NOTHING`,
				v.ID, v.RunID, v.ThreadID, v.Namespace, v.OwnerID, v.Sequence, v.Type, v.Timestamp.UTC(),
				v.Step, v.StepID, v.ParentRunID, v.ParentStepID, v.Branch,
				v.ToolCallID, string(v.ToolID), string(v.Provider), v.ProviderModel,
				string(v.AttemptStatus), string(v.ProcessorPhase), string(v.ProcessorAction),
				string(v.Status), string(v.FinishReason),
				v.Usage.InputTokens, v.Usage.OutputTokens, v.Usage.ReasoningTokens, v.Usage.TotalTokens,
				v.DurationNanos, v.ErrorKind, v.ErrorMessage, payload,
				pluginID, pluginVersion, pluginAction, pluginOutcome,
				obsMetadataJSON(v.Metadata),
			); err != nil {
				return fmt.Errorf("lebro: append run event %q: %w", v.ID, postgresError(err))
			}
		}
		return nil
	})
}

func (r *postgresRepositories) ListRunEvents(ctx context.Context, filter RunEventFilter, p PageRequest) (Page[RunEventRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[RunEventRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[RunEventRecord]{}, err
	}
	clauses := make([]string, 0, 7)
	args := make([]any, 0, 9)
	appendClause := func(format string, value any) {
		clauses = append(clauses, fmt.Sprintf(format, len(args)+1))
		args = append(args, value)
	}
	if filter.RunID != "" {
		appendClause("run_id = $%d", filter.RunID)
	}
	if filter.ThreadID != "" {
		appendClause("thread_id = $%d", filter.ThreadID)
	}
	if filter.Namespace != "" {
		appendClause("namespace = $%d", filter.Namespace)
	}
	if filter.OwnerID != "" {
		appendClause("owner_id = $%d", filter.OwnerID)
	}
	if filter.Type != "" {
		appendClause("type = $%d", filter.Type)
	}
	if !filter.From.IsZero() {
		appendClause("timestamp >= $%d", filter.From.UTC())
	}
	if !filter.To.IsZero() {
		appendClause("timestamp < $%d", filter.To.UTC())
	}
	if filter.Provider != "" {
		appendClause("provider = $%d", filter.Provider)
	}
	if filter.ToolID != "" {
		appendClause("tool_id = $%d", filter.ToolID)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, run_id, thread_id, namespace, owner_id, seq, type, timestamp, step, step_id, parent_run_id, parent_step_id,
		 branch, tool_call_id, tool_id, provider, provider_model, attempt_status, processor_phase,
		 processor_action, status, finish_reason, input_tokens, output_tokens, reasoning_tokens,
		 total_tokens, duration_ns, error_kind, error_message, payload, plugin_id, plugin_version, plugin_action, plugin_outcome, annotations
		 FROM run_events `+where+fmt.Sprintf(` ORDER BY run_id, seq LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2),
		append(args, postgresFetchLimit(limit), offset)...)
	if err != nil {
		return Page[RunEventRecord]{}, fmt.Errorf("lebro: list run events: %w", postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	page := Page[RunEventRecord]{Records: []RunEventRecord{}}
	for rows.Next() {
		var (
			v           RunEventRecord
			payload     sql.NullString
			pluginID    sql.NullString
			pluginVers  sql.NullString
			pluginAct   sql.NullString
			pluginOut   sql.NullString
			annotations sql.NullString
		)
		if err := rows.Scan(
			&v.ID, &v.RunID, &v.ThreadID, &v.Namespace, &v.OwnerID, &v.Sequence, &v.Type, &v.Timestamp,
			&v.Step, &v.StepID, &v.ParentRunID, &v.ParentStepID, &v.Branch,
			&v.ToolCallID, &v.ToolID, &v.Provider, &v.ProviderModel,
			&v.AttemptStatus, &v.ProcessorPhase, &v.ProcessorAction,
			&v.Status, &v.FinishReason,
			&v.Usage.InputTokens, &v.Usage.OutputTokens, &v.Usage.ReasoningTokens, &v.Usage.TotalTokens,
			&v.DurationNanos, &v.ErrorKind, &v.ErrorMessage, &payload,
			&pluginID, &pluginVers, &pluginAct, &pluginOut, &annotations,
		); err != nil {
			return Page[RunEventRecord]{}, fmt.Errorf("lebro: scan run event: %w", postgresError(err))
		}
		v.Timestamp = v.Timestamp.UTC()
		v.Payload = postgresRawJSON([]byte(payload.String))
		if pluginID.Valid && pluginID.String != "" {
			v.Plugin = &PluginAttribution{
				ID:      pluginID.String,
				Version: pluginVers.String,
				Action:  pluginAct.String,
				Outcome: pluginOut.String,
			}
		}
		m, merr := obsParseMetadata(annotations)
		if merr != nil {
			return page, merr
		}
		v.Metadata = m
		page.Records = append(page.Records, v)
	}
	if err := rows.Err(); err != nil {
		return Page[RunEventRecord]{}, fmt.Errorf("lebro: list run events: %w", postgresError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func (r *postgresRepositories) SaveModelAttempts(ctx context.Context, vs []ModelAttemptRecord) error {
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
		for _, v := range vs {
			messageIDs := obsStringArray(v.ProducedMessageIDs)
			if _, err := q.ExecContext(ctx, `INSERT INTO model_attempts (
				id, run_id, thread_id, namespace, owner_id, step, step_id, idx,
				provider, model, routed_model, status, finish_reason,
				input_tokens, output_tokens, reasoning_tokens, total_tokens,
				started_at, finished_at, message_ids,
				error_kind, error_message, provider_request_id, cost_micros, currency, annotations
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26) ON CONFLICT (run_id, id) DO NOTHING`,
				v.ID, v.RunID, v.ThreadID, v.Namespace, v.OwnerID, v.Step, v.StepID, v.Index,
				string(v.Provider), v.Model, v.RoutedModel, string(v.Status), string(v.FinishReason),
				v.Usage.InputTokens, v.Usage.OutputTokens, v.Usage.ReasoningTokens, v.Usage.TotalTokens,
				v.StartedAt.UTC(), v.FinishedAt.UTC(), messageIDs,
				v.ErrorKind, v.ErrorMessage, v.ProviderRequestID, v.CostMicros, v.Currency,
				obsMetadataJSON(v.Metadata),
			); err != nil {
				return fmt.Errorf("lebro: save model attempt %q: %w", v.ID, postgresError(err))
			}
		}
		return nil
	})
}

func (r *postgresRepositories) ListModelAttempts(ctx context.Context, filter ModelAttemptFilter, p PageRequest) (Page[ModelAttemptRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ModelAttemptRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[ModelAttemptRecord]{}, err
	}
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 6)
	appendClause := func(format string, value any) {
		clauses = append(clauses, fmt.Sprintf(format, len(args)+1))
		args = append(args, value)
	}
	if filter.RunID != "" {
		appendClause("run_id = $%d", filter.RunID)
	}
	if filter.ThreadID != "" {
		appendClause("thread_id = $%d", filter.ThreadID)
	}
	if filter.Namespace != "" {
		appendClause("namespace = $%d", filter.Namespace)
	}
	if filter.OwnerID != "" {
		appendClause("owner_id = $%d", filter.OwnerID)
	}
	if filter.Provider != "" {
		appendClause("provider = $%d", filter.Provider)
	}
	if filter.Status != "" {
		appendClause("status = $%d", filter.Status)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, run_id, thread_id, namespace, owner_id, step, step_id, idx, provider, model, routed_model, status,
		 finish_reason, input_tokens, output_tokens, reasoning_tokens, total_tokens, started_at,
		 finished_at, message_ids, error_kind, error_message, provider_request_id, cost_micros,
		 currency, annotations FROM model_attempts `+where+fmt.Sprintf(` ORDER BY run_id, seq LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2),
		append(args, postgresFetchLimit(limit), offset)...)
	if err != nil {
		return Page[ModelAttemptRecord]{}, fmt.Errorf("lebro: list model attempts: %w", postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	page := Page[ModelAttemptRecord]{Records: []ModelAttemptRecord{}}
	for rows.Next() {
		var (
			v           ModelAttemptRecord
			messageIDs  sql.NullString
			annotations sql.NullString
		)
		if err := rows.Scan(
			&v.ID, &v.RunID, &v.ThreadID, &v.Namespace, &v.OwnerID, &v.Step, &v.StepID, &v.Index,
			&v.Provider, &v.Model, &v.RoutedModel, &v.Status, &v.FinishReason,
			&v.Usage.InputTokens, &v.Usage.OutputTokens, &v.Usage.ReasoningTokens, &v.Usage.TotalTokens,
			&v.StartedAt, &v.FinishedAt, &messageIDs,
			&v.ErrorKind, &v.ErrorMessage, &v.ProviderRequestID, &v.CostMicros,
			&v.Currency, &annotations,
		); err != nil {
			return Page[ModelAttemptRecord]{}, fmt.Errorf("lebro: scan model attempt: %w", postgresError(err))
		}
		v.StartedAt, v.FinishedAt = v.StartedAt.UTC(), v.FinishedAt.UTC()
		if messageIDs.Valid && messageIDs.String != "" && messageIDs.String != "null" {
			if err := json.Unmarshal([]byte(messageIDs.String), &v.ProducedMessageIDs); err != nil {
				return Page[ModelAttemptRecord]{}, fmt.Errorf("lebro: decode attempt message IDs: %w", err)
			}
		}
		m, merr := obsParseMetadata(annotations)
		if merr != nil {
			return page, merr
		}
		v.Metadata = m
		page.Records = append(page.Records, v)
	}
	if err := rows.Err(); err != nil {
		return Page[ModelAttemptRecord]{}, fmt.Errorf("lebro: list model attempts: %w", postgresError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}

func (r *postgresRepositories) SaveToolExecutions(ctx context.Context, vs []ToolExecutionRecord) error {
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
		for _, v := range vs {
			var finishedAt any
			if !v.FinishedAt.IsZero() {
				finishedAt = v.FinishedAt.UTC()
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO tool_executions (
				id, run_id, thread_id, namespace, owner_id, step, step_id,
				tool_call_id, tool_id, state, started_at, finished_at,
				error_kind, error_message, annotations
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT (run_id, id) DO NOTHING`,
				v.ID, v.RunID, v.ThreadID, v.Namespace, v.OwnerID, v.Step, v.StepID,
				v.ToolCallID, string(v.ToolID), string(v.State),
				v.StartedAt.UTC(), finishedAt,
				v.ErrorKind, v.ErrorMessage, obsMetadataJSON(v.Metadata),
			); err != nil {
				return fmt.Errorf("lebro: save tool execution %q: %w", v.ID, postgresError(err))
			}
		}
		return nil
	})
}

func (r *postgresRepositories) ListToolExecutions(ctx context.Context, filter ToolExecutionFilter, p PageRequest) (Page[ToolExecutionRecord], error) {
	if err := ctx.Err(); err != nil {
		return Page[ToolExecutionRecord]{}, err
	}
	offset, limit, err := sqlPageBounds(p)
	if err != nil {
		return Page[ToolExecutionRecord]{}, err
	}
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 6)
	appendClause := func(format string, value any) {
		clauses = append(clauses, fmt.Sprintf(format, len(args)+1))
		args = append(args, value)
	}
	if filter.RunID != "" {
		appendClause("run_id = $%d", filter.RunID)
	}
	if filter.ThreadID != "" {
		appendClause("thread_id = $%d", filter.ThreadID)
	}
	if filter.Namespace != "" {
		appendClause("namespace = $%d", filter.Namespace)
	}
	if filter.OwnerID != "" {
		appendClause("owner_id = $%d", filter.OwnerID)
	}
	if filter.ToolID != "" {
		appendClause("tool_id = $%d", filter.ToolID)
	}
	if filter.State != "" {
		appendClause("state = $%d", filter.State)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, run_id, thread_id, namespace, owner_id, step, step_id, tool_call_id, tool_id, state, started_at,
		 finished_at, error_kind, error_message, annotations FROM tool_executions `+where+fmt.Sprintf(` ORDER BY run_id, seq LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2),
		append(args, postgresFetchLimit(limit), offset)...)
	if err != nil {
		return Page[ToolExecutionRecord]{}, fmt.Errorf("lebro: list tool executions: %w", postgresError(err))
	}
	defer func() { _ = rows.Close() }()
	page := Page[ToolExecutionRecord]{Records: []ToolExecutionRecord{}}
	for rows.Next() {
		var (
			v           ToolExecutionRecord
			finishedAt  sql.NullTime
			annotations sql.NullString
		)
		if err := rows.Scan(
			&v.ID, &v.RunID, &v.ThreadID, &v.Namespace, &v.OwnerID, &v.Step, &v.StepID,
			&v.ToolCallID, &v.ToolID, &v.State, &v.StartedAt, &finishedAt,
			&v.ErrorKind, &v.ErrorMessage, &annotations,
		); err != nil {
			return Page[ToolExecutionRecord]{}, fmt.Errorf("lebro: scan tool execution: %w", postgresError(err))
		}
		v.StartedAt = v.StartedAt.UTC()
		if finishedAt.Valid {
			v.FinishedAt = finishedAt.Time.UTC()
		}
		m, merr := obsParseMetadata(annotations)
		if merr != nil {
			return page, merr
		}
		v.Metadata = m
		page.Records = append(page.Records, v)
	}
	if err := rows.Err(); err != nil {
		return Page[ToolExecutionRecord]{}, fmt.Errorf("lebro: list tool executions: %w", postgresError(err))
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextCursor = strconv.Itoa(offset + limit)
	}
	return page, nil
}
