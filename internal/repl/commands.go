package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"coder/internal/config"
	"coder/internal/customizations"
	"coder/internal/provider/copilot"
	"coder/internal/tools"
	"coder/internal/types"
)

func (s *sessionState) handleSlashCommand(ctx context.Context, line string) (exitREPL bool, err error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false, nil
	}

	cmd := strings.ToLower(fields[0])
	args := fields[1:]

	switch cmd {
	case "/exit", "/quit":
		return true, nil

	case "/help", "/?":
		s.cmdHelp()
		return false, nil

	case "/login":
		return false, s.cmdLogin(ctx, args)

	case "/config":
		return false, s.cmdConfig(args)

	case "/model":
		return false, s.cmdModel(ctx, args)

	case "/provider":
		return false, s.cmdProvider(ctx, args)

	case "/skills":
		s.cmdSkills()
		return false, nil

	case "/skill":
		return false, s.cmdSkill(args)

	case "/instructions":
		s.cmdInstructions()
		return false, nil

	case "/tools":
		s.cmdTools()
		return false, nil

	case "/mode":
		return false, s.cmdMode(args)

	case "/compat":
		return false, s.cmdCompat(ctx, args)

	case "/compact":
		return false, s.cmdCompact(args)

	case "/clear":
		s.rt.Agent.Clear()
		s.queuedSkill = ""
		s.printfOut("Conversation cleared.\n")
		return false, nil

	default:
		return false, fmt.Errorf("unknown command %q (type /help for available commands)", fields[0])
	}
}

func (s *sessionState) cmdHelp() {
	s.printfOut(`Commands:
  /help                                  List REPL commands
  /login [--host <url>]                  Sign in to GitHub Copilot via browser/device OAuth flow
  /config [show|save|path]               View live session config or save it to ~/.coder/config.json
  /model [list|<id> [--save]]            List/switch models; --save sets provider default
  /provider [<name> [model] [--save]]    List configured providers or switch live provider
  /skills                                List discovered skills
  /skill <name> [--queue]                Load & display a skill (optionally queue for next turn)
  /instructions                          List loaded global & path-scoped instruction files
  /tools                                 List registered built-in and custom tools
  /mode [smart_auto|full_auto|strict_ask] [--save] View or change live permission policy
  /compat [on|off] [--save]              Toggle Copilot instructions/skills compatibility & reload
  /compact [keep_turns]                  Compact conversation history and report reclaimed chars
  /clear                                 Reset conversation history (keeps provider warm)
  /exit, /quit                           Exit REPL (or press Ctrl+D)
`)
}

func extractSaveFlag(args []string) (cleaned []string, save bool, err error) {
	cleaned = make([]string, 0, len(args))
	for _, a := range args {
		if a == "--save" {
			save = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			return nil, false, fmt.Errorf("unknown flag %q", a)
		}
		cleaned = append(cleaned, a)
	}
	return cleaned, save, nil
}

func (s *sessionState) cmdConfig(args []string) error {
	action := "show"
	if len(args) > 0 {
		if len(args) > 1 || strings.HasPrefix(args[0], "-") {
			return fmt.Errorf("usage: /config [show|save|path]")
		}
		action = strings.ToLower(strings.TrimSpace(args[0]))
	}

	switch action {
	case "show":
		eff := s.rt.EffectiveConfig().Redacted()
		encoded, err := json.MarshalIndent(eff, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal session config: %w", err)
		}
		s.printfOut("%s\n", string(encoded))
		return nil

	case "save":
		return s.saveSessionConfig()

	case "path", "paths":
		userPath, userErr := config.UserConfigPath(s.rt.UserHome)
		if userErr != nil {
			userPath = fmt.Sprintf("(unavailable: %v)", userErr)
		}
		wsPath := config.WorkspaceConfigPath(s.rt.WorkspaceDir)
		s.printfOut("user:      %s\nworkspace: %s\n", userPath, wsPath)
		return nil

	default:
		return fmt.Errorf("usage: /config [show|save|path]")
	}
}

func (s *sessionState) saveSessionConfig() error {
	snap := s.rt.SnapshotSessionConfig()
	savedPath, err := config.SaveUserSessionConfig(s.rt.UserHome, snap)
	if err != nil {
		return fmt.Errorf("save session configuration: %w", err)
	}

	dispModel := snap.Model
	if dispModel == "" {
		dispModel = "auto"
	}
	compatLabel := "off"
	if snap.Compatibility.LoadCopilotInstructions || snap.Compatibility.LoadCopilotSkills {
		compatLabel = "on"
	}
	s.printfOut(
		"Saved session configuration to %s (provider=%s, model=%s, mode=%s, compat=%s).\n",
		savedPath,
		snap.Provider,
		dispModel,
		snap.ApprovalPolicy,
		compatLabel,
	)

	if s.rt.Gate != nil && s.rt.Gate.Policy() != snap.ApprovalPolicy {
		s.printfErr(
			"[note] temporary prompt approval (%q) was not persisted; saved approval_policy=%s (use '/mode %s --save' to persist it).\n",
			s.rt.Gate.Policy(),
			snap.ApprovalPolicy,
			s.rt.Gate.Policy(),
		)
	}
	if s.rt.HasAPIKeyOverride() || strings.TrimSpace(os.Getenv("CODER_API_KEY")) != "" {
		s.printfErr("[note] transient API key from --api-key / CODER_API_KEY was omitted from %s.\n", savedPath)
	}

	nextCfg, err := s.rt.NextSessionConfig()
	if err != nil {
		s.printfErr("[note] could not verify next-session precedence: %v\n", err)
	} else if shadowed := config.ShadowedFields(nextCfg, snap); len(shadowed) > 0 {
		s.printfErr(
			"[note] saved user profile settings are overridden in this workspace/environment (%s). Check workspace .coder/config.json and CODER_* environment variables.\n",
			strings.Join(shadowed, ", "),
		)
	}
	return nil
}

func (s *sessionState) cmdLogin(ctx context.Context, args []string) error {
	var host, proxyURL, token string
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--host" || args[i] == "-h") && i+1 < len(args):
			host = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--host="):
			host = strings.TrimPrefix(args[i], "--host=")
		case args[i] == "--proxy" && i+1 < len(args):
			proxyURL = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--proxy="):
			proxyURL = strings.TrimPrefix(args[i], "--proxy=")
		case args[i] == "--token" && i+1 < len(args):
			token = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--token="):
			token = strings.TrimPrefix(args[i], "--token=")
		}
	}

	if strings.TrimSpace(token) == "" {
		if copilot.LoginRunner != nil {
			if err := copilot.LoginRunner(ctx, host, s.stdin, s.stdout, s.stderr); err != nil {
				return fmt.Errorf("copilot login failed: %w", err)
			}
		} else {
			res, err := copilot.RunDeviceFlowLogin(ctx, copilot.DeviceLoginOptions{
				Host:     host,
				ProxyURL: proxyURL,
				Stdout:   s.stdout,
				Stderr:   s.stderr,
			})
			if err != nil {
				return fmt.Errorf("copilot login failed (%w) — tip: use '/login --token <github_pat_...>' or '/login --proxy http://127.0.0.1:<port>'", err)
			}
			token = res.Token
		}
	}

	if strings.TrimSpace(token) != "" {
		token = strings.TrimSpace(token)
		if strings.HasPrefix(token, "ghp_") {
			return fmt.Errorf("classic PATs (ghp_...) are not supported; create a fine-grained PAT (github_pat_...) with 'Copilot Requests: Read'")
		}
		if s.rt.Config != nil {
			prof := s.rt.Config.Providers["copilot"]
			prof.Name = "copilot"
			prof.Type = types.ProviderTypeCopilotSDK
			prof.APIKey = token
			if proxyURL != "" {
				if prof.Headers == nil {
					prof.Headers = make(map[string]string)
				}
				prof.Headers["proxy"] = proxyURL
			}
			s.rt.Config.Providers["copilot"] = prof
			_ = s.rt.Config.SaveUserConfig(s.rt.UserHome)
		}
	}

	// Reload the Copilot provider so the fresh credential-store token takes effect immediately.
	curModel := ""
	if strings.EqualFold(s.rt.Agent.ProviderName(), "copilot") {
		curModel = s.rt.Agent.Model()
	}
	if err := s.rt.SwitchProvider("copilot", curModel); err != nil {
		return fmt.Errorf("reload copilot provider after login: %w", err)
	}
	if s.rt.Config != nil {
		s.rt.Config.DefaultProvider = "copilot"
	}

	if cp, ok := s.rt.Provider.(*copilot.Provider); ok {
		statusCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if st, err := cp.AuthStatus(statusCtx); err == nil && st != nil && st.IsAuthenticated {
			if st.Login != nil && *st.Login != "" {
				s.printfOut("Signed in to GitHub Copilot as @%s (model: %s).\n", *st.Login, s.rt.Agent.Model())
				return nil
			}
		}
	}
	s.printfOut("GitHub Copilot login completed (provider: %s, model: %s).\n", s.rt.Agent.ProviderName(), s.rt.Agent.Model())
	return nil
}

func (s *sessionState) cmdModel(ctx context.Context, args []string) error {
	if len(args) == 0 || (len(args) == 1 && strings.EqualFold(args[0], "list")) {
		curModel := s.rt.Agent.Model()
		provName := s.rt.Agent.ProviderName()
		s.printfOut("Active model: %s (provider: %s)\n", curModel, provName)

		if s.rt.Provider == nil {
			return nil
		}
		listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		models, err := s.rt.Provider.ListModels(listCtx)
		if err != nil {
			s.printfErr("[note] unable to query live model catalog: %v\n", err)
			return nil
		}
		if len(models) == 0 {
			s.printfOut("Available models: (none reported by provider)\n")
			return nil
		}
		s.printfOut("Available models (%d):\n", len(models))
		for _, m := range models {
			marker := "  "
			if m.ID == curModel {
				marker = "* "
			}
			extra := ""
			if m.ContextSize > 0 {
				extra = fmt.Sprintf(" (ctx: %d)", m.ContextSize)
			}
			s.printfOut("  %s%s%s\n", marker, m.ID, extra)
		}
		return nil
	}

	if strings.HasPrefix(args[0], "-") || strings.EqualFold(args[0], "list") ||
		(len(args) > 1 && (len(args) != 2 || args[1] != "--save")) {
		return fmt.Errorf("usage: /model [list|<id> [--save]]")
	}
	targetModel := args[0]
	save := len(args) == 2
	provName := s.rt.Agent.ProviderName()
	isCopilot := strings.EqualFold(provName, "copilot")

	if save && isCopilot {
		if s.rt.Provider == nil {
			return fmt.Errorf("/model --save requires an initialized provider")
		}
		listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		models, err := s.rt.Provider.ListModels(listCtx)
		if err != nil {
			return fmt.Errorf("validate Copilot model before saving: %w", err)
		}
		var found bool
		for _, m := range models {
			if strings.EqualFold(m.ID, targetModel) {
				targetModel, found = m.ID, true
				break
			}
		}
		if !found {
			return fmt.Errorf("Copilot model %q is not available; use /model list", targetModel)
		}
	} else if save && s.rt.Provider != nil {
		listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if models, err := s.rt.Provider.ListModels(listCtx); err == nil {
			for _, m := range models {
				if strings.EqualFold(m.ID, targetModel) {
					targetModel = m.ID
					break
				}
			}
		}
		cancel()
	}

	if err := s.rt.Agent.SetModel(ctx, targetModel); err != nil {
		return err
	}
	if s.rt.Config != nil {
		if key, existing, ok := s.rt.Config.LookupProvider(provName); ok {
			existing.DefaultModel = s.rt.Agent.ConfiguredModel()
			s.rt.Config.Providers[key] = existing
		}
	}
	s.printfOut("Switched model to %s (provider: %s)\n", s.rt.Agent.Model(), provName)
	if !save {
		return nil
	}
	path, err := config.SaveUserProviderModel(s.rt.UserHome, provName, targetModel)
	if err != nil {
		return fmt.Errorf("model switched for this session, but could not save default: %w", err)
	}
	if isCopilot {
		s.printfOut("Saved Copilot default %s to %s.\n", targetModel, path)
	} else {
		s.printfOut("Saved %s default model %s to %s.\n", provName, targetModel, path)
	}
	cfg, err := s.rt.StartupConfig()
	if err != nil {
		s.printfErr("[note] could not check whether the saved default is overridden: %v\n", err)
	} else if !strings.EqualFold(cfg.DefaultProvider, provName) || !strings.EqualFold(cfg.EffectiveModel(provName), targetModel) {
		s.printfErr("[note] the saved %s default is overridden by current startup settings (provider: %s, model: %q). Check top-level and workspace config, environment variables, and CLI flags.\n", provName, cfg.DefaultProvider, cfg.EffectiveModel(cfg.DefaultProvider))
	}
	return nil
}

func (s *sessionState) cmdProvider(_ context.Context, args []string) error {
	cleaned, save, err := extractSaveFlag(args)
	if err != nil || len(cleaned) > 2 || (save && (len(cleaned) == 0 || strings.EqualFold(cleaned[0], "list"))) {
		return fmt.Errorf("usage: /provider [list|<name> [model] [--save]]")
	}
	if len(cleaned) == 0 || strings.EqualFold(cleaned[0], "list") {
		curProv := s.rt.Agent.ProviderName()
		curModel := s.rt.Agent.Model()
		s.printfOut("Active provider: %s (model: %s)\n", curProv, curModel)
		if s.rt.Config == nil || len(s.rt.Config.Providers) == 0 {
			return nil
		}

		names := make([]string, 0, len(s.rt.Config.Providers))
		for k := range s.rt.Config.Providers {
			names = append(names, k)
		}
		sort.Strings(names)

		s.printfOut("Configured providers (%d):\n", len(names))
		for _, name := range names {
			prof := s.rt.Config.Providers[name]
			marker := "  "
			if strings.EqualFold(name, curProv) {
				marker = "* "
			}
			loc := string(prof.Type)
			if prof.BaseURL != "" {
				loc += " @ " + prof.BaseURL
			}
			defModel := prof.DefaultModel
			if defModel == "" {
				defModel = "(default)"
			}
			s.printfOut("  %s%-12s [%s] default_model=%s\n", marker, name, loc, defModel)
		}
		return nil
	}

	targetProv := strings.TrimSpace(cleaned[0])
	var modelOverride string
	if len(cleaned) > 1 {
		modelOverride = strings.TrimSpace(cleaned[1])
	}
	if err := s.rt.SwitchProvider(targetProv, modelOverride); err != nil {
		return err
	}
	s.printfOut("Switched provider to %s (model: %s)\n", s.rt.Agent.ProviderName(), s.rt.Agent.Model())
	if save {
		return s.saveSessionConfig()
	}
	return nil
}

func (s *sessionState) cmdSkills() {
	if s.rt.Customizations == nil || s.rt.Customizations.Skills == nil {
		s.printfOut("Skills: (none)\n")
		return
	}
	skills := s.rt.Customizations.Skills.List()
	if len(skills) == 0 {
		s.printfOut("Skills: (none discovered)\n")
		return
	}
	s.printfOut("Discovered skills (%d):\n", len(skills))
	for _, sk := range skills {
		desc := strings.TrimSpace(sk.Description)
		if desc == "" {
			desc = "(no description)"
		}
		s.printfOut("  - %s [%s]: %s\n", sk.Name, sk.Source, desc)
	}
}

func (s *sessionState) cmdSkill(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: /skill <name> [--queue]")
	}
	if s.rt.Customizations == nil || s.rt.Customizations.Skills == nil {
		return fmt.Errorf("skill catalog is not initialized")
	}

	queueForTurn := false
	var skillName string
	for _, a := range args {
		switch strings.ToLower(a) {
		case "--queue", "-q", "queue":
			queueForTurn = true
		default:
			if skillName == "" {
				skillName = a
			}
		}
	}
	if skillName == "" {
		return fmt.Errorf("usage: /skill <name> [--queue]")
	}

	content, helpers, info, err := s.rt.Customizations.Skills.Load(skillName)
	if err != nil {
		return err
	}

	s.printfOut("Skill: %s (source: %s)\nFile:  %s\n", info.Name, info.Source, info.SkillFile)
	if len(helpers) > 0 {
		s.printfOut("Companion files (%d): %s\n", len(helpers), strings.Join(helpers, ", "))
	}
	s.printfOut("---\n%s\n---\n", strings.TrimSpace(content))

	if queueForTurn {
		s.queuedSkill = fmt.Sprintf("<skill name=%q path=%q>\n%s\n</skill>", info.Name, info.SkillFile, strings.TrimSpace(content))
		s.printfOut("Queued skill %q for the next prompt.\n", info.Name)
	}
	return nil
}

func (s *sessionState) cmdInstructions() {
	if s.rt.Customizations == nil || s.rt.Customizations.Instructions == nil || len(s.rt.Customizations.Instructions.All) == 0 {
		s.printfOut("Instructions: (none loaded)\n")
		return
	}

	inst := s.rt.Customizations.Instructions
	activeScopedPaths := make(map[string]bool)
	if len(inst.ScopedInstructions) > 0 && s.rt.Toolset != nil {
		for _, doc := range inst.ActiveScopedInstructions(s.rt.Toolset.WorkspaceFiles(1000)) {
			activeScopedPaths[doc.Path] = true
		}
	}

	s.printfOut("Global instructions (%d):\n", len(inst.GlobalInstructions))
	if len(inst.GlobalInstructions) == 0 {
		s.printfOut("  (none)\n")
	}
	for _, doc := range inst.GlobalInstructions {
		s.printfOut("  - [%s] %s\n", doc.Source, doc.Path)
	}

	s.printfOut("Path-scoped instructions (%d):\n", len(inst.ScopedInstructions))
	if len(inst.ScopedInstructions) == 0 {
		s.printfOut("  (none)\n")
	}
	for _, doc := range inst.ScopedInstructions {
		state := "inactive"
		if activeScopedPaths[doc.Path] {
			state = "active"
		}
		s.printfOut("  - [%s] applyTo=%q (%s) %s\n", doc.Source, doc.ApplyTo, state, doc.Path)
	}
}

func (s *sessionState) cmdTools() {
	if s.rt.Toolset == nil {
		s.printfOut("Tools: (none)\n")
		return
	}
	defs := s.rt.Toolset.Definitions()
	customSet := make(map[string]bool)
	if s.rt.Customizations != nil {
		for _, ct := range s.rt.Customizations.CustomTools {
			customSet[ct.Name] = true
		}
	}

	s.printfOut("Registered tools (%d):\n", len(defs))
	for _, d := range defs {
		origin := "builtin"
		if customSet[d.Name] {
			origin = "custom"
		}
		desc := truncateSingleLine(strings.Join(strings.Fields(d.Description), " "), 72)
		s.printfOut("  - %-16s [%s, %s] %s\n", d.Name, origin, d.Category, desc)
	}
}

func (s *sessionState) cmdMode(args []string) error {
	if s.rt.Gate == nil {
		return fmt.Errorf("permission gate is not initialized")
	}
	cleaned, save, err := extractSaveFlag(args)
	if err != nil || len(cleaned) > 1 || (save && len(cleaned) == 0) {
		return fmt.Errorf("usage: /mode [smart_auto|full_auto|strict_ask] [--save]")
	}
	if len(cleaned) == 0 {
		s.printfOut("Approval policy: %s\n", s.rt.Gate.Policy())
		return nil
	}

	raw := strings.ToLower(strings.TrimSpace(cleaned[0]))
	var policy types.ApprovalPolicy
	switch raw {
	case string(types.ApprovalSmartAuto):
		policy = types.ApprovalSmartAuto
	case string(types.ApprovalFullAuto), "yolo":
		policy = types.ApprovalFullAuto
	case string(types.ApprovalStrictAsk), "safe":
		policy = types.ApprovalStrictAsk
	default:
		return fmt.Errorf("invalid mode %q (expected smart_auto, full_auto, or strict_ask)", cleaned[0])
	}

	s.rt.Gate.SetPolicy(policy)
	if s.rt.Config != nil {
		s.rt.Config.ApprovalPolicy = policy
	}
	s.printfOut("Approval policy set to %s\n", policy)
	if save {
		return s.saveSessionConfig()
	}
	return nil
}

func (s *sessionState) cmdCompat(ctx context.Context, args []string) error {
	if s.rt.Config == nil {
		return fmt.Errorf("runtime configuration is nil")
	}
	cleaned, save, err := extractSaveFlag(args)
	if err != nil || len(cleaned) > 1 || (save && len(cleaned) == 0) {
		return fmt.Errorf("usage: /compat [on|off] [--save]")
	}
	if len(cleaned) == 0 {
		state := "off"
		if s.rt.Config.Compatibility.LoadCopilotInstructions || s.rt.Config.Compatibility.LoadCopilotSkills {
			state = "on"
		}
		s.printfOut(
			"Copilot compatibility: %s (instructions=%t, skills=%t, agents_md=%t)\n",
			state,
			s.rt.Config.Compatibility.LoadCopilotInstructions,
			s.rt.Config.Compatibility.LoadCopilotSkills,
			s.rt.Config.Compatibility.LoadAgentsMD,
		)
		return nil
	}

	var enabled bool
	switch strings.ToLower(strings.TrimSpace(cleaned[0])) {
	case "on", "true", "1", "yes":
		enabled = true
	case "off", "false", "0", "no":
		enabled = false
	default:
		return fmt.Errorf("invalid /compat value %q (expected on or off)", cleaned[0])
	}

	s.rt.Config.Compatibility.LoadCopilotInstructions = enabled
	s.rt.Config.Compatibility.LoadCopilotSkills = enabled

	reloaded, err := customizations.LoadAll(s.rt.WorkspaceDir, s.rt.UserHome, s.rt.Config.Compatibility)
	if err != nil {
		return fmt.Errorf("reload customizations: %w", err)
	}

	// Mutate *s.rt.Customizations in place when non-nil so the closure captured inside
	// bootstrap.Build's dynamic promptBuilder immediately observes the updated bundle.
	if s.rt.Customizations != nil {
		*s.rt.Customizations = *reloaded
	} else {
		s.rt.Customizations = reloaded
	}

	maxOut := 32768
	if s.rt.Config.MaxOutputBytes > 0 {
		maxOut = s.rt.Config.MaxOutputBytes
	}
	newToolset := tools.NewToolset(tools.ToolsetOptions{
		WorkspaceDir:   s.rt.WorkspaceDir,
		Approver:       s.rt.Gate,
		Skills:         s.rt.Customizations.Skills,
		CustomTools:    s.rt.Customizations.CustomTools,
		MaxOutputBytes: maxOut,
	})
	s.rt.Toolset = newToolset

	if err := s.rt.Agent.SetTools(ctx, newToolset.Tools()); err != nil {
		return fmt.Errorf("update agent tools: %w", err)
	}

	label := "off"
	if enabled {
		label = "on"
	}
	globalCount, scopedActive, scopedTotal, skillCount, customToolCount := s.customizationCounts()
	s.printfOut(
		"Copilot compatibility %s: %d global + %d/%d scoped instructions, %d skills, %d custom tools\n",
		label,
		globalCount,
		scopedActive,
		scopedTotal,
		skillCount,
		customToolCount,
	)
	if save {
		return s.saveSessionConfig()
	}
	return nil
}

func (s *sessionState) cmdCompact(args []string) error {
	keepTurns := 4
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid keep_turns value %q (expected positive integer)", args[0])
		}
		keepTurns = n
	}
	reclaimed := s.rt.Agent.Compact(keepTurns)
	s.printfOut("Compacted history: reclaimed %d characters.\n", reclaimed)
	return nil
}
