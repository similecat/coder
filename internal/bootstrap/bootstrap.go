package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"coder/internal/agent"
	"coder/internal/config"
	"coder/internal/customizations"
	"coder/internal/permissions"
	"coder/internal/prompt"
	"coder/internal/provider"
	"coder/internal/tools"
	"coder/internal/types"
)

// Options configures construction of a complete Runtime session.
type Options struct {
	WorkspaceDir      string
	UserHome          string
	Overrides         config.Overrides
	CustomProvider    types.Provider // Optional injected provider (e.g. for mock/unit tests)
	PromptFunc        permissions.PromptFunc
	Interactive       *bool
	Stdin             io.Reader
	Stderr            io.Writer
	ExtraSystemPrompt string
	WarmToolchains    bool // When true, warms MSVC environment in a background goroutine on Windows
}

// Runtime bundles the active Agent, Config, Customizations, Permission Gate, and Toolset.
type Runtime struct {
	WorkspaceDir     string
	UserHome         string
	Config           *config.Config
	Customizations   *customizations.Bundle
	Gate             *permissions.Gate
	Toolset          *tools.Toolset
	Provider         types.Provider
	Agent            *agent.Agent
	startupOverrides config.Overrides
}

// StartupConfig re-reads the configuration using the original command-line overrides.
func (r *Runtime) StartupConfig() (*config.Config, error) {
	return config.Load(r.WorkspaceDir, r.UserHome, r.startupOverrides)
}

// NextSessionConfig re-reads the configuration without transient command-line overrides,
// modeling how a fresh session in this workspace/environment will resolve configuration.
func (r *Runtime) NextSessionConfig() (*config.Config, error) {
	return config.Load(r.WorkspaceDir, r.UserHome, config.Overrides{})
}

// HasAPIKeyOverride reports whether an explicit CLI --api-key override was supplied at startup.
func (r *Runtime) HasAPIKeyOverride() bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.startupOverrides.APIKey) != ""
}

// EffectiveConfig returns a cloned Config synchronized with the live Agent's provider and configured model.
func (r *Runtime) EffectiveConfig() *config.Config {
	var base *config.Config
	if r != nil && r.Config != nil {
		base = r.Config.Clone()
	} else {
		base = config.DefaultConfig()
	}
	if r != nil && r.Agent != nil {
		provName := strings.TrimSpace(r.Agent.ProviderName())
		cfgModel := strings.TrimSpace(r.Agent.ConfiguredModel())
		if provName != "" {
			base.DefaultProvider = provName
		}
		base.DefaultModel = ""
		if provName != "" {
			if base.Providers == nil {
				base.Providers = make(map[string]types.ProviderProfile)
			}
			if key, existing, ok := base.LookupProvider(provName); ok {
				existing.DefaultModel = cfgModel
				base.Providers[key] = existing
			} else {
				base.Providers[provName] = types.ProviderProfile{
					Name:         provName,
					Type:         types.ProviderTypeOpenAICompat,
					DefaultModel: cfgModel,
				}
			}
		}
	}
	return base
}

// SnapshotSessionConfig captures the live session's effective configuration for saving to the user profile.
func (r *Runtime) SnapshotSessionConfig() config.SessionSnapshot {
	if r == nil {
		return config.DefaultConfig().SnapshotEffective()
	}
	provName := ""
	cfgModel := ""
	if r.Agent != nil {
		provName = r.Agent.ProviderName()
		cfgModel = r.Agent.ConfiguredModel()
	} else if r.Config != nil {
		provName = r.Config.DefaultProvider
		cfgModel = r.Config.EffectiveModel(provName)
	}
	if r.Config == nil {
		return config.DefaultConfig().Snapshot(provName, cfgModel)
	}
	return r.Config.Snapshot(provName, cfgModel)
}

// Build loads config, discovers instructions/skills/custom tools, sets up permissions & native tools,
// instantiates the configured provider, and returns a ready-to-run Runtime.
func Build(opts Options) (*Runtime, error) {
	ws := strings.TrimSpace(opts.WorkspaceDir)
	if ws == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve working directory: %w", err)
		}
		ws = cwd
	}
	absWS, err := filepath.Abs(ws)
	if err == nil {
		ws = absWS
	}

	userHome := strings.TrimSpace(opts.UserHome)
	if userHome == "" {
		if h, homeErr := os.UserHomeDir(); homeErr == nil {
			userHome = h
		}
	}

	cfg, err := config.Load(ws, userHome, opts.Overrides)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	bundle, err := customizations.LoadAll(ws, userHome, cfg.Compatibility)
	if err != nil {
		return nil, fmt.Errorf("load customizations: %w", err)
	}

	gate := permissions.NewGateWithOptions(permissions.GateOptions{
		WorkspaceDir: ws,
		Policy:       cfg.ApprovalPolicy,
		PromptFunc:   opts.PromptFunc,
		Interactive:  opts.Interactive,
		Stdin:        opts.Stdin,
		Stderr:       opts.Stderr,
	})

	ts := tools.NewToolset(tools.ToolsetOptions{
		WorkspaceDir:   ws,
		Approver:       gate,
		Skills:         bundle.Skills,
		CustomTools:    bundle.CustomTools,
		MaxOutputBytes: cfg.MaxOutputBytes,
	})

	if opts.WarmToolchains && runtime.GOOS == "windows" {
		go func() {
			_ = tools.ResolveMSVCEnv()
		}()
	}

	var prov types.Provider
	providerName := cfg.DefaultProvider
	if opts.CustomProvider != nil {
		prov = opts.CustomProvider
		providerName = prov.Name()
	} else {
		prof, profErr := cfg.ResolveProvider(providerName)
		if profErr != nil {
			return nil, profErr
		}
		prov, err = provider.New(prof)
		if err != nil {
			return nil, fmt.Errorf("init provider %q: %w", providerName, err)
		}
	}

	model := cfg.EffectiveModel(providerName)
	if opts.CustomProvider != nil && strings.TrimSpace(cfg.DefaultModel) != "" {
		model = strings.TrimSpace(cfg.DefaultModel)
	} else if opts.CustomProvider != nil && model == "" {
		model = prov.DefaultModel()
	}

	compilerHint := detectQuickToolchainHint()
	buildPrompt := func() string {
		var scoped []types.InstructionDoc
		if bundle.Instructions != nil && len(bundle.Instructions.ScopedInstructions) > 0 {
			wsFiles := ts.WorkspaceFiles(1000)
			scoped = bundle.Instructions.ActiveScopedInstructions(wsFiles)
		}
		var global []types.InstructionDoc
		if bundle.Instructions != nil {
			global = bundle.Instructions.GlobalInstructions
		}
		var skills []types.SkillInfo
		if bundle.Skills != nil {
			skills = bundle.Skills.List()
		}
		return prompt.Build(prompt.Options{
			WorkspaceDir:       ws,
			CompilerHint:       compilerHint,
			GlobalInstructions: global,
			ScopedInstructions: scoped,
			Skills:             skills,
			ExtraSystemPrompt:  opts.ExtraSystemPrompt,
		})
	}

	initialPrompt := buildPrompt()
	ag, err := agent.New(agent.Options{
		Provider:           prov,
		Model:              model,
		SystemPrompt:       initialPrompt,
		PromptBuilder:      buildPrompt,
		Tools:              ts.Tools(),
		WorkingDir:         ws,
		MaxSteps:           cfg.MaxSteps,
		MaxToolOutputBytes: cfg.MaxOutputBytes,
		ReasoningEffort:    cfg.ReasoningEffort,
		Streaming:          cfg.Streaming,
	})
	if err != nil {
		_ = prov.Close()
		return nil, err
	}

	// Warm dynamic-catalog providers (e.g. Copilot SDK) asynchronously in the background so
	// interactive `coder` prints the REPL prompt in <15ms while pre-warming the runtime & model list!
	if opts.WarmToolchains && opts.CustomProvider == nil {
		if resolver, ok := prov.(interface {
			ResolveModel(ctx context.Context, requested string) (string, bool, error)
		}); ok {
			go func(reqModel string) {
				resolveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				// Warm the catalog only; session creation resolves the current selection.
				_, _, _ = resolver.ResolveModel(resolveCtx, reqModel)
			}(model)
		}
	}

	return &Runtime{
		WorkspaceDir:     ws,
		UserHome:         userHome,
		Config:           cfg,
		Customizations:   bundle,
		Gate:             gate,
		Toolset:          ts,
		Provider:         prov,
		Agent:            ag,
		startupOverrides: opts.Overrides,
	}, nil
}

// SwitchProvider creates and activates a named provider profile from r.Config on the live Agent.
func (r *Runtime) SwitchProvider(providerName string, modelOverride string) error {
	prof, err := r.Config.ResolveProvider(providerName)
	if err != nil {
		return err
	}
	newProv, err := provider.New(prof)
	if err != nil {
		return err
	}
	model := strings.TrimSpace(modelOverride)
	if model == "" {
		model = prof.DefaultModel
	}
	oldProv := r.Provider
	if err := r.Agent.SetProvider(newProv, model); err != nil {
		_ = newProv.Close()
		return err
	}
	r.Provider = newProv
	if r.Config != nil {
		r.Config.DefaultProvider = r.Agent.ProviderName()
		r.Config.DefaultModel = ""
		if key, existing, ok := r.Config.LookupProvider(r.Agent.ProviderName()); ok {
			existing.DefaultModel = r.Agent.ConfiguredModel()
			r.Config.Providers[key] = existing
		}
	}
	if oldProv != nil && oldProv != newProv {
		_ = oldProv.Close()
	}
	return nil
}

// Close shuts down the Agent and active Provider cleanly.
func (r *Runtime) Close() error {
	if r == nil || r.Agent == nil {
		return nil
	}
	return r.Agent.Close()
}

// detectQuickToolchainHint performs sub-millisecond LookPath/stat checks without spawning subprocesses.
func detectQuickToolchainHint() string {
	var found []string
	if _, err := exec.LookPath("python"); err == nil {
		found = append(found, "python")
	} else if _, err := exec.LookPath("python3"); err == nil {
		found = append(found, "python3")
	}
	if _, err := exec.LookPath("g++"); err == nil {
		found = append(found, "g++")
	} else if _, err := exec.LookPath("clang++"); err == nil {
		found = append(found, "clang++")
	} else if _, err := exec.LookPath("cl.exe"); err == nil {
		found = append(found, "MSVC cl.exe")
	} else if runtime.GOOS == "windows" {
		if pfx86 := os.Getenv("ProgramFiles(x86)"); pfx86 != "" {
			vsInstaller := filepath.Join(pfx86, "Microsoft Visual Studio", "Installer", "vswhere.exe")
			if _, statErr := os.Stat(vsInstaller); statErr == nil {
				found = append(found, "MSVC cl.exe (vcvars64 auto-loaded in run_command)")
			}
		}
	}
	if _, err := exec.LookPath("go"); err == nil {
		found = append(found, "go")
	}
	if _, err := exec.LookPath("node"); err == nil {
		found = append(found, "node")
	}
	return strings.Join(found, ", ")
}
