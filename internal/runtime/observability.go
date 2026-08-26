package runtime

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Shared validation for durable observability records. Every adapter applies
// the same checks so MemoryStore, SQLiteStore, and PostgresStore reject the
// same inputs with equivalent errors. Unlike messages, records do not require
// their thread to exist yet: failed runs persist diagnostics before any
// transcript is written, and ThreadID may be empty entirely.

func validateObservabilityIdentity(record string, id string, runID RunID) error {
	if id == "" {
		return fmt.Errorf("lebro: %s ID is required", record)
	}
	if runID == "" {
		return fmt.Errorf("lebro: %s run ID is required", record)
	}
	return nil
}

// validateModelAttemptRecord checks one attempt record in place.
func validateModelAttemptRecord(v ModelAttemptRecord) error {
	if err := validateObservabilityIdentity("model attempt", v.ID, v.RunID); err != nil {
		return err
	}
	if v.Index <= 0 {
		return fmt.Errorf("lebro: model attempt %q index must be positive", v.ID)
	}
	switch v.Status {
	case ModelAttemptSuccess, ModelAttemptFallback, ModelAttemptFailed, ModelAttemptCancelled:
	default:
		return fmt.Errorf("lebro: model attempt %q has invalid status %q", v.ID, v.Status)
	}
	if v.StartedAt.IsZero() || v.FinishedAt.IsZero() {
		return errors.New("lebro: model attempt timestamps are required")
	}
	if v.FinishedAt.Before(v.StartedAt) {
		return fmt.Errorf("lebro: model attempt %q finishes before it starts", v.ID)
	}
	if err := validateUsage(v.Usage); err != nil {
		return fmt.Errorf("lebro: model attempt %q usage: %w", v.ID, err)
	}
	if err := validateFinishReasonValue(v.FinishReason); err != nil {
		return fmt.Errorf("lebro: model attempt %q: %w", v.ID, err)
	}
	for i, messageID := range v.ProducedMessageIDs {
		if messageID == "" {
			return fmt.Errorf("lebro: model attempt %q produced message ID %d is empty", v.ID, i)
		}
	}
	return v.Metadata.Validate()
}

// validateToolExecutionRecord checks one tool-execution record in place.
func validateToolExecutionRecord(v ToolExecutionRecord) error {
	if err := validateObservabilityIdentity("tool execution", v.ID, v.RunID); err != nil {
		return err
	}
	if v.ToolCallID == "" {
		return fmt.Errorf("lebro: tool execution %q requires a tool call ID", v.ID)
	}
	if v.ToolID == "" {
		return fmt.Errorf("lebro: tool execution %q requires a tool ID", v.ID)
	}
	switch v.State {
	case ToolExecutionSucceeded, ToolExecutionInvalidInput, ToolExecutionInvalidOutput,
		ToolExecutionHandlerError, ToolExecutionPanicked, ToolExecutionCancelled,
		ToolExecutionNotFound, ToolExecutionUnauthorized:
	default:
		return fmt.Errorf("lebro: tool execution %q has invalid state %q", v.ID, v.State)
	}
	if v.StartedAt.IsZero() {
		return errors.New("lebro: tool execution start timestamp is required")
	}
	if !v.FinishedAt.IsZero() && v.FinishedAt.Before(v.StartedAt) {
		return fmt.Errorf("lebro: tool execution %q finishes before it starts", v.ID)
	}
	return v.Metadata.Validate()
}

// validateRunEventRecord checks one event record in place.
func validateRunEventRecord(v RunEventRecord) error {
	if err := validateObservabilityIdentity("run event", v.ID, v.RunID); err != nil {
		return err
	}
	if v.Sequence <= 0 {
		return fmt.Errorf("lebro: run event %q sequence must be positive", v.ID)
	}
	if v.Type == "" {
		return fmt.Errorf("lebro: run event %q type is required", v.ID)
	}
	if v.Timestamp.IsZero() {
		return errors.New("lebro: run event timestamp is required")
	}
	if v.Plugin != nil && v.Plugin.ID == "" {
		return fmt.Errorf("lebro: run event %q plugin attribution requires an ID", v.ID)
	}
	if err := validateJSON(v.Payload); err != nil {
		return fmt.Errorf("lebro: run event %q payload: %w", v.ID, err)
	}
	return v.Metadata.Validate()
}

func validateUsage(usage ModelUsage) error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningTokens < 0 || usage.TotalTokens < 0 {
		return errors.New("token counts must not be negative")
	}
	return nil
}

func validateFinishReasonValue(reason FinishReason) error {
	if reason == "" {
		return nil
	}
	if !validFinishReason(reason) {
		return fmt.Errorf("invalid finish reason %q", reason)
	}
	return nil
}

// withinEventRange reports whether ts falls inside an inclusive-from,
// exclusive-to filter window.
func withinEventRange(ts time.Time, from, to time.Time) bool {
	if !from.IsZero() && ts.Before(from) {
		return false
	}
	if !to.IsZero() && !ts.Before(to) {
		return false
	}
	return true
}

// obsEventExtras flattens nullable event fields into column values. Plugin
// attribution columns are NOT NULL DEFAULT ”, so an absent plugin binds
// empty strings rather than SQL NULL.
func obsEventExtras(v RunEventRecord) (payload any, pluginID, pluginVersion, pluginAction, pluginOutcome any) {
	payload = nullableJSON(v.Payload)
	if v.Plugin == nil {
		return payload, "", "", "", ""
	}
	return payload,
		v.Plugin.ID,
		v.Plugin.Version,
		v.Plugin.Action,
		v.Plugin.Outcome
}

// nullableJSON encodes a raw JSON value as a nullable TEXT column value.
func nullableJSON(v json.RawMessage) any {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return string(v)
}

// obsMetadataJSON encodes validated namespaced metadata as a nullable JSON
// object column value.
func obsMetadataJSON(m Metadata) any {
	if len(m) == 0 {
		return nil
	}
	encoded, err := json.Marshal(map[string]json.RawMessage(m))
	if err != nil {
		// Validation already guaranteed JSON-encodable values.
		return nil
	}
	return string(encoded)
}

// obsParseMetadata decodes a nullable JSON-object metadata column.
func obsParseMetadata(v sql.NullString) (Metadata, error) {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil, nil
	}
	var decoded Metadata
	if err := json.Unmarshal([]byte(v.String), &decoded); err != nil {
		return nil, fmt.Errorf("lebro: decode record metadata: %w", err)
	}
	return decoded, nil
}

// obsStringArray marshals a string slice into a nullable JSON array column
// value; a nil slice becomes NULL. Marshaling a string slice cannot fail.
func obsStringArray(values []string) any {
	if len(values) == 0 {
		return nil
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}
