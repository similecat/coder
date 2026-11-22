package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"coder/internal/config"
	"coder/internal/types"
)

func TestLoadAllFiveProblems(t *testing.T) {
	explicitRoot := filepath.Join("..", "..", "testdata", "problems")
	problems, err := LoadProblems(explicitRoot, nil)
	if err != nil {
		t.Fatalf("LoadProblems(%q) failed: %v", explicitRoot, err)
	}
	if len(problems) != 5 {
		t.Fatalf("expected 5 problems, got %d", len(problems))
	}

	// Also verify auto-discovery with empty problemsRoot finds testdata\problems.
	autoProblems, err := LoadProblems("", nil)
	if err != nil {
		t.Fatalf("LoadProblems(\"\") auto-discovery failed: %v", err)
	}
	if len(autoProblems) != 5 {
		t.Fatalf("expected 5 auto-discovered problems, got %d", len(autoProblems))
	}

	byID := make(map[string]Problem, len(problems))
	for _, p := range problems {
		byID[p.ID] = p
		if p.Title == "" {
			t.Errorf("problem %s has empty Title", p.ID)
		}
		if p.Prompt == "" {
			t.Errorf("problem %s has empty Prompt", p.ID)
		}
		if p.TimeoutSec <= 0 {
			t.Errorf("problem %s has invalid TimeoutSec=%d", p.ID, p.TimeoutSec)
		}
		if fi, statErr := os.Stat(p.WorkspaceDir); statErr != nil || !fi.IsDir() {
			t.Errorf("problem %s missing WorkspaceDir %s: %v", p.ID, p.WorkspaceDir, statErr)
		}
		verifyPath := filepath.Join(p.Dir, filepath.FromSlash(p.VerifyEntry))
		if fi, statErr := os.Stat(verifyPath); statErr != nil || fi.IsDir() {
			t.Errorf("problem %s missing VerifyEntry %s: %v", p.ID, verifyPath, statErr)
		}
	}

	expected := []struct {
		id         string
		lang       string
		compatReq  bool
		cppSources int
	}{
		{id: "cpp_graph_bugfix", lang: "cpp", compatReq: false, cppSources: 1},
		{id: "cpp_lru_ttl", lang: "cpp", compatReq: false, cppSources: 0},
		{id: "py_copilot_skill_compat", lang: "python", compatReq: true, cppSources: 0},
		{id: "py_dag_scheduler_bugfix", lang: "python", compatReq: false, cppSources: 0},
		{id: "py_expr_engine", lang: "python", compatReq: false, cppSources: 0},
	}

	for _, exp := range expected {
		got, ok := byID[exp.id]
		if !ok {
			t.Fatalf("missing expected problem %q", exp.id)
		}
		if got.Language != exp.lang {
			t.Errorf("%s: language=%q, want %q", exp.id, got.Language, exp.lang)
		}
		if got.CopilotCompatRequired != exp.compatReq {
			t.Errorf("%s: copilot_compat_required=%v, want %v", exp.id, got.CopilotCompatRequired, exp.compatReq)
		}
		if len(got.CPPSources) != exp.cppSources {
			t.Errorf("%s: len(cpp_sources)=%d, want %d", exp.id, len(got.CPPSources), exp.cppSources)
		}
	}

	// Verify comma-separated ID filtering and order preservation.
	subset, err := LoadProblems(explicitRoot, []string{"py_expr_engine,cpp_lru_ttl"})
	if err != nil {
		t.Fatalf("LoadProblems subset failed: %v", err)
	}
	if len(subset) != 2 || subset[0].ID != "py_expr_engine" || subset[1].ID != "cpp_lru_ttl" {
		t.Fatalf("unexpected subset order/count: %+v", subset)
	}

	// Verify language filtering.
	cppOnly, err := FilterProblemsByLanguage(problems, "cpp")
	if err != nil || len(cppOnly) != 2 {
		t.Fatalf("FilterProblemsByLanguage(cpp) got %d problems, err=%v", len(cppOnly), err)
	}
	pyOnly, err := FilterProblemsByLanguage(problems, "python")
	if err != nil || len(pyOnly) != 3 {
		t.Fatalf("FilterProblemsByLanguage(python) got %d problems, err=%v", len(pyOnly), err)
	}

	// Verify unknown problem ID produces an error.
	if _, err := LoadProblems(explicitRoot, []string{"does_not_exist"}); err == nil {
		t.Fatalf("expected error for unknown problem ID")
	}
}

func TestStarterWorkspacesFailVerification(t *testing.T) {
	problems, err := LoadProblems(filepath.Join("..", "..", "testdata", "problems"), nil)
	if err != nil {
		t.Fatalf("LoadProblems failed: %v", err)
	}
	if len(problems) != 5 {
		t.Fatalf("expected 5 problems, got %d", len(problems))
	}

	ctx := context.Background()
	for _, prob := range problems {
		prob := prob
		t.Run(prob.ID, func(t *testing.T) {
			tempWS := filepath.Join(t.TempDir(), "workspace")
			if err := CopyWorkspace(prob.WorkspaceDir, tempWS); err != nil {
				t.Fatalf("CopyWorkspace(%s) failed: %v", prob.ID, err)
			}

			if prob.ID == "py_copilot_skill_compat" {
				// Confirm hidden .github files were faithfully copied into temp workspace.
				instFile := filepath.Join(tempWS, ".github", "copilot-instructions.md")
				skillFile := filepath.Join(tempWS, ".github", "skills", "telemetry-codec", "SKILL.md")
				if _, err := os.Stat(instFile); err != nil {
					t.Fatalf("expected %s to be copied: %v", instFile, err)
				}
				if _, err := os.Stat(skillFile); err != nil {
					t.Fatalf("expected %s to be copied: %v", skillFile, err)
				}
			}

			res := VerifyWorkspace(ctx, prob, tempWS)
			if res.Passed {
				t.Fatalf("expected unmodified starter workspace for %s to FAIL verification, but it PASSED!\nOutput:\n%s", prob.ID, res.Output)
			}
			if res.ExitCode == 0 {
				t.Errorf("%s: expected non-zero exit code on starter failure, got 0 (err=%s)", prob.ID, res.Error)
			}
			if res.Compiler == "" {
				t.Errorf("%s: expected compiler/interpreter label to be populated (err=%s)", prob.ID, res.Error)
			}
			if res.Output == "" {
				t.Errorf("%s: expected diagnostic failure output from verifier, got empty output (err=%s)", prob.ID, res.Error)
			}
		})
	}
}

type mockBenchSolverProvider struct {
	mu                     sync.Mutex
	cppStep                int
	pyStep                 int
	sawCopilotInstructions bool
	sawLazySkillCatalog    bool
	sawLoadedSkillSpec     bool
	assertionErrors        []string
}

func (m *mockBenchSolverProvider) Name() string         { return "mock-bench" }
func (m *mockBenchSolverProvider) DefaultModel() string { return "mock-solver-v1" }
func (m *mockBenchSolverProvider) ListModels(context.Context) ([]types.ModelInfo, error) {
	return []types.ModelInfo{{ID: "mock-solver-v1", Provider: "mock-bench"}}, nil
}
func (m *mockBenchSolverProvider) Close() error { return nil }

func (m *mockBenchSolverProvider) recordFailureLocked(msg string) {
	m.assertionErrors = append(m.assertionErrors, msg)
}

func (m *mockBenchSolverProvider) StreamChat(
	_ context.Context,
	req types.ChatRequest,
	onEvent func(types.StreamEvent),
) (*types.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(req.Messages) == 0 {
		m.recordFailureLocked("received empty messages slice")
		return &types.ChatResponse{Content: "error"}, nil
	}

	promptText := req.Messages[0].Content
	switch {
	case strings.Contains(promptText, "TTLLRUCache"):
		return m.handleCPPLRUTTL(req, onEvent)
	case strings.Contains(promptText, "encode_packet"):
		return m.handlePyCopilotSkillCompat(req, onEvent)
	default:
		m.recordFailureLocked("unexpected problem prompt: " + promptText)
		return &types.ChatResponse{Content: "unknown problem"}, nil
	}
}

func (m *mockBenchSolverProvider) handleCPPLRUTTL(
	req types.ChatRequest,
	onEvent func(types.StreamEvent),
) (*types.ChatResponse, error) {
	m.cppStep++
	switch m.cppStep {
	case 1:
		if !hasTool(req.Tools, "read_file") || !hasTool(req.Tools, "write_file") {
			m.recordFailureLocked("cpp_lru_ttl missing read_file or write_file tool definitions")
		}
		return &types.ChatResponse{
			ToolCalls: []types.ToolCall{
				{
					ID:        "cpp-call-1",
					Name:      "read_file",
					Arguments: mustJSON(map[string]any{"path": "lru_cache.hpp"}),
				},
			},
			InputTokens:  120,
			OutputTokens: 25,
		}, nil

	case 2:
		readMsg, ok := findToolResult(req.Messages, "cpp-call-1")
		if !ok || readMsg.IsError || !strings.Contains(readMsg.Content, "class TTLLRUCache") {
			m.recordFailureLocked("cpp_lru_ttl step 2 did not receive starter lru_cache.hpp content")
		}
		return &types.ChatResponse{
			ToolCalls: []types.ToolCall{
				{
					ID:   "cpp-call-2",
					Name: "write_file",
					Arguments: mustJSON(map[string]any{
						"path":    "lru_cache.hpp",
						"content": referenceCPPLRUTTLHeader,
					}),
				},
			},
			InputTokens:  300,
			OutputTokens: 450,
		}, nil

	default:
		writeMsg, ok := findToolResult(req.Messages, "cpp-call-2")
		if !ok || writeMsg.IsError {
			m.recordFailureLocked("cpp_lru_ttl step 3 write_file returned an error")
		}
		done := "Implemented O(1) C++20 TTLLRUCache in lru_cache.hpp."
		onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: done})
		return &types.ChatResponse{
			Content:      done,
			InputTokens:  350,
			OutputTokens: 20,
		}, nil
	}
}

func (m *mockBenchSolverProvider) handlePyCopilotSkillCompat(
	req types.ChatRequest,
	onEvent func(types.StreamEvent),
) (*types.ChatResponse, error) {
	m.pyStep++
	switch m.pyStep {
	case 1:
		// Verify .github/copilot-instructions.md was injected into SystemPrompt.
		if strings.Contains(req.SystemPrompt, `CODEC_SCHEMA_VERSION = "telemetry.v2"`) &&
			strings.Contains(req.SystemPrompt, "FRAME_SYNC_WORD = 0xCAFE") {
			m.sawCopilotInstructions = true
		} else {
			m.recordFailureLocked("SystemPrompt missing .github/copilot-instructions.md constants")
		}

		// Verify 2-stage lazy skill catalog: compact metadata present, full CRC wire spec NOT eagerly dumped.
		if strings.Contains(req.SystemPrompt, "<available_skills>") &&
			strings.Contains(req.SystemPrompt, "telemetry-codec:") &&
			!strings.Contains(req.SystemPrompt, "0x1021") {
			m.sawLazySkillCatalog = true
		} else {
			m.recordFailureLocked("SystemPrompt did not exhibit expected 2-stage lazy skill catalog")
		}

		if !hasTool(req.Tools, "load_skill") {
			m.recordFailureLocked("load_skill tool missing from tool definitions")
		}

		// Issue parallel read-only tool calls: load_skill + read_file.
		return &types.ChatResponse{
			ToolCalls: []types.ToolCall{
				{
					ID:        "py-call-1",
					Name:      "load_skill",
					Arguments: mustJSON(map[string]any{"skill_name": "telemetry-codec"}),
				},
				{
					ID:        "py-call-2",
					Name:      "read_file",
					Arguments: mustJSON(map[string]any{"path": "codec.py"}),
				},
			},
			InputTokens:  180,
			OutputTokens: 35,
		}, nil

	case 2:
		skillMsg, okSkill := findToolResult(req.Messages, "py-call-1")
		if okSkill && !skillMsg.IsError &&
			strings.Contains(skillMsg.Content, ">HBBBBHH") &&
			strings.Contains(skillMsg.Content, "0x1021") {
			m.sawLoadedSkillSpec = true
		} else {
			m.recordFailureLocked("load_skill(telemetry-codec) did not return full wire format and CRC-16 spec")
		}

		readMsg, okRead := findToolResult(req.Messages, "py-call-2")
		if !okRead || readMsg.IsError || !strings.Contains(readMsg.Content, "def encode_packet") {
			m.recordFailureLocked("read_file(codec.py) did not return starter codec.py")
		}

		return &types.ChatResponse{
			ToolCalls: []types.ToolCall{
				{
					ID:   "py-call-3",
					Name: "write_file",
					Arguments: mustJSON(map[string]any{
						"path":    "codec.py",
						"content": referencePyTelemetryCodec,
					}),
				},
			},
			InputTokens:  520,
			OutputTokens: 480,
		}, nil

	default:
		writeMsg, ok := findToolResult(req.Messages, "py-call-3")
		if !ok || writeMsg.IsError {
			m.recordFailureLocked("py_copilot_skill_compat write_file(codec.py) failed")
		}
		done := "Implemented encode_packet and decode_packet per telemetry-codec skill and repository instructions."
		onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: done})
		return &types.ChatResponse{
			Content:      done,
			InputTokens:  560,
			OutputTokens: 24,
		}, nil
	}
}

func TestEndToEndMockAgentSolvesCppAndPythonAndSkillCompat(t *testing.T) {
	mockProv := &mockBenchSolverProvider{}
	var verboseBuf bytes.Buffer

	report, err := RunSuite(context.Background(), SuiteOptions{
		ProblemsRoot:   filepath.Join("..", "..", "testdata", "problems"),
		ProblemIDs:     []string{"cpp_lru_ttl", "py_copilot_skill_compat"},
		UserHome:       t.TempDir(),
		CustomProvider: mockProv,
		Verbose:        true,
		Stderr:         &verboseBuf,
	})
	if err != nil {
		t.Fatalf("RunSuite failed: %v", err)
	}

	if len(mockProv.assertionErrors) > 0 {
		t.Fatalf("mock agent pipeline assertions failed:\n- %s", strings.Join(mockProv.assertionErrors, "\n- "))
	}
	if !mockProv.sawCopilotInstructions {
		t.Fatalf("expected .github/copilot-instructions.md to be loaded")
	}
	if !mockProv.sawLazySkillCatalog {
		t.Fatalf("expected 2-stage lazy skill catalog in system prompt")
	}
	if !mockProv.sawLoadedSkillSpec {
		t.Fatalf("expected load_skill to load full SKILL.md on demand")
	}

	if report.TotalProblems != 2 || report.PassedProblems != 2 || report.FailedProblems != 0 {
		for _, r := range report.Results {
			t.Logf("Result %s: status=%s err=%q verifyOut=%q", r.ProblemID, r.Status, r.Error, r.VerifyOutput)
		}
		t.Fatalf("expected 2/2 passed, got %d/%d (passRate=%.1f%%)", report.PassedProblems, report.TotalProblems, report.PassRatePct)
	}

	// Confirm C++ was verified with real C++ compiler and Python with python.
	cppRes := report.Results[0]
	if cppRes.ProblemID != "cpp_lru_ttl" || !cppRes.Passed || cppRes.LLMCalls != 3 || cppRes.ToolCalls != 2 {
		t.Fatalf("unexpected cpp_lru_ttl result: %+v", cppRes)
	}
	if cppRes.Compiler == "" {
		t.Errorf("expected cpp_lru_ttl compiler to be recorded")
	}

	pyRes := report.Results[1]
	if pyRes.ProblemID != "py_copilot_skill_compat" || !pyRes.Passed || pyRes.LLMCalls != 3 || pyRes.ToolCalls != 3 {
		t.Fatalf("unexpected py_copilot_skill_compat result: %+v", pyRes)
	}

	scorecard := FormatScorecard(report)
	for _, needle := range []string{"cpp_lru_ttl", "py_copilot_skill_compat", "PASS", "2/2 passed (100.0%)"} {
		if !strings.Contains(scorecard, needle) {
			t.Errorf("scorecard missing %q:\n%s", needle, scorecard)
		}
	}
	if !strings.Contains(verboseBuf.String(), "load_skill") {
		t.Errorf("verbose stderr output did not log load_skill tool call:\n%s", verboseBuf.String())
	}
}

func TestNoCopilotCompatDisablesRepoCopilotCustomizations(t *testing.T) {
	var capturedPrompt string
	var capturedTools []types.ToolDefinition

	probe := &singleStepCaptureProvider{
		onReq: func(req types.ChatRequest) {
			capturedPrompt = req.SystemPrompt
			capturedTools = req.Tools
		},
	}

	problems, err := LoadProblems(filepath.Join("..", "..", "testdata", "problems"), []string{"py_copilot_skill_compat"})
	if err != nil {
		t.Fatalf("LoadProblems failed: %v", err)
	}

	res := RunProblem(context.Background(), problems[0], SuiteOptions{
		UserHome:       t.TempDir(),
		CopilotCompat:  config.Bool(false),
		CustomProvider: probe,
	})
	if res.Passed {
		t.Fatalf("expected unmodified workspace to fail verification")
	}
	if strings.Contains(capturedPrompt, "FRAME_SYNC_WORD") {
		t.Errorf("expected CopilotCompat=false to exclude .github/copilot-instructions.md from prompt")
	}
	if hasTool(capturedTools, "load_skill") {
		t.Errorf("expected CopilotCompat=false to exclude .github/skills from load_skill registration")
	}
}

func TestRunCLIFlagsListAndHelp(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "problems")

	var out, errBuf bytes.Buffer
	code := RunCLI([]string{"--list", "--problems-dir", root}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("RunCLI(--list) exited %d: %s", code, errBuf.String())
	}
	for _, id := range []string{"cpp_lru_ttl", "cpp_graph_bugfix", "py_expr_engine", "py_dag_scheduler_bugfix", "py_copilot_skill_compat"} {
		if !strings.Contains(out.String(), id) {
			t.Errorf("--list output missing %q:\n%s", id, out.String())
		}
	}

	out.Reset()
	errBuf.Reset()
	code = RunCLI([]string{"--list", "--lang", "cpp", "--json", "--problems-dir", root}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("RunCLI(--list --lang cpp --json) exited %d: %s", code, errBuf.String())
	}
	var listed []Problem
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
		t.Fatalf("failed to parse --list --json output: %v\n%s", err, out.String())
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 cpp problems in JSON list, got %d", len(listed))
	}

	out.Reset()
	errBuf.Reset()
	code = RunCLI([]string{"--list", "--problems", "cpp_lru_ttl,py_expr_engine", "--no-copilot-compat", "--json", "--problems-dir", root}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("RunCLI(--list --problems ... --no-copilot-compat) exited %d: %s", code, errBuf.String())
	}
	var subsetListed []Problem
	if err := json.Unmarshal(out.Bytes(), &subsetListed); err != nil || len(subsetListed) != 2 {
		t.Fatalf("expected 2 filtered problems in JSON list, got %d (err=%v)", len(subsetListed), err)
	}

	out.Reset()
	errBuf.Reset()
	if code := RunCLI([]string{"--list", "--copilot-compat=false", "--problems-dir", root}, &out, &errBuf); code != 0 {
		t.Fatalf("RunCLI(--copilot-compat=false) exited %d: %s", code, errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := RunCLI([]string{"--help"}, &out, &errBuf); code != 0 || !strings.Contains(out.String(), "--no-copilot-compat") {
		t.Fatalf("RunCLI(--help) unexpected code=%d out=%q", code, out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := RunCLI([]string{"--unknown-flag"}, &out, &errBuf); code == 0 {
		t.Fatalf("expected non-zero exit code for invalid flag")
	}
}

type singleStepCaptureProvider struct {
	onReq func(types.ChatRequest)
}

func (p *singleStepCaptureProvider) Name() string         { return "capture" }
func (p *singleStepCaptureProvider) DefaultModel() string { return "capture-1" }
func (p *singleStepCaptureProvider) ListModels(context.Context) ([]types.ModelInfo, error) {
	return nil, nil
}
func (p *singleStepCaptureProvider) Close() error { return nil }
func (p *singleStepCaptureProvider) StreamChat(_ context.Context, req types.ChatRequest, _ func(types.StreamEvent)) (*types.ChatResponse, error) {
	if p.onReq != nil {
		p.onReq(req)
	}
	return &types.ChatResponse{Content: "done"}, nil
}

func hasTool(defs []types.ToolDefinition, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func findToolResult(msgs []types.Message, callID string) (types.Message, bool) {
	for _, m := range msgs {
		if m.Role == types.RoleTool && m.ToolCallID == callID {
			return m, true
		}
	}
	return types.Message{}, false
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

const referenceCPPLRUTTLHeader = `#pragma once

#include <cstddef>
#include <cstdint>
#include <iterator>
#include <list>
#include <map>
#include <optional>
#include <unordered_map>
#include <utility>

template <typename K, typename V>
class TTLLRUCache {
public:
    explicit TTLLRUCache(size_t capacity) : capacity_(capacity) {}

    size_t capacity() const noexcept { return capacity_; }

    void put(const K& key, V value, int64_t ttl_ms, int64_t now_ms = 0) {
        if (capacity_ == 0) {
            return;
        }
        if (ttl_ms <= 0) {
            erase(key);
            return;
        }

        const int64_t expiry_ms = now_ms + ttl_ms;
        if (auto it = index_.find(key); it != index_.end()) {
            auto node_it = it->second;
            node_it->value = std::move(value);
            node_it->expiry_ms = expiry_ms;
            expiry_index_.erase(node_it->exp_it);
            node_it->exp_it = expiry_index_.emplace(expiry_ms, key);
            lru_list_.splice(lru_list_.begin(), lru_list_, node_it);
            return;
        }

        if (index_.size() >= capacity_) {
            purge_expired(now_ms);
            if (index_.size() >= capacity_) {
                auto victim_it = std::prev(lru_list_.end());
                expiry_index_.erase(victim_it->exp_it);
                index_.erase(victim_it->key);
                lru_list_.pop_back();
            }
        }

        auto exp_it = expiry_index_.emplace(expiry_ms, key);
        lru_list_.push_front(Node{key, std::move(value), expiry_ms, exp_it});
        index_.emplace(lru_list_.front().key, lru_list_.begin());
    }

    std::optional<V> get(const K& key, int64_t now_ms) {
        auto it = index_.find(key);
        if (it == index_.end()) {
            return std::nullopt;
        }
        auto node_it = it->second;
        if (now_ms >= node_it->expiry_ms) {
            expiry_index_.erase(node_it->exp_it);
            lru_list_.erase(node_it);
            index_.erase(it);
            return std::nullopt;
        }
        lru_list_.splice(lru_list_.begin(), lru_list_, node_it);
        return node_it->value;
    }

    bool erase(const K& key) {
        auto it = index_.find(key);
        if (it == index_.end()) {
            return false;
        }
        auto node_it = it->second;
        expiry_index_.erase(node_it->exp_it);
        lru_list_.erase(node_it);
        index_.erase(it);
        return true;
    }

    size_t purge_expired(int64_t now_ms) {
        size_t removed = 0;
        auto exp_it = expiry_index_.begin();
        while (exp_it != expiry_index_.end() && exp_it->first <= now_ms) {
            auto idx_it = index_.find(exp_it->second);
            if (idx_it != index_.end()) {
                lru_list_.erase(idx_it->second);
                index_.erase(idx_it);
            }
            exp_it = expiry_index_.erase(exp_it);
            ++removed;
        }
        return removed;
    }

    size_t size(int64_t now_ms) {
        purge_expired(now_ms);
        return index_.size();
    }

private:
    using ExpiryMap = std::multimap<int64_t, K>;

    struct Node {
        K key;
        V value;
        int64_t expiry_ms;
        typename ExpiryMap::iterator exp_it;
    };

    using NodeList = std::list<Node>;

    size_t capacity_;
    NodeList lru_list_;
    std::unordered_map<K, typename NodeList::iterator> index_;
    ExpiryMap expiry_index_;
};
`

const referencePyTelemetryCodec = `"""Telemetry packet encoder/decoder (telemetry.v2)."""

from __future__ import annotations

import json
import math
import struct

CODEC_SCHEMA_VERSION = "telemetry.v2"
FRAME_SYNC_WORD = 0xCAFE
_PROTOCOL_VERSION = 0x02

_STATUS_TO_CODE = {
    "OK": 0x01,
    "WARN": 0x02,
    "CRIT": 0x03,
    "MAINT": 0x04,
}
_CODE_TO_STATUS = {v: k for k, v in _STATUS_TO_CODE.items()}


class TelemetryCodecError(ValueError):
    """Base exception for all telemetry codec errors."""


class ChecksumMismatchError(TelemetryCodecError):
    """Raised when trailing CRC-16 checksum mismatches."""


class FrameFormatError(TelemetryCodecError):
    """Raised on malformed frames or invalid packet fields."""


__all__ = [
    "CODEC_SCHEMA_VERSION",
    "FRAME_SYNC_WORD",
    "TelemetryCodecError",
    "ChecksumMismatchError",
    "FrameFormatError",
    "encode_packet",
    "decode_packet",
]


def _crc16_ccitt_false(data: bytes | memoryview) -> int:
    crc = 0xFFFF
    for byte in data:
        crc ^= byte << 8
        for _ in range(8):
            if crc & 0x8000:
                crc = ((crc << 1) ^ 0x1021) & 0xFFFF
            else:
                crc = (crc << 1) & 0xFFFF
    return crc


def _validate_metrics(metrics: object) -> dict:
    if not isinstance(metrics, dict):
        raise FrameFormatError("metrics must be a dict")
    for k, v in metrics.items():
        if not isinstance(k, str):
            raise FrameFormatError("metric keys must be str")
        if isinstance(v, bool):
            continue
        if isinstance(v, int):
            continue
        if isinstance(v, float):
            if not math.isfinite(v):
                raise FrameFormatError("non-finite float in metrics")
            continue
        if isinstance(v, str):
            continue
        raise FrameFormatError(f"unsupported metric value type: {type(v).__name__}")
    return metrics


def encode_packet(packet: dict) -> bytes:
    if not isinstance(packet, dict):
        raise FrameFormatError("packet must be a dict")

    for req_key in ("device_id", "seq", "status", "metrics"):
        if req_key not in packet:
            raise FrameFormatError(f"missing required key: {req_key}")

    device_id = packet["device_id"]
    if not isinstance(device_id, str) or not (1 <= len(device_id) <= 64):
        raise FrameFormatError("device_id must be 1..64 printable ASCII chars")
    if not all(0x20 <= ord(ch) <= 0x7E for ch in device_id):
        raise FrameFormatError("device_id contains non-printable-ASCII characters")
    dev_bytes = device_id.encode("ascii")

    seq = packet["seq"]
    if isinstance(seq, bool) or not isinstance(seq, int) or not (0 <= seq <= 0xFFFF):
        raise FrameFormatError("seq must be uint16 (0..65535)")

    status = packet["status"]
    if not isinstance(status, str) or status not in _STATUS_TO_CODE:
        raise FrameFormatError(f"unknown status: {status!r}")
    status_code = _STATUS_TO_CODE[status]

    flags = packet.get("flags", 0)
    if isinstance(flags, bool) or not isinstance(flags, int) or not (0 <= flags <= 0xFF):
        raise FrameFormatError("flags must be uint8 (0..255)")

    metrics = _validate_metrics(packet["metrics"])
    try:
        payload_bytes = json.dumps(
            metrics,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
            allow_nan=False,
        ).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise FrameFormatError(f"invalid metrics JSON: {exc}") from exc

    if len(payload_bytes) > 0xFFFF:
        raise FrameFormatError("metrics JSON payload exceeds 65535 bytes")

    header = struct.pack(
        ">HBBBBHH",
        FRAME_SYNC_WORD,
        _PROTOCOL_VERSION,
        status_code,
        flags,
        len(dev_bytes),
        seq,
        len(payload_bytes),
    )
    body = header + dev_bytes + payload_bytes
    crc = _crc16_ccitt_false(body)
    return body + struct.pack(">H", crc)


def decode_packet(frame: bytes | bytearray) -> dict:
    if not isinstance(frame, (bytes, bytearray)) or len(frame) < 12:
        raise FrameFormatError("frame must be bytes/bytearray of at least 12 bytes")

    raw = bytes(frame)
    sync, version, status_code, flags, dev_len, seq, payload_len = struct.unpack(">HBBBBHH", raw[:10])
    if sync != FRAME_SYNC_WORD:
        raise FrameFormatError(f"invalid sync word: 0x{sync:04X}")
    if version != _PROTOCOL_VERSION:
        raise FrameFormatError(f"unsupported protocol version: 0x{version:02X}")
    if not (1 <= dev_len <= 64):
        raise FrameFormatError(f"invalid device_id_len: {dev_len}")
    if len(raw) != 10 + dev_len + payload_len + 2:
        raise FrameFormatError("frame length mismatch")

    (expected_crc,) = struct.unpack(">H", raw[-2:])
    actual_crc = _crc16_ccitt_false(raw[:-2])
    if actual_crc != expected_crc:
        raise ChecksumMismatchError(
            f"CRC mismatch: expected 0x{expected_crc:04X}, got 0x{actual_crc:04X}"
        )

    if status_code not in _CODE_TO_STATUS:
        raise FrameFormatError(f"unknown status wire byte: 0x{status_code:02X}")

    dev_raw = raw[10 : 10 + dev_len]
    if not all(0x20 <= b <= 0x7E for b in dev_raw):
        raise FrameFormatError("non-printable ASCII in device_id")
    device_id = dev_raw.decode("ascii")

    payload_raw = raw[10 + dev_len : -2]
    try:
        parsed = json.loads(payload_raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        raise FrameFormatError(f"invalid payload JSON: {exc}") from exc

    metrics = _validate_metrics(parsed)

    return {
        "schema": CODEC_SCHEMA_VERSION,
        "device_id": device_id,
        "seq": seq,
        "status": _CODE_TO_STATUS[status_code],
        "flags": flags,
        "metrics": metrics,
    }
`
