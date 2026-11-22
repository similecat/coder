package customizations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"coder/internal/types"
)

// DiscoverCustomTools loads custom tool specs from <userHome>\.coder\tools\*.json
// and <workspaceDir>\.coder\tools\*.json (workspace overrides user on same Name).
func DiscoverCustomTools(workspaceDir string, userHome string) ([]types.CustomToolSpec, error) {
	home := resolveUserHome(userHome)
	ws := strings.TrimSpace(workspaceDir)

	byName := make(map[string]types.CustomToolSpec)

	var userToolsDir string
	if home != "" {
		userToolsDir = filepath.Join(home, ".coder", "tools")
		specs, err := loadCustomToolsDir(userToolsDir)
		if err != nil {
			return nil, err
		}
		for _, spec := range specs {
			byName[spec.Name] = spec
		}
	}

	if ws != "" {
		wsToolsDir := filepath.Join(ws, ".coder", "tools")
		if userToolsDir == "" || canonicalPathKey(userToolsDir) != canonicalPathKey(wsToolsDir) {
			specs, err := loadCustomToolsDir(wsToolsDir)
			if err != nil {
				return nil, err
			}
			for _, spec := range specs {
				byName[spec.Name] = spec
			}
		}
	}

	if len(byName) == 0 {
		return nil, nil
	}

	out := make([]types.CustomToolSpec, 0, len(byName))
	for _, spec := range byName {
		out = append(out, spec)
	}
	slices.SortFunc(out, func(a, b types.CustomToolSpec) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func loadCustomToolsDir(dir string) ([]types.CustomToolSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read custom tools dir %s: %w", dir, err)
	}

	var specs []types.CustomToolSpec
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			continue
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		fileSpecs, err := parseCustomToolFile(fullPath)
		if err != nil {
			return nil, err
		}
		specs = append(specs, fileSpecs...)
	}
	return specs, nil
}

func parseCustomToolFile(path string) ([]types.CustomToolSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read custom tool file %s: %w", path, err)
	}
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("custom tool file %s is empty", path)
	}

	var parsed []types.CustomToolSpec
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(trimmed, &parsed); err != nil {
			return nil, fmt.Errorf("parse custom tool array in %s: %w", path, err)
		}
	case '{':
		var single types.CustomToolSpec
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return nil, fmt.Errorf("parse custom tool object in %s: %w", path, err)
		}
		parsed = []types.CustomToolSpec{single}
	default:
		return nil, fmt.Errorf("custom tool file %s must contain a JSON object or array", path)
	}

	cleanPath := filepath.Clean(path)
	for i := range parsed {
		parsed[i].Name = strings.TrimSpace(parsed[i].Name)
		parsed[i].Command = strings.TrimSpace(parsed[i].Command)
		if parsed[i].Name == "" {
			return nil, fmt.Errorf("custom tool in %s missing required field \"name\"", path)
		}
		if parsed[i].Command == "" {
			return nil, fmt.Errorf("custom tool %q in %s missing required field \"command\"", parsed[i].Name, path)
		}
		if parsed[i].Parameters == nil {
			parsed[i].Parameters = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		parsed[i].SourceFile = cleanPath
	}
	return parsed, nil
}
