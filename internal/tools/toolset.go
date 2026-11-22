package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"coder/internal/types"
)

// ToolsetOptions configures the native tool registry for a workspace session.
type ToolsetOptions struct {
	WorkspaceDir   string
	Approver       types.Approver
	Skills         types.SkillCatalog
	CustomTools    []types.CustomToolSpec
	MaxOutputBytes int
	DisableRg      bool
}

// Toolset holds the ordered set of built-in, skill, and custom tools for an agent session.
type Toolset struct {
	mu           sync.RWMutex
	workspaceDir string
	ordered      []types.Tool
	byName       map[string]types.Tool
}

// NewToolset constructs a complete Toolset containing all built-in native tools,
// load_skill (when opts.Skills is non-empty), and any configured CustomTools.
func NewToolset(opts ToolsetOptions) *Toolset {
	ws := normalizeWorkspaceDir(opts.WorkspaceDir)
	ts := &Toolset{
		workspaceDir: ws,
		byName:       make(map[string]types.Tool, 8+len(opts.CustomTools)),
	}

	ts.register(NewReadFileTool(ws, opts.Approver))
	ts.register(NewWriteFileTool(ws, opts.Approver))
	ts.register(NewEditFileTool(ws, opts.Approver))
	ts.register(NewListDirTool(ws, opts.Approver))
	ts.register(newGrepSearchToolWithOptions(ws, opts.Approver, opts.DisableRg))
	ts.register(newRunCommandToolWithCap(ws, opts.Approver, opts.MaxOutputBytes))

	if opts.Skills != nil && len(opts.Skills.List()) > 0 {
		ts.register(NewLoadSkillTool(opts.Skills))
	}

	for _, spec := range opts.CustomTools {
		if strings.TrimSpace(spec.Name) == "" {
			continue
		}
		ts.register(NewCustomTool(spec, ws, opts.Approver))
	}

	return ts
}

func (ts *Toolset) register(tool types.Tool) {
	if tool == nil {
		return
	}
	name := tool.Definition().Name
	if name == "" {
		return
	}
	if existingIdx, ok := ts.indexOf(name); ok {
		ts.ordered[existingIdx] = tool
		ts.byName[name] = tool
		return
	}
	ts.ordered = append(ts.ordered, tool)
	ts.byName[name] = tool
}

func (ts *Toolset) indexOf(name string) (int, bool) {
	for i, t := range ts.ordered {
		if t.Definition().Name == name {
			return i, true
		}
	}
	return -1, false
}

// Tools returns all registered tools in deterministic order.
func (ts *Toolset) Tools() []types.Tool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]types.Tool, len(ts.ordered))
	copy(out, ts.ordered)
	return out
}

// Definitions returns JSON Schema ToolDefinitions for all registered tools.
func (ts *Toolset) Definitions() []types.ToolDefinition {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	defs := make([]types.ToolDefinition, len(ts.ordered))
	for i, t := range ts.ordered {
		defs[i] = t.Definition()
	}
	return defs
}

// Get looks up a registered tool by exact name.
func (ts *Toolset) Get(name string) (types.Tool, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	t, ok := ts.byName[name]
	return t, ok
}

// Execute looks up a tool by name and runs it, returning an actionable ToolResult if unknown.
func (ts *Toolset) Execute(ctx context.Context, name string, args json.RawMessage) (types.ToolResult, error) {
	tool, ok := ts.Get(name)
	if !ok {
		return types.ToolResult{
			Content: fmt.Sprintf("unknown tool %q", name),
			IsError: true,
		}, nil
	}
	return tool.Execute(ctx, args)
}

// WorkspaceFiles returns up to limit slash-normalized relative workspace file paths
// for fast path-scoped instruction rule (applyTo) matching.
func (ts *Toolset) WorkspaceFiles(limit int) []string {
	return ListWorkspaceFiles(ts.workspaceDir, limit)
}
