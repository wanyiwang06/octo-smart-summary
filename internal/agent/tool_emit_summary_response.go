package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	SummaryResultClarification        = "clarification"
	SummaryResultWorkflowConfirmation = "workflow_confirmation"
	SummaryResultWorkflowStarted      = "workflow_started"
	SummaryResultWorkflowCompleted    = "workflow_completed"
	SummaryResultAgentPreview         = "agent_preview"
	SummaryResultAgentRevision        = "agent_revision"
	SummaryResultExplanation          = "explanation"
	SummaryResultError                = "error"
)

var validSummaryResultTypes = map[string]struct{}{
	SummaryResultClarification:        {},
	SummaryResultWorkflowConfirmation: {},
	SummaryResultWorkflowStarted:      {},
	SummaryResultWorkflowCompleted:    {},
	SummaryResultAgentPreview:         {},
	SummaryResultAgentRevision:        {},
	SummaryResultExplanation:          {},
	SummaryResultError:                {},
}

// SummaryResponsePayload is the validated, typed shape carried by
// emit_summary_response. It is exported so the HTTP/application layer can
// perform authoritative checks (task ownership, proposal version, latest
// preview version) after the Agent loop returns it.
type SummaryResponsePayload struct {
	ResultType      string                     `json:"result_type"`
	Reply           string                     `json:"reply"`
	ExecutionTarget string                     `json:"execution_target,omitempty"`
	Workflow        *SummaryResponseWorkflow   `json:"workflow,omitempty"`
	Preview         *SummaryResponsePreview    `json:"preview,omitempty"`
	Confirmation    map[string]json.RawMessage `json:"confirmation,omitempty"`
	MissingFields   []string                   `json:"missing_fields,omitempty"`
}

type SummaryResponseWorkflow struct {
	TaskID int64  `json:"task_id"`
	Status string `json:"status"`
	Saved  bool   `json:"saved"`
}

type SummaryResponsePreview struct {
	Content         string   `json:"content"`
	Version         int      `json:"version"`
	ParentMessageID int64    `json:"parent_message_id,omitempty"`
	Assumptions     []string `json:"assumptions,omitempty"`
}

type allowedSummaryResultTypesContextKey struct{}

// WithAllowedSummaryResultTypes constrains which result types the terminal tool
// may accept for this request. Absence means structural validation only; an
// explicitly empty allowlist denies every result. The application layer should
// derive this list from trusted route/session state, never from model output.
func WithAllowedSummaryResultTypes(ctx context.Context, resultTypes ...string) context.Context {
	allowed := make(map[string]struct{}, len(resultTypes))
	for _, resultType := range resultTypes {
		resultType = strings.TrimSpace(resultType)
		if resultType != "" {
			allowed[resultType] = struct{}{}
		}
	}
	return context.WithValue(ctx, allowedSummaryResultTypesContextKey{}, allowed)
}

// EmitSummaryResponseTool returns the only successful termination mechanism
// used by the summary_workspace profile. It validates shape only; authoritative
// workflow/proposal/preview state checks remain an application-layer concern.
func EmitSummaryResponseTool() (Tool, TerminalHandler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "emit_summary_response",
			Description: "提交本轮智能总结的结构化结果并结束回合。必须单独调用；reply 是对话气泡，预览正文只能放在 preview.content。",
			Parameters: map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"result_type": map[string]interface{}{
						"type": "string",
						"enum": []string{
							SummaryResultClarification,
							SummaryResultWorkflowConfirmation,
							SummaryResultWorkflowStarted,
							SummaryResultWorkflowCompleted,
							SummaryResultAgentPreview,
							SummaryResultAgentRevision,
							SummaryResultExplanation,
							SummaryResultError,
						},
					},
					"reply": map[string]interface{}{
						"type":        "string",
						"description": "展示在对话气泡中的简短回复；不要把总结正文放在这里。",
					},
					"execution_target": map[string]interface{}{
						"type": "string",
						"enum": []string{"personal_workflow", "team_workflow", "agent_preview"},
					},
					"workflow": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]interface{}{
							"task_id": map[string]interface{}{"type": "integer"},
							"status":  map[string]interface{}{"type": "string"},
							"saved":   map[string]interface{}{"type": "boolean"},
						},
						"required": []string{"task_id", "status", "saved"},
					},
					"preview": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]interface{}{
							"content":           map[string]interface{}{"type": "string"},
							"version":           map[string]interface{}{"type": "integer"},
							"parent_message_id": map[string]interface{}{"type": "integer"},
							"assumptions": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "string"},
							},
						},
						"required": []string{"content", "version"},
					},
					"confirmation": map[string]interface{}{
						"type":        "object",
						"description": "服务端可验证的多人协作提案；不得表示任务已经创建。",
					},
					"missing_fields": map[string]interface{}{
						"type":  "array",
						"items": map[string]interface{}{"type": "string"},
					},
				},
				"required": []string{"result_type", "reply"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (TerminalOutcome, error) {
		payload, canonical, err := parseSummaryResponsePayload(args)
		if err != nil {
			return TerminalOutcome{}, err
		}
		if allowed, ok := ctx.Value(allowedSummaryResultTypesContextKey{}).(map[string]struct{}); ok {
			if _, accepted := allowed[payload.ResultType]; !accepted {
				return TerminalOutcome{}, fmt.Errorf("result_type %q is not allowed for this request", payload.ResultType)
			}
		}
		return TerminalOutcome{
			VisibleContent: payload.Reply,
			ResultType:     payload.ResultType,
			Payload:        canonical,
		}, nil
	}

	return schema, handler
}

func parseSummaryResponsePayload(args json.RawMessage) (SummaryResponsePayload, json.RawMessage, error) {
	var payload SummaryResponsePayload
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return SummaryResponsePayload{}, nil, fmt.Errorf("parse emit_summary_response args: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return SummaryResponsePayload{}, nil, fmt.Errorf("parse emit_summary_response args: %w", err)
	}

	payload.ResultType = strings.TrimSpace(payload.ResultType)
	payload.Reply = strings.TrimSpace(payload.Reply)
	payload.ExecutionTarget = strings.TrimSpace(payload.ExecutionTarget)
	if _, ok := validSummaryResultTypes[payload.ResultType]; !ok {
		return SummaryResponsePayload{}, nil, fmt.Errorf("invalid result_type %q", payload.ResultType)
	}
	if payload.Reply == "" {
		return SummaryResponsePayload{}, nil, fmt.Errorf("reply is required")
	}
	if err := validateSummaryResponseShape(payload); err != nil {
		return SummaryResponsePayload{}, nil, err
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return SummaryResponsePayload{}, nil, fmt.Errorf("marshal emit_summary_response payload: %w", err)
	}
	return payload, json.RawMessage(canonical), nil
}

func validateSummaryResponseShape(payload SummaryResponsePayload) error {
	hasWorkflow := payload.Workflow != nil
	hasPreview := payload.Preview != nil
	hasConfirmation := payload.Confirmation != nil

	switch payload.ResultType {
	case SummaryResultAgentPreview, SummaryResultAgentRevision:
		if payload.ExecutionTarget != "agent_preview" {
			return fmt.Errorf("execution_target must be agent_preview for %s", payload.ResultType)
		}
		if !hasPreview || strings.TrimSpace(payload.Preview.Content) == "" || payload.Preview.Version <= 0 {
			return fmt.Errorf("preview content and positive version are required for %s", payload.ResultType)
		}
		if payload.ResultType == SummaryResultAgentPreview && payload.Preview.ParentMessageID != 0 {
			return fmt.Errorf("agent_preview cannot have parent_message_id")
		}
		if payload.ResultType == SummaryResultAgentRevision && payload.Preview.ParentMessageID <= 0 {
			return fmt.Errorf("agent_revision requires parent_message_id")
		}
		if hasWorkflow || hasConfirmation {
			return fmt.Errorf("%s cannot include workflow or confirmation", payload.ResultType)
		}
	case SummaryResultWorkflowConfirmation:
		if payload.ExecutionTarget != "team_workflow" {
			return fmt.Errorf("execution_target must be team_workflow for workflow_confirmation")
		}
		if !hasConfirmation || len(payload.Confirmation) == 0 {
			return fmt.Errorf("confirmation is required for workflow_confirmation")
		}
		if hasWorkflow || hasPreview {
			return fmt.Errorf("workflow_confirmation cannot include workflow or preview")
		}
	case SummaryResultWorkflowStarted, SummaryResultWorkflowCompleted:
		if payload.ExecutionTarget != "personal_workflow" && payload.ExecutionTarget != "team_workflow" {
			return fmt.Errorf("workflow result requires a workflow execution_target")
		}
		if !hasWorkflow || payload.Workflow.TaskID <= 0 || strings.TrimSpace(payload.Workflow.Status) == "" {
			return fmt.Errorf("workflow task_id and status are required for %s", payload.ResultType)
		}
		if payload.ResultType == SummaryResultWorkflowCompleted && !payload.Workflow.Saved {
			return fmt.Errorf("workflow_completed requires saved=true")
		}
		if hasPreview || hasConfirmation {
			return fmt.Errorf("%s cannot include preview or confirmation", payload.ResultType)
		}
	case SummaryResultClarification, SummaryResultExplanation, SummaryResultError:
		if hasWorkflow || hasPreview || hasConfirmation {
			return fmt.Errorf("%s cannot include workflow, preview, or confirmation", payload.ResultType)
		}
	}
	return nil
}
