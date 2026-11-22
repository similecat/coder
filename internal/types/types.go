package types

import (
	"context"
	"encoding/json"
	"time"
)

// Role identifies the author of a conversation message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ApprovalPolicy controls how tool side effects are approved.
type ApprovalPolicy string

const (
	ApprovalSmartAuto ApprovalPolicy = "smart_auto" // Auto-approve reads & workspace edits; confirm shell commands & external writes
	ApprovalFullAuto  ApprovalPolicy = "full_auto"  // Auto-approve all tools without prompting
	ApprovalStrictAsk ApprovalPolicy = "strict_ask" // Auto-approve read-only tools; confirm all edits, writes, and shell commands
)

// ToolCategory classifies a tool's side-effect profile for permission checks and parallel execution.
type ToolCategory string

const (
	ToolCategoryRead    ToolCategory = "read"    // Pure read/search/skill load; safe to parallelize and auto-approve
	ToolCategoryEdit    ToolCategory = "edit"    // Workspace file create/edit
	ToolCategoryExecute ToolCategory = "execute" // Shell/compiler command or custom executable script
)

// Message represents a single turn item in conversation history.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
}

// ToolCall represents a model's request to invoke a tool.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult represents the output returned from executing a tool.
type ToolResult struct {
	Content    string         `json:"content"`
	IsError    bool           `json:"is_error,omitempty"`
	DurationMs int64          `json:"duration_ms,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// ToolDefinition describes a tool's JSON Schema contract exposed to models.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Category    ToolCategory   `json:"category"`
}

// Tool is an executable capability exposed to the coding agent.
type Tool interface {
	Definition() ToolDefinition
	Execute(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// ApprovalRequest describes an action requiring policy evaluation.
type ApprovalRequest struct {
	ToolName    string
	Category    ToolCategory
	Summary     string
	TargetPath  string // Optional target file/directory path
	Command     string // Optional shell command
	WorkingDir  string
}

// Approver evaluates whether a tool invocation is permitted.
type Approver interface {
	Check(ctx context.Context, req ApprovalRequest) (approved bool, reason string, err error)
	Policy() ApprovalPolicy
	SetPolicy(policy ApprovalPolicy)
}

// InstructionSource identifies where an instruction or skill originated.
type InstructionSource string

const (
	SourceCoderUser    InstructionSource = "coder_user"
	SourceCoderRepo    InstructionSource = "coder_repo"
	SourceCopilotUser  InstructionSource = "copilot_user"
	SourceCopilotRepo  InstructionSource = "copilot_repo"
	SourceAgentsMD     InstructionSource = "agents_md"
)

// InstructionDoc represents a loaded instruction or path-scoped rule file.
type InstructionDoc struct {
	Path        string            `json:"path"`
	Source      InstructionSource `json:"source"`
	ApplyTo     string            `json:"apply_to,omitempty"` // Optional glob pattern(s) from frontmatter
	Description string            `json:"description,omitempty"`
	Content     string            `json:"content"`
}

// SkillInfo represents a discovered skill (metadata loaded eagerly; full body loaded on demand).
type SkillInfo struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	SkillFile   string            `json:"skill_file"`
	Dir         string            `json:"dir"`
	Source      InstructionSource `json:"source"`
}

// SkillCatalog provides compact metadata listing and on-demand full skill loading.
type SkillCatalog interface {
	List() []SkillInfo
	Load(name string) (content string, helperFiles []string, info SkillInfo, err error)
}

// CustomToolSpec defines a user- or repo-configured custom command/script tool in .coder/tools/*.json.
type CustomToolSpec struct {
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Parameters       map[string]any    `json:"parameters"`
	Command          string            `json:"command"`
	Args             []string          `json:"args,omitempty"`
	Shell            bool              `json:"shell,omitempty"`
	WorkingDir       string            `json:"working_dir,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	TimeoutSec       int               `json:"timeout_sec,omitempty"`
	ReadOnly         bool              `json:"read_only,omitempty"`
	RequiresApproval *bool             `json:"requires_approval,omitempty"`
	SourceFile       string            `json:"source_file,omitempty"`
}

// StreamEventType classifies real-time streaming events emitted during a turn.
type StreamEventType string

const (
	EventTextDelta      StreamEventType = "text_delta"
	EventReasoningDelta StreamEventType = "reasoning_delta"
	EventToolCallStart  StreamEventType = "tool_call_start"
	EventToolCallEnd    StreamEventType = "tool_call_end"
	EventUsage          StreamEventType = "usage"
	EventError          StreamEventType = "error"
)

// StreamEvent carries incremental streaming updates to the CLI or caller.
type StreamEvent struct {
	Type        StreamEventType `json:"type"`
	Text        string          `json:"text,omitempty"`
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	ToolName    string          `json:"tool_name,omitempty"`
	ToolArgs    string          `json:"tool_args,omitempty"`
	ToolResult  *ToolResult     `json:"tool_result,omitempty"`
	InputTokens int             `json:"input_tokens,omitempty"`
	OutputTokens int            `json:"output_tokens,omitempty"`
	CachedTokens int            `json:"cached_tokens,omitempty"`
	Err         error           `json:"-"`
}

// TurnMetrics records latency and usage telemetry for a completed user turn.
type TurnMetrics struct {
	Provider       string        `json:"provider"`
	Model          string        `json:"model"`
	TTFT           time.Duration `json:"ttft_ns"`
	TotalDuration  time.Duration `json:"total_duration_ns"`
	LLMCalls       int           `json:"llm_calls"`
	ToolCalls      int           `json:"tool_calls"`
	InputTokens    int           `json:"input_tokens"`
	OutputTokens   int           `json:"output_tokens"`
	CachedTokens   int           `json:"cached_tokens"`
}

// ModelInfo describes an available model exposed by a provider.
type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Provider    string `json:"provider"`
	ContextSize int    `json:"context_size,omitempty"`
}

// ProviderType identifies the wire/SDK backend implementation for a provider profile.
type ProviderType string

const (
	ProviderTypeCopilotSDK   ProviderType = "copilot_sdk"
	ProviderTypeOpenAICompat ProviderType = "openai_compat"
	ProviderTypeAnthropic    ProviderType = "anthropic"
)

// ProviderProfile holds configuration for a named model provider backend.
type ProviderProfile struct {
	Name         string            `json:"name,omitempty"`
	Type         ProviderType      `json:"type"`
	BaseURL      string            `json:"base_url,omitempty"`
	APIKey       string            `json:"api_key,omitempty"`
	APIKeyEnv    string            `json:"api_key_env,omitempty"`
	DefaultModel string            `json:"default_model,omitempty"`
	APIVersion   string            `json:"api_version,omitempty"` // Optional, e.g. Azure OpenAI api-version
	Headers      map[string]string `json:"headers,omitempty"`
	TimeoutSec   int               `json:"timeout_sec,omitempty"`
	WarmSession  *bool             `json:"warm_session,omitempty"`
}

// Provider is the base contract implemented by all model backends.
type Provider interface {
	Name() string
	DefaultModel() string
	ListModels(ctx context.Context) ([]ModelInfo, error)
	Close() error
}

// ChatRequest is the input for stateless streaming LLM chat providers (OpenAI-compatible, Anthropic, local Ollama/vLLM/llama.cpp).
type ChatRequest struct {
	Model           string           `json:"model"`
	SystemPrompt    string           `json:"system_prompt"`
	Messages        []Message        `json:"messages"`
	Tools           []ToolDefinition `json:"tools,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	MaxTokens       int              `json:"max_tokens,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
}

// ChatResponse is the completed assistant response from a single LLM round-trip.
type ChatResponse struct {
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	InputTokens  int        `json:"input_tokens,omitempty"`
	OutputTokens int        `json:"output_tokens,omitempty"`
	CachedTokens int        `json:"cached_tokens,omitempty"`
	StopReason   string     `json:"stop_reason,omitempty"`
}

// ChatProvider is implemented by direct HTTP SSE providers whose tool loop is orchestrated by internal/agent.
type ChatProvider interface {
	Provider
	StreamChat(ctx context.Context, req ChatRequest, onEvent func(StreamEvent)) (*ChatResponse, error)
}

// ManagedSessionConfig configures a warm stateful SDK session (such as GitHub Copilot SDK).
type ManagedSessionConfig struct {
	Model           string
	SystemPrompt    string
	WorkingDir      string
	Tools           []Tool
	ReasoningEffort string
	Streaming       bool
}

// ManagedSession represents a stateful session that executes registered Tools internally via callbacks.
type ManagedSession interface {
	RunTurn(ctx context.Context, prompt string, onEvent func(StreamEvent)) (finalText string, metrics TurnMetrics, err error)
	SetModel(ctx context.Context, model string) error
	SetTools(ctx context.Context, tools []Tool) error
	Close() error
}

// ManagedSessionProvider is implemented by SDK-backed providers (e.g. GitHub Copilot SDK) that maintain warm sessions.
type ManagedSessionProvider interface {
	Provider
	CreateSession(ctx context.Context, cfg ManagedSessionConfig) (ManagedSession, error)
}
