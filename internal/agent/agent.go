package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"coder/internal/types"
)

// ErrMaxStepsExceeded is returned when a turn exceeds MaxSteps tool round-trips.
var ErrMaxStepsExceeded = errors.New("agent exceeded maximum tool-call steps for a single turn")

// Options configures an Agent instance.
type Options struct {
	Provider           types.Provider
	Model              string
	SystemPrompt       string
	PromptBuilder      func() string // Optional dynamic builder called before turns/session creation
	Tools              []types.Tool
	WorkingDir         string
	MaxSteps           int
	MaxToolOutputBytes int
	MaxHistoryChars    int
	ReasoningEffort    string
	Streaming          bool
}

// Agent coordinates conversation state, streaming LLM calls, and parallel native tool execution.
type Agent struct {
	mu                 sync.Mutex
	provider           types.Provider
	model              string
	systemPrompt       string
	promptBuilder      func() string
	tools              []types.Tool
	toolMap            map[string]types.Tool
	workingDir         string
	maxSteps           int
	maxToolOutputBytes int
	maxHistoryChars    int
	reasoningEffort    string
	streaming          bool

	// Stateful history for ChatProvider backends (OpenAI-compat, Ollama, Anthropic, etc.)
	history []types.Message

	// Warm session for ManagedSessionProvider backends (GitHub Copilot SDK)
	managedSession types.ManagedSession
}

// New creates a new low-latency coding Agent.
func New(opts Options) (*Agent, error) {
	if opts.Provider == nil {
		return nil, errors.New("agent requires a non-nil Provider")
	}
	maxSteps := opts.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 40
	}
	maxOut := opts.MaxToolOutputBytes
	if maxOut <= 0 {
		maxOut = 32768
	}
	maxHist := opts.MaxHistoryChars
	if maxHist <= 0 {
		maxHist = 240000
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = opts.Provider.DefaultModel()
	}

	a := &Agent{
		provider:           opts.Provider,
		model:              model,
		systemPrompt:       opts.SystemPrompt,
		promptBuilder:      opts.PromptBuilder,
		workingDir:         opts.WorkingDir,
		maxSteps:           maxSteps,
		maxToolOutputBytes: maxOut,
		maxHistoryChars:    maxHist,
		reasoningEffort:    opts.ReasoningEffort,
		streaming:          opts.Streaming,
	}
	a.setToolsLocked(opts.Tools)
	return a, nil
}

func (a *Agent) setToolsLocked(tools []types.Tool) {
	a.tools = make([]types.Tool, len(tools))
	copy(a.tools, tools)
	a.toolMap = make(map[string]types.Tool, len(tools))
	for _, t := range tools {
		a.toolMap[t.Definition().Name] = t
	}
}

func (a *Agent) currentSystemPromptLocked() string {
	if a.promptBuilder != nil {
		if built := strings.TrimSpace(a.promptBuilder()); built != "" {
			a.systemPrompt = built
			return built
		}
	}
	return a.systemPrompt
}

// Model returns the currently active model ID.
func (a *Agent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// ProviderName returns the active provider's name.
func (a *Agent) ProviderName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider.Name()
}

// SetModel updates the active model, updating any warm managed session in place.
func (a *Agent) SetModel(ctx context.Context, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model name cannot be empty")
	}
	a.mu.Lock()
	a.model = model
	ms := a.managedSession
	a.mu.Unlock()

	if ms != nil {
		if err := ms.SetModel(ctx, model); err != nil {
			// If warm session rejects dynamic model switch, recycle session on next turn.
			a.mu.Lock()
			if a.managedSession == ms {
				_ = a.managedSession.Close()
				a.managedSession = nil
			}
			a.mu.Unlock()
		}
	}
	return nil
}

// SetProvider switches the active backend provider cleanly.
func (a *Agent) SetProvider(prov types.Provider, model string) error {
	if prov == nil {
		return errors.New("provider cannot be nil")
	}
	a.mu.Lock()
	oldMS := a.managedSession
	a.managedSession = nil
	a.provider = prov
	if strings.TrimSpace(model) != "" {
		a.model = strings.TrimSpace(model)
	} else {
		a.model = prov.DefaultModel()
	}
	a.mu.Unlock()

	if oldMS != nil {
		_ = oldMS.Close()
	}
	return nil
}

// SetTools updates the registered toolset.
func (a *Agent) SetTools(ctx context.Context, tools []types.Tool) error {
	a.mu.Lock()
	a.setToolsLocked(tools)
	ms := a.managedSession
	a.mu.Unlock()

	if ms != nil {
		return ms.SetTools(ctx, tools)
	}
	return nil
}

// Clear resets conversation history and recycles any stateful session while keeping the underlying client warm.
func (a *Agent) Clear() {
	a.mu.Lock()
	a.history = nil
	oldMS := a.managedSession
	a.managedSession = nil
	a.mu.Unlock()

	if oldMS != nil {
		_ = oldMS.Close()
	}
}

// History returns a shallow copy of the current message history.
func (a *Agent) History() []types.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]types.Message, len(a.history))
	copy(out, a.history)
	return out
}

// Compact trims bulky historical tool outputs and drops oldest user-turn segments
// cleanly on RoleUser boundaries so tool_call / tool_result pairs are never orphaned.
func (a *Agent) Compact(keepRecentTurns int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.compactLocked(keepRecentTurns)
}

func (a *Agent) compactLocked(keepRecentTurns int) int {
	if len(a.history) == 0 {
		return 0
	}
	if keepRecentTurns <= 0 {
		keepRecentTurns = 4
	}

	// Find indices of RoleUser messages.
	var userIdx []int
	for i, m := range a.history {
		if m.Role == types.RoleUser {
			userIdx = append(userIdx, i)
		}
	}

	beforeChars := estimateHistoryChars(a.history)

	// First pass: shrink large older tool outputs prior to the last keepRecentTurns user turns.
	cutoffMsgIdx := len(a.history)
	if len(userIdx) > keepRecentTurns {
		cutoffMsgIdx = userIdx[len(userIdx)-keepRecentTurns]
	} else if len(userIdx) > 1 {
		cutoffMsgIdx = userIdx[len(userIdx)-1]
	}

	for i := 0; i < cutoffMsgIdx; i++ {
		if a.history[i].Role == types.RoleTool && len(a.history[i].Content) > 600 {
			a.history[i].Content = truncateHeadTail(a.history[i].Content, 600)
		}
	}

	// Second pass: if still exceeding maxHistoryChars and we have multiple user turns, drop oldest full user turns.
	if len(userIdx) > keepRecentTurns && estimateHistoryChars(a.history) > a.maxHistoryChars {
		dropUntil := userIdx[len(userIdx)-keepRecentTurns]
		a.history = append([]types.Message(nil), a.history[dropUntil:]...)
	}

	afterChars := estimateHistoryChars(a.history)
	if beforeChars > afterChars {
		return beforeChars - afterChars
	}
	return 0
}

// Close releases any warm session and closes the underlying provider.
func (a *Agent) Close() error {
	a.mu.Lock()
	ms := a.managedSession
	a.managedSession = nil
	prov := a.provider
	a.mu.Unlock()

	var firstErr error
	if ms != nil {
		if err := ms.Close(); err != nil {
			firstErr = err
		}
	}
	if prov != nil {
		if err := prov.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RunTurn executes a complete user turn, handling all intermediate tool calls and streaming events.
func (a *Agent) RunTurn(ctx context.Context, userPrompt string, onEvent func(types.StreamEvent)) (string, types.TurnMetrics, error) {
	if onEvent == nil {
		onEvent = func(types.StreamEvent) {}
	}

	a.mu.Lock()
	prov := a.provider
	a.mu.Unlock()

	if msp, ok := prov.(types.ManagedSessionProvider); ok {
		return a.runManagedTurn(ctx, msp, userPrompt, onEvent)
	}
	if cp, ok := prov.(types.ChatProvider); ok {
		return a.runChatTurn(ctx, cp, userPrompt, onEvent)
	}
	return "", types.TurnMetrics{}, fmt.Errorf("provider %q implements neither ManagedSessionProvider nor ChatProvider", prov.Name())
}

func (a *Agent) runManagedTurn(
	ctx context.Context,
	msp types.ManagedSessionProvider,
	userPrompt string,
	onEvent func(types.StreamEvent),
) (string, types.TurnMetrics, error) {
	a.mu.Lock()
	ms := a.managedSession
	model := a.model
	sysPrompt := a.currentSystemPromptLocked()
	tools := make([]types.Tool, len(a.tools))
	copy(tools, a.tools)
	workDir := a.workingDir
	effort := a.reasoningEffort
	streaming := a.streaming
	a.mu.Unlock()

	if ms == nil {
		created, err := msp.CreateSession(ctx, types.ManagedSessionConfig{
			Model:           model,
			SystemPrompt:    sysPrompt,
			WorkingDir:      workDir,
			Tools:           tools,
			ReasoningEffort: effort,
			Streaming:       streaming,
		})
		if err != nil {
			return "", types.TurnMetrics{Provider: msp.Name(), Model: model}, err
		}
		a.mu.Lock()
		a.managedSession = created
		if am, ok := created.(interface{ ActiveModel() string }); ok {
			if resolved := strings.TrimSpace(am.ActiveModel()); resolved != "" {
				a.model = resolved
				model = resolved
			}
		}
		ms = created
		a.mu.Unlock()
	}

	text, metrics, err := ms.RunTurn(ctx, userPrompt, onEvent)
	if metrics.Provider == "" {
		metrics.Provider = msp.Name()
	}
	if metrics.Model != "" {
		a.mu.Lock()
		a.model = metrics.Model
		a.mu.Unlock()
	} else {
		metrics.Model = model
	}
	return text, metrics, err
}

func (a *Agent) runChatTurn(
	ctx context.Context,
	cp types.ChatProvider,
	userPrompt string,
	onEvent func(types.StreamEvent),
) (string, types.TurnMetrics, error) {
	turnStart := time.Now()
	var ttft time.Duration
	var gotFirstOutput bool
	var eventMu sync.Mutex

	safeEmit := func(ev types.StreamEvent) {
		eventMu.Lock()
		if !gotFirstOutput && (ev.Type == types.EventTextDelta || ev.Type == types.EventReasoningDelta || ev.Type == types.EventToolCallStart) {
			ttft = time.Since(turnStart)
			gotFirstOutput = true
		}
		onEvent(ev)
		eventMu.Unlock()
	}

	a.mu.Lock()
	model := a.model
	sysPrompt := a.currentSystemPromptLocked()
	effort := a.reasoningEffort
	maxSteps := a.maxSteps
	if estimateHistoryChars(a.history) > a.maxHistoryChars {
		a.compactLocked(4)
	}
	a.history = append(a.history, types.Message{
		Role:    types.RoleUser,
		Content: userPrompt,
	})
	a.mu.Unlock()

	metrics := types.TurnMetrics{
		Provider: cp.Name(),
		Model:    model,
	}

	var lastAssistantText string

	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			metrics.TTFT = ttft
			metrics.TotalDuration = time.Since(turnStart)
			return lastAssistantText, metrics, err
		}

		a.mu.Lock()
		msgsSnapshot := make([]types.Message, len(a.history))
		copy(msgsSnapshot, a.history)
		defs := make([]types.ToolDefinition, 0, len(a.tools))
		for _, t := range a.tools {
			defs = append(defs, t.Definition())
		}
		sysPrompt = a.currentSystemPromptLocked()
		model = a.model
		a.mu.Unlock()

		req := types.ChatRequest{
			Model:           model,
			SystemPrompt:    sysPrompt,
			Messages:        msgsSnapshot,
			Tools:           defs,
			ReasoningEffort: effort,
		}

		resp, err := cp.StreamChat(ctx, req, safeEmit)
		metrics.LLMCalls++
		if err != nil {
			metrics.TTFT = ttft
			metrics.TotalDuration = time.Since(turnStart)
			return lastAssistantText, metrics, err
		}

		if !gotFirstOutput {
			eventMu.Lock()
			if !gotFirstOutput {
				ttft = time.Since(turnStart)
				gotFirstOutput = true
			}
			eventMu.Unlock()
		}

		metrics.InputTokens += resp.InputTokens
		metrics.OutputTokens += resp.OutputTokens
		metrics.CachedTokens += resp.CachedTokens
		if resp.Content != "" {
			lastAssistantText = resp.Content
		}

		a.mu.Lock()
		a.history = append(a.history, types.Message{
			Role:      types.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		a.mu.Unlock()

		if len(resp.ToolCalls) == 0 {
			metrics.TTFT = ttft
			metrics.TotalDuration = time.Since(turnStart)
			return resp.Content, metrics, nil
		}

		// Execute requested tool calls: consecutive read-only calls run concurrently in goroutines!
		toolMsgs := a.executeToolBatch(ctx, resp.ToolCalls, safeEmit)
		metrics.ToolCalls += len(resp.ToolCalls)

		a.mu.Lock()
		a.history = append(a.history, toolMsgs...)
		a.mu.Unlock()
	}

	metrics.TTFT = ttft
	metrics.TotalDuration = time.Since(turnStart)
	return lastAssistantText, metrics, ErrMaxStepsExceeded
}

func (a *Agent) executeToolBatch(
	ctx context.Context,
	calls []types.ToolCall,
	emit func(types.StreamEvent),
) []types.Message {
	results := make([]types.Message, len(calls))

	i := 0
	for i < len(calls) {
		if a.isReadOnlyCall(calls[i].Name) {
			// Group contiguous read-only calls so they execute concurrently while preserving exact ordering.
			end := i + 1
			for end < len(calls) && a.isReadOnlyCall(calls[end].Name) {
				end++
			}
			if end-i == 1 {
				results[i] = a.executeSingleTool(ctx, calls[i], emit)
			} else {
				var wg sync.WaitGroup
				for idx := i; idx < end; idx++ {
					wg.Add(1)
					go func(pos int, tc types.ToolCall) {
						defer wg.Done()
						results[pos] = a.executeSingleTool(ctx, tc, emit)
					}(idx, calls[idx])
				}
				wg.Wait()
			}
			i = end
		} else {
			results[i] = a.executeSingleTool(ctx, calls[i], emit)
			i++
		}
	}

	return results
}

func (a *Agent) isReadOnlyCall(toolName string) bool {
	a.mu.Lock()
	t, ok := a.toolMap[toolName]
	a.mu.Unlock()
	if !ok {
		return false
	}
	return t.Definition().Category == types.ToolCategoryRead
}

func (a *Agent) executeSingleTool(
	ctx context.Context,
	tc types.ToolCall,
	emit func(types.StreamEvent),
) types.Message {
	emit(types.StreamEvent{
		Type:       types.EventToolCallStart,
		ToolCallID: tc.ID,
		ToolName:   tc.Name,
		ToolArgs:   string(tc.Arguments),
	})

	a.mu.Lock()
	tool, ok := a.toolMap[tc.Name]
	maxBytes := a.maxToolOutputBytes
	a.mu.Unlock()

	start := time.Now()
	var res types.ToolResult
	if !ok {
		res = types.ToolResult{
			Content: fmt.Sprintf("unknown tool %q", tc.Name),
			IsError: true,
		}
	} else {
		args := tc.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		execRes, err := tool.Execute(ctx, args)
		res = execRes
		if err != nil {
			res.IsError = true
			if res.Content == "" {
				res.Content = err.Error()
			} else {
				res.Content = res.Content + "\nerror: " + err.Error()
			}
		}
	}
	if res.DurationMs == 0 {
		res.DurationMs = time.Since(start).Milliseconds()
	}
	if maxBytes > 0 && len(res.Content) > maxBytes {
		res.Content = truncateHeadTail(res.Content, maxBytes)
	}

	resCopy := res
	emit(types.StreamEvent{
		Type:       types.EventToolCallEnd,
		ToolCallID: tc.ID,
		ToolName:   tc.Name,
		ToolResult: &resCopy,
	})

	return types.Message{
		Role:       types.RoleTool,
		ToolCallID: tc.ID,
		ToolName:   tc.Name,
		Content:    res.Content,
		IsError:    res.IsError,
	}
}

func estimateHistoryChars(msgs []types.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) + 32
		for _, tc := range m.ToolCalls {
			total += len(tc.Name) + len(tc.Arguments) + 16
		}
	}
	return total
}

func truncateHeadTail(s string, maxBytes int) string {
	if len(s) <= maxBytes || maxBytes < 64 {
		return s
	}
	half := (maxBytes - 64) / 2
	omitted := len(s) - 2*half
	return fmt.Sprintf("%s\n...[truncated %d bytes]...\n%s", s[:half], omitted, s[len(s)-half:])
}
