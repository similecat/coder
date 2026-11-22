package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"coder/internal/types"
)

const (
	defaultReadMaxLines = 400
	defaultReadMaxBytes = 32 * 1024  // 32KB default cap
	explicitEOFMaxBytes = 256 * 1024 // 256KB safety ceiling when end_line == -1
)

// ReadFileTool implements native fast file reading with line numbering and smart truncation.
type ReadFileTool struct {
	workspaceDir string
	approver     types.Approver
}

// NewReadFileTool constructs the read_file tool.
func NewReadFileTool(workspaceDir string, approver types.Approver) types.Tool {
	return &ReadFileTool{
		workspaceDir: normalizeWorkspaceDir(workspaceDir),
		approver:     approver,
	}
}

func (t *ReadFileTool) Definition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "read_file",
		Description: "Read a text file with 1-based line numbers ('1: ...'). Supports optional start_line and end_line (-1 for EOF; defaults to up to 400 lines or 32KB).",
		Category:    types.ToolCategoryRead,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path (relative to workspace root or absolute).",
				},
				"start_line": map[string]any{
					"type":        "integer",
					"description": "Optional 1-based starting line number (default: 1).",
				},
				"end_line": map[string]any{
					"type":        "integer",
					"description": "Optional 1-based inclusive ending line number (-1 for EOF; default reads up to 400 lines or 32KB).",
				},
			},
			"required": []string{"path"},
		},
	}
}

type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   *int   `json:"end_line"`
}

func (t *ReadFileTool) Execute(ctx context.Context, rawArgs json.RawMessage) (types.ToolResult, error) {
	start := time.Now()
	var args readFileArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("invalid read_file arguments: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	absPath, err := resolveToolPath(t.workspaceDir, args.Path)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("read_file path error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	disp := displayPath(t.workspaceDir, absPath)
	approved, reason, err := checkApproval(ctx, t.approver, types.ApprovalRequest{
		ToolName:   "read_file",
		Category:   types.ToolCategoryRead,
		Summary:    fmt.Sprintf("Read file %s", disp),
		TargetPath: absPath,
		WorkingDir: t.workspaceDir,
	})
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("read_file permission error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if !approved {
		return types.ToolResult{
			Content:    fmt.Sprintf("read_file permission denied (%s): %s", reason, disp),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("read_file error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if fi.IsDir() {
		return types.ToolResult{
			Content:    fmt.Sprintf("read_file error: %s is a directory (use list_dir instead)", disp),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if fi.Size() == 0 {
		return types.ToolResult{
			Content:    "(empty file)",
			DurationMs: time.Since(start).Milliseconds(),
			Metadata: map[string]any{
				"total_lines": 0,
				"path":        disp,
			},
		}, nil
	}

	f, err := os.Open(absPath)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("read_file open error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	defer f.Close()

	// Sample up to first 8KB for binary NUL check, then rewind.
	sample := make([]byte, 8192)
	nSample, _ := io.ReadFull(f, sample)
	if nSample > 0 && isBinarySample(sample[:nSample]) {
		return types.ToolResult{
			Content:    fmt.Sprintf("read_file error: %s appears to be a binary file (%d bytes)", disp, fi.Size()),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("read_file seek error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	startLine := args.StartLine
	if startLine <= 0 {
		startLine = 1
	}

	// Determine line/byte caps.
	explicitEnd := 0
	readToEOF := false
	maxLines := defaultReadMaxLines
	maxBytes := defaultReadMaxBytes

	if args.EndLine != nil {
		if *args.EndLine == -1 {
			readToEOF = true
			maxLines = 0 // unlimited lines up to explicitEOFMaxBytes
			maxBytes = explicitEOFMaxBytes
		} else if *args.EndLine > 0 {
			explicitEnd = *args.EndLine
			if explicitEnd < startLine {
				return types.ToolResult{
					Content:    fmt.Sprintf("read_file error: end_line (%d) must be >= start_line (%d) or -1", explicitEnd, startLine),
					IsError:    true,
					DurationMs: time.Since(start).Milliseconds(),
				}, nil
			}
			maxLines = explicitEnd - startLine + 1
			maxBytes = explicitEOFMaxBytes
		}
	}

	reader := bufio.NewReaderSize(f, 32*1024)
	var b strings.Builder
	totalLines := 0
	shownCount := 0
	firstShown := 0
	lastShown := 0
	truncatedByCap := false
	usedBytes := 0

	for {
		if err := ctx.Err(); err != nil {
			return types.ToolResult{
				Content:    fmt.Sprintf("read_file canceled: %v", err),
				IsError:    true,
				DurationMs: time.Since(start).Milliseconds(),
			}, nil
		}

		line, readErr := reader.ReadString('\n')
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		totalLines++

		if totalLines >= startLine && !truncatedByCap && (explicitEnd == 0 || totalLines <= explicitEnd) {
			if maxLines > 0 && shownCount >= maxLines {
				// Only mark truncatedByCap when using default cap (no explicit end_line).
				if explicitEnd == 0 && !readToEOF {
					truncatedByCap = true
				}
			} else {
				clean := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
				formatted := fmt.Sprintf("%d: %s", totalLines, clean)
				needed := len(formatted)
				if shownCount > 0 {
					needed++ // newline separator
				}
				if usedBytes+needed > maxBytes {
					if shownCount == 0 {
						// Always make progress on at least one very long line.
						keep := max(maxBytes-32, 64)
						if len(formatted) > keep {
							formatted = formatted[:keep] + " ...[line truncated]"
						}
						b.WriteString(formatted)
						shownCount = 1
						firstShown = totalLines
						lastShown = totalLines
						usedBytes = len(formatted)
					}
					truncatedByCap = true
				} else {
					if shownCount > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(formatted)
					if shownCount == 0 {
						firstShown = totalLines
					}
					shownCount++
					lastShown = totalLines
					usedBytes += needed
				}
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return types.ToolResult{
				Content:    fmt.Sprintf("read_file read error: %v", readErr),
				IsError:    true,
				DurationMs: time.Since(start).Milliseconds(),
			}, nil
		}
	}

	if startLine > totalLines {
		return types.ToolResult{
			Content:    fmt.Sprintf("read_file error: start_line (%d) exceeds total line count (%d) in %s", startLine, totalLines, disp),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
			Metadata: map[string]any{
				"total_lines": totalLines,
				"path":        disp,
			},
		}, nil
	}

	if truncatedByCap && lastShown < totalLines {
		_, _ = fmt.Fprintf(&b, "\n... [Truncated: showing lines %d-%d of %d total lines. Pass start_line=%d to continue reading.]",
			firstShown, lastShown, totalLines, lastShown+1)
	}

	return types.ToolResult{
		Content:    b.String(),
		DurationMs: time.Since(start).Milliseconds(),
		Metadata: map[string]any{
			"path":        disp,
			"start_line":  firstShown,
			"end_line":    lastShown,
			"total_lines": totalLines,
			"truncated":   truncatedByCap && lastShown < totalLines,
		},
	}, nil
}

// WriteFileTool implements atomic/clean file creation and overwriting.
type WriteFileTool struct {
	workspaceDir string
	approver     types.Approver
}

// NewWriteFileTool constructs the write_file tool.
func NewWriteFileTool(workspaceDir string, approver types.Approver) types.Tool {
	return &WriteFileTool{
		workspaceDir: normalizeWorkspaceDir(workspaceDir),
		approver:     approver,
	}
}

func (t *WriteFileTool) Definition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "write_file",
		Description: "Create or overwrite a file with exact text content, creating parent directories if needed.",
		Category:    types.ToolCategoryEdit,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path (relative to workspace root or absolute).",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Full text content to write to the file.",
				},
			},
			"required": []string{"path", "content"},
		},
	}
}

type writeFileArgs struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
}

func (t *WriteFileTool) Execute(ctx context.Context, rawArgs json.RawMessage) (types.ToolResult, error) {
	start := time.Now()
	var args writeFileArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("invalid write_file arguments: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if args.Content == nil {
		return types.ToolResult{
			Content:    "write_file error: 'content' field is required",
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	absPath, err := resolveToolPath(t.workspaceDir, args.Path)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("write_file path error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	content := *args.Content
	disp := displayPath(t.workspaceDir, absPath)

	approved, reason, err := checkApproval(ctx, t.approver, types.ApprovalRequest{
		ToolName:   "write_file",
		Category:   types.ToolCategoryEdit,
		Summary:    fmt.Sprintf("Write %s (%d bytes)", disp, len(content)),
		TargetPath: absPath,
		WorkingDir: t.workspaceDir,
	})
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("write_file permission error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if !approved {
		return types.ToolResult{
			Content:    fmt.Sprintf("write_file permission denied (%s): %s", reason, disp),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	data := []byte(content)
	if err := writeFileAtomic(absPath, data); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("write_file failed: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	lines := countTextLines(content)
	return types.ToolResult{
		Content:    fmt.Sprintf("Wrote %d bytes (%d lines) to %s", len(data), lines, disp),
		DurationMs: time.Since(start).Milliseconds(),
		Metadata: map[string]any{
			"path":  disp,
			"bytes": len(data),
			"lines": lines,
		},
	}, nil
}

// EditFileTool implements surgical string replacements with automatic Windows CRLF/LF preservation.
type EditFileTool struct {
	workspaceDir string
	approver     types.Approver
}

// NewEditFileTool constructs the edit_file tool.
func NewEditFileTool(workspaceDir string, approver types.Approver) types.Tool {
	return &EditFileTool{
		workspaceDir: normalizeWorkspaceDir(workspaceDir),
		approver:     approver,
	}
}

func (t *EditFileTool) Definition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "edit_file",
		Description: "Replace an exact string block (old_str -> new_str) in an existing file. Automatically normalizes CRLF/LF differences while preserving the file's dominant line ending.",
		Category:    types.ToolCategoryEdit,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path (relative to workspace root or absolute).",
				},
				"old_str": map[string]any{
					"type":        "string",
					"description": "Exact existing text block to replace (include surrounding lines for uniqueness).",
				},
				"new_str": map[string]any{
					"type":        "string",
					"description": "Replacement text block.",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "If true, replace all occurrences of old_str; if false (default), old_str must match uniquely once.",
				},
			},
			"required": []string{"path", "old_str", "new_str"},
		},
	}
}

type editFileArgs struct {
	Path       string  `json:"path"`
	OldStr     *string `json:"old_str"`
	NewStr     *string `json:"new_str"`
	ReplaceAll bool    `json:"replace_all"`
}

func (t *EditFileTool) Execute(ctx context.Context, rawArgs json.RawMessage) (types.ToolResult, error) {
	start := time.Now()
	var args editFileArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("invalid edit_file arguments: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if args.OldStr == nil || args.NewStr == nil {
		return types.ToolResult{
			Content:    "edit_file error: both 'old_str' and 'new_str' are required",
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	oldStr := *args.OldStr
	newStr := *args.NewStr
	if oldStr == "" {
		return types.ToolResult{
			Content:    "edit_file error: 'old_str' must not be empty (use write_file to create or overwrite an entire file)",
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if oldStr == newStr {
		return types.ToolResult{
			Content:    "edit_file error: 'old_str' and 'new_str' are identical; no changes needed",
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	absPath, err := resolveToolPath(t.workspaceDir, args.Path)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("edit_file path error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	disp := displayPath(t.workspaceDir, absPath)
	approved, reason, err := checkApproval(ctx, t.approver, types.ApprovalRequest{
		ToolName:   "edit_file",
		Category:   types.ToolCategoryEdit,
		Summary:    fmt.Sprintf("Edit %s", disp),
		TargetPath: absPath,
		WorkingDir: t.workspaceDir,
	})
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("edit_file permission error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if !approved {
		return types.ToolResult{
			Content:    fmt.Sprintf("edit_file permission denied (%s): %s", reason, disp),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	rawBytes, err := os.ReadFile(absPath)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("edit_file read error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	original := string(rawBytes)
	updated, replacedCount, diagErr := applySmartEdit(original, oldStr, newStr, args.ReplaceAll, disp)
	if diagErr != "" {
		return types.ToolResult{
			Content:    diagErr,
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	if err := writeFileAtomic(absPath, []byte(updated)); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("edit_file write failed: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	return types.ToolResult{
		Content:    fmt.Sprintf("Edited %s (%d replacement(s), %d lines total)", disp, replacedCount, countTextLines(updated)),
		DurationMs: time.Since(start).Milliseconds(),
		Metadata: map[string]any{
			"path":         disp,
			"replacements": replacedCount,
			"lines":        countTextLines(updated),
		},
	}, nil
}

func detectDominantLineEnding(s string) string {
	crlf := strings.Count(s, "\r\n")
	lfOnly := strings.Count(s, "\n") - crlf
	if crlf > 0 && crlf >= lfOnly {
		return "\r\n"
	}
	return "\n"
}

func toLF(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func applyDominantEnding(lfText, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(lfText, "\n", "\r\n")
	}
	return lfText
}

func applySmartEdit(original, oldStr, newStr string, replaceAll bool, disp string) (updated string, count int, errMsg string) {
	dominant := detectDominantLineEnding(original)

	// Strategy 1: Try literal match against original file bytes.
	exactCount := strings.Count(original, oldStr)
	if exactCount > 0 {
		if exactCount > 1 && !replaceAll {
			lines := findMatchLineNumbers(toLF(original), toLF(oldStr), 10)
			return "", 0, fmt.Sprintf(
				"edit_file failed: old_str matched %d times in %s (at lines %s). Include more surrounding context lines in old_str/new_str for uniqueness, or set replace_all=true.",
				exactCount, disp, formatIntList(lines))
		}
		effectiveNew := newStr
		// Preserve dominant CRLF when caller provided LF in new_str and did not explicitly specify CRLF in old_str.
		if dominant == "\r\n" && !strings.Contains(oldStr, "\r\n") && strings.Contains(newStr, "\n") {
			effectiveNew = applyDominantEnding(toLF(newStr), "\r\n")
		}
		if replaceAll {
			return strings.ReplaceAll(original, oldStr, effectiveNew), exactCount, ""
		}
		return strings.Replace(original, oldStr, effectiveNew, 1), 1, ""
	}

	// Strategy 2: If file uses CRLF and old_str used LF, try direct CRLF-adapted match in original (preserves untouched bytes 100%).
	if dominant == "\r\n" && !strings.Contains(oldStr, "\r\n") && strings.Contains(oldStr, "\n") {
		crlfOld := applyDominantEnding(toLF(oldStr), "\r\n")
		crlfNew := applyDominantEnding(toLF(newStr), "\r\n")
		crlfCount := strings.Count(original, crlfOld)
		if crlfCount > 0 {
			if crlfCount > 1 && !replaceAll {
				lines := findMatchLineNumbers(toLF(original), toLF(oldStr), 10)
				return "", 0, fmt.Sprintf(
					"edit_file failed: old_str matched %d times in %s (at lines %s). Include more surrounding context lines in old_str/new_str for uniqueness, or set replace_all=true.",
					crlfCount, disp, formatIntList(lines))
			}
			if replaceAll {
				return strings.ReplaceAll(original, crlfOld, crlfNew), crlfCount, ""
			}
			return strings.Replace(original, crlfOld, crlfNew, 1), 1, ""
		}
	}

	// Strategy 3: Full LF normalization fallback (handles mixed line endings or LF file with CRLF args),
	// restoring the original file's dominant line ending.
	normOrig := toLF(original)
	normOld := toLF(oldStr)
	normNew := toLF(newStr)
	if normOld != "" {
		normCount := strings.Count(normOrig, normOld)
		if normCount > 0 {
			if normCount > 1 && !replaceAll {
				lines := findMatchLineNumbers(normOrig, normOld, 10)
				return "", 0, fmt.Sprintf(
					"edit_file failed: old_str matched %d times in %s (at lines %s). Include more surrounding context lines in old_str/new_str for uniqueness, or set replace_all=true.",
					normCount, disp, formatIntList(lines))
			}
			var replacedLF string
			if replaceAll {
				replacedLF = strings.ReplaceAll(normOrig, normOld, normNew)
			} else {
				replacedLF = strings.Replace(normOrig, normOld, normNew, 1)
			}
			return applyDominantEnding(replacedLF, dominant), normCount, ""
		}
	}

	// Build actionable diagnostic when 0 matches occur.
	totalLines := countTextLines(normOrig)
	hint := buildMismatchHint(normOrig, normOld)
	return "", 0, fmt.Sprintf(
		"edit_file failed: old_str was not found in %s (%d lines).%s Use read_file to inspect exact text and indentation before retrying.",
		disp, totalLines, hint)
}

func findMatchLineNumbers(lfContent, lfNeedle string, maxMatches int) []int {
	if lfNeedle == "" {
		return nil
	}
	var lines []int
	searchFrom := 0
	lineSoFar := 1
	for len(lines) < maxMatches {
		idx := strings.Index(lfContent[searchFrom:], lfNeedle)
		if idx < 0 {
			break
		}
		absIdx := searchFrom + idx
		lineSoFar += strings.Count(lfContent[searchFrom:absIdx], "\n")
		lines = append(lines, lineSoFar)
		step := len(lfNeedle)
		if step == 0 {
			step = 1
		}
		lineSoFar += strings.Count(lfContent[absIdx:absIdx+step], "\n")
		searchFrom = absIdx + step
	}
	return lines
}

func buildMismatchHint(lfContent, lfOld string) string {
	firstNonEmpty := ""
	for _, l := range strings.Split(lfOld, "\n") {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" {
			firstNonEmpty = trimmed
			break
		}
	}
	if firstNonEmpty == "" {
		return ""
	}
	var nearLines []int
	for i, l := range strings.Split(lfContent, "\n") {
		if strings.Contains(strings.TrimSpace(l), firstNonEmpty) {
			nearLines = append(nearLines, i+1)
			if len(nearLines) >= 5 {
				break
			}
		}
	}
	if len(nearLines) > 0 {
		return fmt.Sprintf(" Similar text (possible whitespace/indentation or context mismatch) appears near line(s) %s.", formatIntList(nearLines))
	}
	return ""
}

func formatIntList(nums []int) string {
	if len(nums) == 0 {
		return "?"
	}
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

func countTextLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
