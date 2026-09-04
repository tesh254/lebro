package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tesh254/lebro"
)

// maxRequestBodyBytes bounds a decoded request body. Without a bound, a single
// client can make the server allocate without limit. One megabyte is far above
// a realistic run request and far below a memory problem.
const maxRequestBodyBytes = 1 << 20

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	agents, workflows := s.counts()
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:    "ok",
		Agents:    agents,
		Workflows: workflows,
	})
}

func (s *Server) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, AgentListResponse{Agents: s.agentSummaries()})
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, WorkflowListResponse{Workflows: s.workflowSummaries()})
}

func (s *Server) handleAgentRun(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.lookupAgent(r.PathValue("id"))
	if !ok {
		writeError(w, r, ErrorCodeNotFound)
		return
	}

	request, err := decodeJSON[RunRequest](r)
	if err != nil {
		writeError(w, r, ErrorCodeInvalidRequest)
		return
	}

	input, ok := s.runInput(w, r, request)
	if !ok {
		return
	}

	result, err := agent.Run(r.Context(), input)
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runResponseFromResult(result, s.config.Redactor))
}

func (s *Server) handleWorkflowRun(w http.ResponseWriter, r *http.Request) {
	workflow, ok := s.lookupWorkflow(r.PathValue("id"))
	if !ok {
		writeError(w, r, ErrorCodeNotFound)
		return
	}

	request, err := decodeJSON[WorkflowRunRequest](r)
	if err != nil {
		writeError(w, r, ErrorCodeInvalidRequest)
		return
	}

	// Validate before starting the run so a rejected input never creates a run
	// record, and so the caller gets a validation error rather than a step
	// failure for a mistake made before any step executed.
	if err := workflow.ValidateInput(request.Input); err != nil {
		writeError(w, r, ErrorCodeInvalidInput)
		return
	}

	threadID, ok := s.threadIDFromQuery(w, r)
	if !ok {
		return
	}

	result, err := workflow.Run(r.Context(), lebro.WorkflowRunInput{
		Input:    request.Input,
		ThreadID: threadID,
		Metadata: request.Metadata,
	})
	if err != nil {
		writeRunError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowRunResponseFromResult(result))
}

func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request) {
	threads, _, ok := s.threadStore()
	if !ok {
		writeError(w, r, ErrorCodeNotFound)
		return
	}

	record, err := threads.GetThread(r.Context(), lebro.ThreadID(r.PathValue("id")))
	if err != nil {
		writeRunError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, ThreadResponse{
		ID:        string(record.ID),
		Namespace: record.Namespace,
		OwnerID:   record.OwnerID,
		Metadata:  record.Metadata,
		CreatedAt: record.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: record.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	_, messages, ok := s.threadStore()
	if !ok {
		writeError(w, r, ErrorCodeNotFound)
		return
	}

	page := lebro.PageRequest{Cursor: r.URL.Query().Get("cursor")}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 {
			writeError(w, r, ErrorCodeInvalidRequest)
			return
		}
		page.Limit = limit
	}

	result, err := messages.ListMessages(r.Context(), lebro.ThreadID(r.PathValue("id")), page)
	if err != nil {
		if errors.Is(err, lebro.ErrInvalidPage) {
			writeError(w, r, ErrorCodeInvalidRequest)
			return
		}
		writeRunError(w, r, err)
		return
	}

	response := MessageListResponse{
		Messages:   make([]MessageResponse, 0, len(result.Records)),
		NextCursor: result.NextCursor,
	}
	for _, record := range result.Records {
		response.Messages = append(response.Messages, MessageResponse{
			ID:        record.ID,
			Role:      string(record.Message.Role),
			Content:   record.Message.Content,
			Reasoning: redactedReasoningText(s.config.Redactor, record.Message.Reasoning.Text),
			CreatedAt: record.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	document, err := s.OpenAPI()
	if err != nil {
		writeError(w, r, ErrorCodeInternal)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(document)
}

// runInput builds the agent run input from a decoded request, resolving the
// thread from the query string. It writes the error response and reports false
// when the request cannot be served.
func (s *Server) runInput(w http.ResponseWriter, r *http.Request, request RunRequest) (lebro.RunInput, bool) {
	if err := request.Reasoning.Validate(); err != nil {
		writeError(w, r, ErrorCodeInvalidRequest)
		return lebro.RunInput{}, false
	}
	threadID, ok := s.threadIDFromQuery(w, r)
	if !ok {
		return lebro.RunInput{}, false
	}

	messages := make([]lebro.Message, 0, len(request.Messages))
	for _, message := range request.Messages {
		// The role is fixed here rather than taken from the request: a client
		// that could choose a role could inject a system prompt or forge an
		// assistant turn the model would treat as its own prior output.
		messages = append(messages, lebro.Message{Role: lebro.RoleUser, Content: message.Content})
	}

	return lebro.RunInput{
		Messages:  messages,
		ThreadID:  threadID,
		Metadata:  request.Metadata,
		Reasoning: request.Reasoning,
	}, true
}

// threadIDFromQuery reads the optional thread_id query parameter. Naming a
// thread without a configured Store is rejected rather than ignored: silently
// dropping it would run against no thread while the caller believes their
// conversation is being continued and persisted.
func (s *Server) threadIDFromQuery(w http.ResponseWriter, r *http.Request) (lebro.ThreadID, bool) {
	raw := r.URL.Query().Get("thread_id")
	if raw == "" {
		return "", true
	}
	if _, _, ok := s.threadStore(); !ok {
		writeError(w, r, ErrorCodeInvalidRequest)
		return "", false
	}
	return lebro.ThreadID(raw), true
}

// decodeJSON reads and decodes a bounded request body. An empty body decodes to
// the zero value, so a run with no messages is expressible as an empty body
// rather than requiring "{}". Unknown fields are rejected so a client that
// misspells a field is told, rather than having the value silently ignored.
func decodeJSON[T any](r *http.Request) (T, error) {
	var value T
	if r.Body == nil {
		return value, nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		return value, err
	}
	if int64(len(body)) > maxRequestBodyBytes {
		return value, errors.New("lebro/httpapi: request body too large")
	}
	if len(body) == 0 {
		return value, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	// A body with anything past the first JSON value is malformed; accepting it
	// would let two different payloads produce the same run.
	//
	// This must be a second Decode rather than decoder.More(): More reports
	// whether another value follows *within an array or object being streamed*,
	// so at the top level it returns false for stray trailing bytes and a body
	// like `{}]` would be accepted. Decoding again surfaces both cases — a
	// second value and unparseable trailing bytes — as a non-EOF result.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return value, errors.New("lebro/httpapi: request body contains trailing content after the first JSON value")
	}
	return value, nil
}
