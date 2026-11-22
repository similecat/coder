package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"coder/internal/config"
	"coder/internal/customizations"
	"coder/internal/provider/copilot"
	"coder/internal/tools"
	"coder/internal/types"
)

// DoctorCheck represents a single diagnostic check item in `coder doctor`.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ok", "info", "missing"
	Detail string `json:"detail"`
}

// DoctorReport holds the structured result of `coder doctor`.
type DoctorReport struct {
	Version               string                 `json:"version"`
	WorkspaceDir          string                 `json:"workspace_dir"`
	UserConfigPath        string                 `json:"user_config_path"`
	UserConfigExists      bool                   `json:"user_config_exists"`
	WorkspaceConfigPath   string                 `json:"workspace_config_path"`
	WorkspaceConfigExists bool                   `json:"workspace_config_exists"`
	ActiveProvider        string                 `json:"active_provider"`
	EffectiveModel        string                 `json:"effective_model"`
	ApprovalPolicy        string                 `json:"approval_policy"`
	Compatibility         config.Compatibility   `json:"compatibility"`
	GlobalInstructions    int                    `json:"global_instructions"`
	ScopedInstructions    int                    `json:"scoped_instructions"`
	Skills                int                    `json:"skills"`
	CustomTools           int                    `json:"custom_tools"`
	Checks                []DoctorCheck          `json:"checks"`
}

// LocalProbeURL overrides the default Ollama probe endpoint in unit tests when non-empty.
var LocalProbeURL = "http://127.0.0.1:11434/api/tags"

func runDoctorCommand(
	wsDir string,
	userHome string,
	parsed parsedCLI,
	stdout io.Writer,
	stderr io.Writer,
) int {
	userCfgPath, _ := config.UserConfigPath(userHome)
	wsCfgPath := config.WorkspaceConfigPath(wsDir)

	cfg, err := config.Load(wsDir, userHome, parsed.overrides)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coder doctor: load config: %v\n", err)
		return 1
	}

	bundle, err := customizations.LoadAll(wsDir, userHome, cfg.Compatibility)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coder doctor: load customizations: %v\n", err)
		return 1
	}

	report := DoctorReport{
		Version:               Version,
		WorkspaceDir:          wsDir,
		UserConfigPath:        userCfgPath,
		UserConfigExists:      fileExists(userCfgPath),
		WorkspaceConfigPath:   wsCfgPath,
		WorkspaceConfigExists: fileExists(wsCfgPath),
		ActiveProvider:        cfg.DefaultProvider,
		EffectiveModel:        cfg.EffectiveModel(cfg.DefaultProvider),
		ApprovalPolicy:        string(cfg.ApprovalPolicy),
		Compatibility:         cfg.Compatibility,
	}

	if report.EffectiveModel == "" {
		report.EffectiveModel = "(auto)"
	}

	if bundle.Instructions != nil {
		report.GlobalInstructions = len(bundle.Instructions.GlobalInstructions)
		report.ScopedInstructions = len(bundle.Instructions.ScopedInstructions)
	}
	if bundle.Skills != nil {
		report.Skills = len(bundle.Skills.List())
	}
	report.CustomTools = len(bundle.CustomTools)

	checks := make([]DoctorCheck, 7)
	var wg sync.WaitGroup

	// 0: GitHub Copilot CLI binary + 1: Copilot Auth status
	wg.Add(1)
	go func() {
		defer wg.Done()
		checks[0], checks[1] = checkCopilotCLIAndAuth(cfg)
	}()

	// 2: Local model server HTTP probe (500ms non-fatal)
	wg.Add(1)
	go func() {
		defer wg.Done()
		checks[2] = probeLocalModelServer(cfg)
	}()

	// 3: Python
	wg.Add(1)
	go func() {
		defer wg.Done()
		checks[3] = checkPython()
	}()

	// 4: C++ compiler (MSVC cl.exe / vcvars64 on Windows or g++/clang++)
	wg.Add(1)
	go func() {
		defer wg.Done()
		checks[4] = checkCppToolchain()
	}()

	// 5: Go toolchain
	wg.Add(1)
	go func() {
		defer wg.Done()
		checks[5] = checkCommandVersion("go", "go", "version")
	}()

	// 6: Ripgrep
	wg.Add(1)
	go func() {
		defer wg.Done()
		if rg := tools.ResolveRipgrepPath(); rg != "" {
			checks[6] = DoctorCheck{Name: "ripgrep", Status: "ok", Detail: rg}
		} else {
			checks[6] = DoctorCheck{Name: "ripgrep", Status: "info", Detail: "rg not found (using pure-Go regex search fallback)"}
		}
	}()

	wg.Wait()
	report.Checks = checks

	if parsed.jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return 0
	}

	printDoctorReport(stdout, report, bundle)
	return 0
}

func printDoctorReport(w io.Writer, r DoctorReport, bundle *customizations.Bundle) {
	_, _ = fmt.Fprintf(w, "coder doctor (v%s)\n", r.Version)
	_, _ = fmt.Fprintf(w, "  Workspace:         %s\n", r.WorkspaceDir)
	_, _ = fmt.Fprintf(w, "  User config:       %s (%s)\n", r.UserConfigPath, existsLabel(r.UserConfigExists))
	_, _ = fmt.Fprintf(w, "  Workspace config:  %s (%s)\n", r.WorkspaceConfigPath, existsLabel(r.WorkspaceConfigExists))
	_, _ = fmt.Fprintf(w, "  Active provider:   %s (model: %s, mode: %s)\n", r.ActiveProvider, r.EffectiveModel, r.ApprovalPolicy)
	_, _ = fmt.Fprintf(
		w,
		"  Compatibility:     copilot_instructions=%t, copilot_skills=%t, agents_md=%t\n",
		r.Compatibility.LoadCopilotInstructions,
		r.Compatibility.LoadCopilotSkills,
		r.Compatibility.LoadAgentsMD,
	)
	_, _ = fmt.Fprintf(
		w,
		"  Customizations:    %d global instructions, %d scoped instructions, %d skills, %d custom tools\n",
		r.GlobalInstructions,
		r.ScopedInstructions,
		r.Skills,
		r.CustomTools,
	)

	if bundle != nil && bundle.Skills != nil {
		if skills := bundle.Skills.List(); len(skills) > 0 {
			names := make([]string, 0, len(skills))
			for _, sk := range skills {
				names = append(names, fmt.Sprintf("%s (%s)", sk.Name, sk.Source))
			}
			_, _ = fmt.Fprintf(w, "  Skills list:       %s\n", strings.Join(names, ", "))
		}
	}

	_, _ = fmt.Fprintln(w, "\nProviders & Toolchains:")
	for _, c := range r.Checks {
		tag := "[OK]  "
		switch c.Status {
		case "info":
			tag = "[INFO]"
		case "missing":
			tag = "[MISS]"
		}
		_, _ = fmt.Fprintf(w, "  %s %-18s %s\n", tag, c.Name+":", c.Detail)
	}
}

func checkCopilotCLIAndAuth(cfg *config.Config) (DoctorCheck, DoctorCheck) {
	cliPath := copilot.ResolveCLIPath()
	if cliPath == "" {
		return DoctorCheck{
				Name:   "copilot-cli",
				Status: "missing",
				Detail: "copilot binary not found on PATH",
			}, DoctorCheck{
				Name:   "copilot-auth",
				Status: "info",
				Detail: "copilot CLI not installed",
			}
	}

	cliCheck := DoctorCheck{
		Name:   "copilot-cli",
		Status: "ok",
		Detail: cliPath,
	}

	for _, envName := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if strings.TrimSpace(os.Getenv(envName)) != "" {
			return cliCheck, DoctorCheck{
				Name:   "copilot-auth",
				Status: "ok",
				Detail: "token configured via " + envName,
			}
		}
	}

	prof := types.ProviderProfile{
		Name: "copilot",
		Type: types.ProviderTypeCopilotSDK,
	}
	if cfg != nil {
		if resolved, err := cfg.ResolveProvider("copilot"); err == nil {
			prof = resolved
		}
	}

	prov, err := copilot.New(prof)
	if err != nil {
		return cliCheck, DoctorCheck{Name: "copilot-auth", Status: "info", Detail: err.Error()}
	}
	defer func() { _ = prov.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	st, err := prov.AuthStatus(ctx)
	if err != nil {
		return cliCheck, DoctorCheck{
			Name:   "copilot-auth",
			Status: "info",
			Detail: fmt.Sprintf("unable to query auth status: %v", err),
		}
	}
	if st != nil && st.IsAuthenticated {
		detail := "authenticated"
		if st.Login != nil && *st.Login != "" {
			detail = "authenticated as @" + *st.Login
		}
		return cliCheck, DoctorCheck{
			Name:   "copilot-auth",
			Status: "ok",
			Detail: detail,
		}
	}
	return cliCheck, DoctorCheck{
		Name:   "copilot-auth",
		Status: "info",
		Detail: "not signed in — run 'coder login' (or '/login' in REPL) or set COPILOT_GITHUB_TOKEN",
	}
}

func probeLocalModelServer(cfg *config.Config) DoctorCheck {
	const defaultOllamaProbe = "http://127.0.0.1:11434/api/tags"
	candidates := []string{LocalProbeURL}
	if LocalProbeURL == defaultOllamaProbe {
		if cfg != nil {
			if prof, err := cfg.ResolveProvider(cfg.DefaultProvider); err == nil && isLocalHTTPURL(prof.BaseURL) {
				candidates = []string{strings.TrimRight(prof.BaseURL, "/") + "/models"}
			} else {
				candidates = []string{
					"http://127.0.0.1:11434/api/tags",
					"http://127.0.0.1:1234/v1/models",
					"http://127.0.0.1:8080/v1/models",
				}
			}
		}
	}

	client := &http.Client{Timeout: 500 * time.Millisecond}
	for _, probeURL := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			cancel()
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return DoctorCheck{
				Name:   "local-llm",
				Status: "ok",
				Detail: fmt.Sprintf("reachable at %s (HTTP %d)", probeURL, resp.StatusCode),
			}
		}
	}

	return DoctorCheck{
		Name:   "local-llm",
		Status: "info",
		Detail: fmt.Sprintf("%s offline (informational)", candidates[0]),
	}
}

func isLocalHTTPURL(u string) bool {
	lower := strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(lower, "http://127.0.0.1") ||
		strings.HasPrefix(lower, "http://localhost") ||
		strings.HasPrefix(lower, "http://[::1]")
}

func checkPython() DoctorCheck {
	candidates := []string{"python", "python3"}
	for _, bin := range candidates {
		if _, err := exec.LookPath(bin); err == nil {
			return checkCommandVersion("python", bin, "--version")
		}
	}
	return DoctorCheck{Name: "python", Status: "missing", Detail: "python not found on PATH"}
}

func checkCppToolchain() DoctorCheck {
	if runtime.GOOS == "windows" {
		if clPath, err := exec.LookPath("cl.exe"); err == nil {
			return DoctorCheck{Name: "c++", Status: "ok", Detail: "MSVC cl.exe (" + clPath + ")"}
		}
		if vcvars := tools.FindVCVars64(); vcvars != "" {
			return DoctorCheck{Name: "c++", Status: "ok", Detail: "MSVC vcvars64 (" + vcvars + ")"}
		}
	}
	for _, compiler := range []string{"g++", "clang++", "cl.exe"} {
		if p, err := exec.LookPath(compiler); err == nil {
			return DoctorCheck{Name: "c++", Status: "ok", Detail: fmt.Sprintf("%s (%s)", compiler, p)}
		}
	}
	return DoctorCheck{Name: "c++", Status: "missing", Detail: "no C++ compiler found (checked MSVC vcvars64, g++, clang++)"}
}

func checkCommandVersion(label string, bin string, arg string) DoctorCheck {
	path, err := exec.LookPath(bin)
	if err != nil {
		return DoctorCheck{Name: label, Status: "missing", Detail: bin + " not found on PATH"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, arg).CombinedOutput()
	if err != nil {
		return DoctorCheck{Name: label, Status: "ok", Detail: path}
	}
	firstLine := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	if firstLine == "" {
		firstLine = path
	}
	return DoctorCheck{Name: label, Status: "ok", Detail: firstLine}
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func existsLabel(exists bool) string {
	if exists {
		return "found"
	}
	return "not found, using defaults"
}
