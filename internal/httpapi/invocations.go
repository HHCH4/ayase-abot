package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Abot/internal/agent"
	agentruntime "Abot/internal/agent/runtime"
	"Abot/internal/conversation"
)

func (s *Server) requireRuntime() (*agentruntime.Coordinator, error) {
	if s.runtime == nil {
		return nil, errors.New("Agent Runtime 尚未装配")
	}
	return s.runtime, nil
}

type invocationPayload struct {
	UserID         string                  `json:"user_id"`
	IdempotencyKey string                  `json:"idempotency_key"`
	BotID          string                  `json:"bot_id"`
	ConversationID string                  `json:"conversation_id"`
	WorkspaceID    string                  `json:"workspace_id"`
	TargetPath     string                  `json:"target_path"`
	SessionID      string                  `json:"session_id"`
	ProviderID     string                  `json:"provider_id"`
	ModelID        string                  `json:"model_id"`
	Message        string                  `json:"message"`
	Attachments    []chatAttachmentPayload `json:"attachments"`
	Stream         bool                    `json:"stream"`
}

func (s *Server) createInvocation(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload invocationPayload
	if err := decodeJSONWithLimit(writer, request, &payload, maxChatRequestBytes); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	attachments, err := normalizeChatAttachments(payload.Attachments)
	if err != nil {
		writeError(writer, fmt.Errorf("请求附件无效: %w", err))
		return
	}
	conversationID := strings.TrimSpace(payload.ConversationID)
	if conversationID == "" {
		conversationID = strings.TrimSpace(payload.SessionID)
	}
	if err := s.validateChatAttachmentRefs(request.Context(), payload.UserID, conversationID, attachments); err != nil {
		writeError(writer, fmt.Errorf("请求附件 ref 无效: %w", err))
		return
	}
	if s.conversations != nil && conversationID != "" {
		if _, getErr := s.conversations.Get(request.Context(), payload.UserID, conversationID); getErr != nil {
			writeError(writer, getErr)
			return
		}
	}
	if s.conversations != nil && conversationID == "" {
		created, createErr := s.conversations.Create(request.Context(), conversation.CreateRequest{UserID: payload.UserID})
		if createErr != nil {
			writeError(writer, createErr)
			return
		}
		conversationID = created.ID
	}
	if conversationID == "" {
		conversationID = agent.NewSessionID()
	}
	attachments, err = s.materializeChatAttachments(request.Context(), payload.UserID, conversationID, invocationIdempotencyKey(request, payload.IdempotencyKey), attachments)
	if err != nil {
		writeError(writer, err)
		return
	}
	var workspaceID *string
	if strings.TrimSpace(payload.WorkspaceID) != "" {
		workspaceID = &payload.WorkspaceID
	}
	item, err := runtime.StartInvocation(request.Context(), agent.ChatRequest{
		UserID: payload.UserID, IdempotencyKey: invocationIdempotencyKey(request, payload.IdempotencyKey), BotID: payload.BotID, ConversationID: conversationID, SessionID: conversationID,
		ProviderID: payload.ProviderID, ModelID: payload.ModelID, Message: payload.Message,
		Attachments: attachments, WorkspaceID: workspaceID, TargetPath: strings.TrimSpace(payload.TargetPath), Stream: true,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.Header().Set("Location", "/api/v1/invocations/"+item.ID)
	writeJSON(writer, http.StatusAccepted, item)
}

func invocationIdempotencyKey(request *http.Request, bodyValue string) string {
	if request != nil {
		if value := strings.TrimSpace(request.Header.Get("Idempotency-Key")); value != "" {
			return value
		}
	}
	return strings.TrimSpace(bodyValue)
}

func (s *Server) listInvocations(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := runtime.ListInvocations(request.Context(), request.URL.Query().Get("user_id"), nil)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"invocations": items})
}

func (s *Server) getInvocation(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.GetInvocation(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) getInvocationResult(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	report, err := runtime.GetCompletionReport(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func (s *Server) getInvocationBaseline(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	baseline, err := runtime.GetWorkspaceBaseline(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, baseline)
}

type invocationPlanPayload struct {
	Revision      int64                   `json:"revision"`
	Status        agentruntime.PlanStatus `json:"status"`
	CurrentStepID string                  `json:"current_step_id"`
	Blocker       string                  `json:"blocker"`
	Steps         []agentruntime.PlanStep `json:"steps"`
	Reason        string                  `json:"reason"`
}

func (s *Server) getInvocationPlan(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	plan, err := runtime.GetTaskPlan(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, plan)
}

func (s *Server) updateInvocationPlan(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload invocationPlanPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	plan, err := runtime.UpdateTaskPlan(request.Context(), request.PathValue("id"), agentruntime.TaskPlan{
		Revision: payload.Revision, Status: payload.Status, CurrentStepID: payload.CurrentStepID,
		Blocker: payload.Blocker, Steps: payload.Steps,
	}, payload.Reason)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, plan)
}

type invocationContinuationPayload struct {
	ExpectedPlanRevision int64                                     `json:"expected_plan_revision"`
	Decision             agentruntime.WorkflowContinuationDecision `json:"decision"`
	StepID               string                                    `json:"step_id"`
	Message              string                                    `json:"message,omitempty"`
	IdempotencyKey       string                                    `json:"idempotency_key,omitempty"`
	Evidence             []agentruntime.EvidenceRef                `json:"evidence,omitempty"`
}

// continueInvocation records an explicit user decision and returns the new
// child task. The failed source task remains immutable; no source output,
// inline attachments or tool arguments are echoed in this response.
func (s *Server) continueInvocation(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload invocationContinuationPayload
	if err := decodeSingleJSONWithLimit(writer, request, &payload, 2*1024*1024); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	child, err := runtime.ContinueInterruptedWorkflow(request.Context(), request.PathValue("id"), agentruntime.WorkflowContinuationRequest{
		ExpectedPlanRevision: payload.ExpectedPlanRevision,
		Decision:             payload.Decision,
		StepID:               payload.StepID,
		Message:              payload.Message,
		IdempotencyKey:       idempotencyKey,
		Evidence:             payload.Evidence,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.Header().Set("Location", "/api/v1/invocations/"+child.ID)
	writeJSON(writer, http.StatusAccepted, child)
}

type invocationContractPayload struct {
	Version            int64                             `json:"version"`
	TaskType           agentruntime.TaskType             `json:"task_type"`
	Goal               string                            `json:"goal"`
	RequestedOutcome   string                            `json:"requested_outcome"`
	AcceptanceCriteria []agentruntime.Criterion          `json:"acceptance_criteria"`
	Constraints        []agentruntime.Constraint         `json:"constraints"`
	NonGoals           []string                          `json:"non_goals"`
	MutationAllowed    bool                              `json:"mutation_allowed"`
	ValidationRequired bool                              `json:"validation_required"`
	ExternalActions    []agentruntime.ExternalActionRule `json:"external_actions"`
	SourceMessageIDs   []string                          `json:"source_message_ids"`
	Reason             string                            `json:"reason"`
}

func (s *Server) getInvocationContract(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	contract, err := runtime.GetTaskContract(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, contract)
}

func (s *Server) listInvocationContracts(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	contracts, err := runtime.ListTaskContracts(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"contracts": contracts})
}

func (s *Server) updateInvocationContract(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload invocationContractPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	contract, err := runtime.UpdateTaskContract(request.Context(), request.PathValue("id"), agentruntime.TaskContract{
		Version: payload.Version, TaskType: payload.TaskType, Goal: payload.Goal, RequestedOutcome: payload.RequestedOutcome,
		AcceptanceCriteria: payload.AcceptanceCriteria, Constraints: payload.Constraints, NonGoals: payload.NonGoals,
		MutationAllowed: payload.MutationAllowed, ValidationRequired: payload.ValidationRequired,
		ExternalActions: payload.ExternalActions, SourceMessageIDs: payload.SourceMessageIDs,
	}, payload.Reason)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, contract)
}

func (s *Server) listInvocationEvents(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	after, parseErr := parseSequence(request)
	if parseErr != nil {
		writeError(writer, parseErr)
		return
	}
	items, err := runtime.ListEvents(request.Context(), request.PathValue("id"), after, 500)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"events": items})
}

func (s *Server) getInvocationTrace(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	trace, err := runtime.GetInvocationTrace(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, trace)
}

func (s *Server) getInvocationUsage(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	usage, err := runtime.GetInvocationUsage(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, usage)
}

func (s *Server) listInvocationToolCalls(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := runtime.ListToolCalls(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"tool_calls": items})
}

func (s *Server) listInvocationContextManifests(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := runtime.ListContextManifests(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"manifests": items})
}

func (s *Server) getInvocationToolSet(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.GetToolSetSnapshot(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) getInvocationModelCapabilities(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.GetModelCapabilitySnapshot(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) reconfirmInvocationInstructions(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.ReconfirmInstructions(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) getInvocationRuntimeSnapshot(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.GetRuntimeSnapshot(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

type invocationWorkingSetPayload struct {
	Revision int64                         `json:"revision"`
	Items    []agentruntime.WorkingSetItem `json:"items"`
	Reason   string                        `json:"reason"`
}

func (s *Server) getInvocationWorkingSet(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.GetWorkingSet(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) updateInvocationWorkingSet(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload invocationWorkingSetPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	item, err := runtime.UpdateWorkingSet(request.Context(), request.PathValue("id"), agentruntime.WorkingSet{Revision: payload.Revision, Items: payload.Items}, payload.Reason)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

type verificationRunPayload struct {
	ID           string                          `json:"id"`
	PlanStepID   string                          `json:"plan_step_id"`
	Kind         string                          `json:"kind"`
	Command      string                          `json:"command"`
	Status       agentruntime.VerificationStatus `json:"status"`
	ExitCode     *int                            `json:"exit_code"`
	Summary      string                          `json:"summary"`
	OutputRef    string                          `json:"output_ref"`
	OutputDigest string                          `json:"output_digest"`
	Revision     int64                           `json:"revision"`
	Reason       string                          `json:"reason"`
}

func (s *Server) listInvocationVerifications(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := runtime.ListVerificationRuns(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"verifications": items})
}

func (s *Server) createInvocationVerification(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload verificationRunPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	item, err := runtime.CreateVerificationRun(request.Context(), agentruntime.VerificationRun{InvocationID: request.PathValue("id"), ID: payload.ID, PlanStepID: payload.PlanStepID, Kind: payload.Kind, Command: payload.Command, Status: payload.Status, ExitCode: payload.ExitCode, Summary: payload.Summary, OutputRef: payload.OutputRef, OutputDigest: payload.OutputDigest, Revision: payload.Revision})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, item)
}

func (s *Server) updateInvocationVerification(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload verificationRunPayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	item, err := runtime.UpdateVerificationRun(request.Context(), agentruntime.VerificationRun{ID: request.PathValue("verification_id"), PlanStepID: payload.PlanStepID, Kind: payload.Kind, Command: payload.Command, Status: payload.Status, ExitCode: payload.ExitCode, Summary: payload.Summary, OutputRef: payload.OutputRef, OutputDigest: payload.OutputDigest}, payload.Revision, payload.Reason)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (s *Server) streamInvocation(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, errors.New("当前 HTTP 服务不支持 SSE"))
		return
	}
	after, parseErr := parseSequence(request)
	if parseErr != nil {
		writeError(writer, parseErr)
		return
	}
	backlog, live, unsubscribe, err := runtime.Subscribe(request.Context(), request.PathValue("id"), after)
	if err != nil {
		writeError(writer, err)
		return
	}
	defer unsubscribe()
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	last := after
	sentTerminal := false
	send := func(event agentruntime.AgentEvent) bool {
		if event.Sequence <= last {
			return true
		}
		if err := writeRuntimeSSE(writer, event); err != nil {
			return false
		}
		last = event.Sequence
		if event.Type == agentruntime.EventInvocationCompleted || event.Type == agentruntime.EventInvocationFailed || event.Type == agentruntime.EventInvocationCancelled || event.Type == agentruntime.EventInvocationExpired {
			sentTerminal = true
		}
		flusher.Flush()
		return true
	}
	for _, event := range backlog {
		if !send(event) {
			return
		}
	}
	if sentTerminal {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case event, open := <-live:
			if !open {
				return
			}
			if !send(event) || sentTerminal {
				return
			}
		case <-ticker.C:
			item, getErr := runtime.GetInvocation(request.Context(), request.PathValue("id"))
			if getErr != nil || item.Status.Terminal() {
				// The terminal event is normally delivered through live. The
				// replay closes the stream even if the observer joined late.
				if getErr == nil && !sentTerminal {
					if events, listErr := runtime.ListEvents(request.Context(), item.ID, last, 20); listErr == nil {
						for _, event := range events {
							if !send(event) {
								return
							}
						}
					}
				}
				if sentTerminal || getErr != nil {
					return
				}
			}
		}
	}
}

func (s *Server) cancelInvocation(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.CancelInvocation(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

type invocationResumePayload struct {
	WaitID         string                              `json:"wait_id"`
	Name           string                              `json:"name,omitempty"`
	Response       map[string]any                      `json:"response"`
	Responses      []agentruntime.InvocationResumeItem `json:"responses,omitempty"`
	BoundaryID     string                              `json:"boundary_id,omitempty"`
	IdempotencyKey string                              `json:"idempotency_key,omitempty"`
}

func (s *Server) resumeInvocation(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload invocationResumePayload
	if err := decodeJSONWithLimit(writer, request, &payload, 512<<10); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	item, err := runtime.ResumeInvocation(request.Context(), request.PathValue("id"), agentruntime.InvocationResumeRequest{
		WaitID: payload.WaitID, Name: payload.Name, Response: payload.Response, Responses: payload.Responses, BoundaryID: payload.BoundaryID, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, item)
}

type invocationToolStatusPayload struct {
	WaitID string `json:"wait_id"`
}

type invocationToolStatusReconcilePayload struct {
	WaitID  string   `json:"wait_id,omitempty"`
	WaitIDs []string `json:"wait_ids,omitempty"`
}

// queryInvocationToolStatus asks the configured external Tool Runtime for a
// proof about one current long-running wait. It is intentionally read-only:
// even a succeeded proof does not submit a FunctionResponse or change the
// Invocation; the caller must explicitly choose a bounded resume payload.
func (s *Server) queryInvocationToolStatus(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	waitID := ""
	if request != nil && request.URL != nil {
		waitID = strings.TrimSpace(request.URL.Query().Get("wait_id"))
	}
	if request != nil && request.Method == http.MethodPost {
		var payload invocationToolStatusPayload
		if err := decodeJSONWithLimit(writer, request, &payload, 8<<10); err != nil {
			writeError(writer, fmt.Errorf("请求体无效: %w", err))
			return
		}
		if strings.TrimSpace(payload.WaitID) != "" {
			waitID = strings.TrimSpace(payload.WaitID)
		}
	}
	status, err := runtime.QueryLongRunningToolStatus(request.Context(), request.PathValue("id"), waitID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

// reconcileInvocationToolStatus is the explicit state-changing companion to
// queryInvocationToolStatus. It never accepts a caller-supplied result: the
// Runtime may resume only when the authenticated Tool Runtime proof itself
// contains a bounded, digest-checked result body.
func (s *Server) reconcileInvocationToolStatus(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload invocationToolStatusReconcilePayload
	if err := decodeJSONWithLimit(writer, request, &payload, 8<<10); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	if strings.TrimSpace(payload.WaitID) != "" && len(payload.WaitIDs) > 0 {
		writeError(writer, fmt.Errorf("%w: wait_id 不能与 wait_ids 同时提供", agentruntime.ErrInvalidRuntimeLongRunningToolStatus))
		return
	}
	if len(payload.WaitIDs) > 0 {
		result, err := runtime.ReconcileLongRunningToolStatuses(request.Context(), request.PathValue("id"), payload.WaitIDs)
		if err != nil {
			writeError(writer, err)
			return
		}
		status := http.StatusOK
		if result.Resumed {
			status = http.StatusAccepted
		}
		writeJSON(writer, status, result)
		return
	}
	result, err := runtime.ReconcileLongRunningToolStatus(request.Context(), request.PathValue("id"), payload.WaitID)
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusOK
	if result.Resumed {
		status = http.StatusAccepted
	}
	writeJSON(writer, status, result)
}

type invocationConfigMigrationPayload struct {
	ExpectedConfigSnapshotDigest string `json:"expected_config_snapshot_digest"`
	IdempotencyKey               string `json:"idempotency_key,omitempty"`
}

// migrateInvocationConfig is an explicit, user-confirmed migration boundary.
// The request carries only the digest observed by the client; the Runtime
// resolves the current provider/model/tool configuration and persists the new
// snapshot atomically with a metadata-only audit event.
func (s *Server) migrateInvocationConfig(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload invocationConfigMigrationPayload
	if err := decodeJSONWithLimit(writer, request, &payload, 8<<10); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	item, err := runtime.MigrateRuntimeConfig(request.Context(), request.PathValue("id"), payload.ExpectedConfigSnapshotDigest, idempotencyKey)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, item)
}

func (s *Server) listApprovals(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	items, err := runtime.ListApprovals(request.Context(), request.URL.Query().Get("invocation_id"), agentruntime.ApprovalStatus(request.URL.Query().Get("status")))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"approvals": items})
}

type approvalResolvePayload struct {
	// Approved is kept for the first API shape. Decision is the canonical
	// wire value used by the runtime design and lets clients express an
	// explicit reject without relying on a missing boolean field.
	Approved *bool  `json:"approved"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func (s *Server) resolveApproval(writer http.ResponseWriter, request *http.Request) {
	runtime, err := s.requireRuntime()
	if err != nil {
		writeError(writer, err)
		return
	}
	var payload approvalResolvePayload
	if err := decodeJSON(writer, request, &payload); err != nil {
		writeError(writer, fmt.Errorf("请求体无效: %w", err))
		return
	}
	approved, err := parseApprovalDecision(payload)
	if err != nil {
		writeError(writer, err)
		return
	}
	item, err := runtime.ResolveApproval(request.Context(), request.PathValue("id"), approved, strings.TrimSpace(payload.Reason))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, item)
}

func parseApprovalDecision(payload approvalResolvePayload) (bool, error) {
	decision := strings.ToLower(strings.TrimSpace(payload.Decision))
	if decision != "" {
		switch decision {
		case "approve", "approved", "allow", "yes", "true":
			if payload.Approved != nil && !*payload.Approved {
				return false, errors.New("approved 与 decision 冲突")
			}
			return true, nil
		case "reject", "rejected", "deny", "no", "false":
			if payload.Approved != nil && *payload.Approved {
				return false, errors.New("approved 与 decision 冲突")
			}
			return false, nil
		default:
			return false, errors.New("decision 必须是 approve 或 reject")
		}
	}
	if payload.Approved == nil {
		return false, errors.New("必须提供 approved 或 decision")
	}
	return *payload.Approved, nil
}

func parseSequence(request *http.Request) (int64, error) {
	value := strings.TrimSpace(request.URL.Query().Get("after"))
	if value == "" {
		value = strings.TrimSpace(request.Header.Get("Last-Event-ID"))
	}
	if value == "" {
		return 0, nil
	}
	sequence, err := strconv.ParseInt(value, 10, 64)
	if err != nil || sequence < 0 {
		return 0, errors.New("after 必须是非负整数")
	}
	return sequence, nil
}

func writeRuntimeSSE(writer http.ResponseWriter, event agentruntime.AgentEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, data)
	return err
}

// streamRuntimeChat adapts the durable AgentEvent stream to the historical
// /chat message/done/error SSE shape. New clients should consume
// /invocations/{id}/stream directly; keeping this projection lets existing
// WebUI integrations upgrade without losing task durability.
func (s *Server) streamRuntimeChat(writer http.ResponseWriter, request *http.Request, invocation agentruntime.Invocation) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, errors.New("当前 HTTP 服务不支持 SSE"))
		return
	}
	backlog, live, unsubscribe, err := s.runtime.Subscribe(request.Context(), invocation.ID, 0)
	if err != nil {
		writeError(writer, err)
		return
	}
	defer unsubscribe()
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	var finalText strings.Builder
	var sawDelta bool
	last := int64(0)
	terminal := false
	consume := func(event agentruntime.AgentEvent) bool {
		if event.Sequence <= last {
			return true
		}
		last = event.Sequence
		textValue, _ := event.Data["text"].(string)
		switch event.Type {
		case agentruntime.EventAssistantDelta:
			if textValue != "" {
				sawDelta = true
				finalText.WriteString(textValue)
				if err := writeSSE(writer, "message", map[string]any{"type": "message", "delta": textValue}); err != nil {
					return false
				}
				flusher.Flush()
			}
		case agentruntime.EventAssistantMessage:
			if textValue != "" {
				if !sawDelta {
					finalText.Reset()
					finalText.WriteString(textValue)
					if err := writeSSE(writer, "message", map[string]any{"type": "message", "delta": textValue}); err != nil {
						return false
					}
					flusher.Flush()
				} else if finalText.Len() == 0 {
					finalText.WriteString(textValue)
				}
			}
		case agentruntime.EventApprovalRequested:
			data := cloneRuntimeEventData(event.Data)
			data["type"] = "approval"
			data["invocation_id"] = invocation.ID
			if err := writeSSE(writer, "approval", data); err != nil {
				return false
			}
			flusher.Flush()
		case agentruntime.EventCommandOutput, agentruntime.EventToolRequested, agentruntime.EventToolCompleted,
			agentruntime.EventToolFailed, agentruntime.EventApprovalResolved, agentruntime.EventContextCompacted,
			agentruntime.EventContextCompactionFailed,
			agentruntime.EventUsageUpdated, agentruntime.EventPlanUpdated, agentruntime.EventArtifactUpdated,
			agentruntime.EventToolStarted, agentruntime.EventToolOutput, agentruntime.EventModelStarted,
			agentruntime.EventModelCompleted, agentruntime.EventModelRetrying, agentruntime.EventModelFailed,
			agentruntime.EventApprovalExpired, agentruntime.EventInvocationCancelling, agentruntime.EventRuntimeNotice, agentruntime.EventADK:
			data := map[string]any{"type": "runtime", "event": event.Type, "data": event.Data}
			if err := writeSSE(writer, "runtime", data); err != nil {
				return false
			}
			flusher.Flush()
		case agentruntime.EventInvocationCompleted:
			terminal = true
			if err := writeSSE(writer, "done", map[string]any{"type": "done", "text": finalText.String(), "conversation_id": invocation.ConversationID, "session_id": invocation.SessionID, "invocation_id": invocation.ID}); err != nil {
				return false
			}
			flusher.Flush()
		case agentruntime.EventInvocationFailed, agentruntime.EventInvocationCancelled, agentruntime.EventInvocationExpired:
			terminal = true
			message := "Agent 运行失败"
			if event.Type == agentruntime.EventInvocationCancelled {
				message = "Agent 已取消"
			} else if event.Type == agentruntime.EventInvocationExpired {
				message = "Agent 等待已过期"
			}
			if value, ok := event.Data["error"].(string); ok && strings.TrimSpace(value) != "" {
				message = value
			}
			if err := writeSSE(writer, "error", map[string]any{"type": "error", "error": message, "invocation_id": invocation.ID}); err != nil {
				return false
			}
			flusher.Flush()
		}
		return true
	}
	for _, event := range backlog {
		if !consume(event) || terminal {
			return
		}
	}
	for {
		select {
		case <-request.Context().Done():
			return
		case event, open := <-live:
			if !open || !consume(event) || terminal {
				return
			}
		}
	}
}

// waitRuntimeChat is the non-stream compatibility projection. It waits on
// durable events rather than running a second Kernel loop, so both endpoint
// shapes observe exactly one Invocation.
func (s *Server) waitRuntimeChat(writer http.ResponseWriter, request *http.Request, invocation agentruntime.Invocation) {
	backlog, live, unsubscribe, err := s.runtime.Subscribe(request.Context(), invocation.ID, 0)
	if err != nil {
		writeError(writer, err)
		return
	}
	defer unsubscribe()
	var finalText strings.Builder
	last := int64(0)
	consume := func(event agentruntime.AgentEvent) (bool, string) {
		if event.Sequence <= last {
			return false, ""
		}
		last = event.Sequence
		switch event.Type {
		case agentruntime.EventAssistantDelta:
			if textValue, ok := event.Data["text"].(string); ok {
				finalText.WriteString(textValue)
			}
		case agentruntime.EventAssistantMessage:
			if textValue, ok := event.Data["text"].(string); ok && finalText.Len() == 0 {
				finalText.WriteString(textValue)
			}
		case agentruntime.EventInvocationCompleted:
			return true, ""
		case agentruntime.EventInvocationFailed, agentruntime.EventInvocationCancelled, agentruntime.EventInvocationExpired:
			if message, ok := event.Data["error"].(string); ok && message != "" {
				return true, message
			}
			return true, "Agent 运行失败"
		}
		return false, ""
	}
	for _, event := range backlog {
		if done, failure := consume(event); done {
			if failure != "" {
				writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": failure, "invocation_id": invocation.ID})
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"text": finalText.String(), "conversation_id": invocation.ConversationID, "session_id": invocation.SessionID, "invocation_id": invocation.ID})
			return
		}
	}
	for {
		select {
		case <-request.Context().Done():
			writeError(writer, request.Context().Err())
			return
		case event, open := <-live:
			if !open {
				writeError(writer, errors.New("Agent Runtime 事件流已关闭"))
				return
			}
			if done, failure := consume(event); done {
				if failure != "" {
					writeJSON(writer, http.StatusInternalServerError, map[string]any{"error": failure, "invocation_id": invocation.ID})
					return
				}
				writeJSON(writer, http.StatusOK, map[string]any{"text": finalText.String(), "conversation_id": invocation.ConversationID, "session_id": invocation.SessionID, "invocation_id": invocation.ID})
				return
			}
		}
	}
}

func cloneRuntimeEventData(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}
