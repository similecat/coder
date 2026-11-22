package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	msvcOnce      sync.Once
	cachedMSVCMap map[string]string
	cachedVCVars  string
)

// FindVCVars64 locates Microsoft Visual Studio's vcvars64.bat on Windows.
// Returns an empty string on non-Windows platforms or if Visual Studio C++ tools are not installed.
func FindVCVars64() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	resolveMSVCOnce()
	return cachedVCVars
}

// ResolveMSVCEnv resolves and caches the Visual Studio x64 developer environment (cl.exe, nmake, link, INCLUDE, LIB).
// On Windows, vcvars64.bat is executed at most ONCE per process via sync.Once and merged with the current process
// environment so every subsequent compile invocation runs with 0ms environment setup overhead.
func ResolveMSVCEnv() []string {
	if runtime.GOOS != "windows" {
		return os.Environ()
	}
	resolveMSVCOnce()
	return buildMergedEnv(cachedMSVCMap, nil)
}

func resolveMSVCOnce() {
	msvcOnce.Do(func() {
		// Fast check: if cl.exe is already on PATH and INCLUDE + LIB are populated, no vcvars64 call is needed.
		if _, err := exec.LookPath("cl.exe"); err == nil && os.Getenv("INCLUDE") != "" && os.Getenv("LIB") != "" {
			return
		}

		vcvars := discoverVCVars64Path()
		cachedVCVars = vcvars
		if vcvars == "" {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		out, err := captureVCVarsEnvironment(ctx, vcvars)
		if err != nil || len(out) == 0 {
			return
		}

		parsed := make(map[string]string, 64)
		for _, rawLine := range strings.Split(out, "\n") {
			line := strings.TrimSuffix(rawLine, "\r")
			if line == "" || strings.HasPrefix(line, "=") {
				continue
			}
			eq := strings.IndexByte(line, '=')
			if eq <= 0 {
				continue
			}
			key := line[:eq]
			val := line[eq+1:]
			parsed[key] = val
		}
		if len(parsed) > 0 {
			cachedMSVCMap = parsed
		}
	})
}

func discoverVCVars64Path() string {
	// 1. Check vswhere.exe in standard Visual Studio Installer directories.
	var vswhereCandidates []string
	if pfx86 := os.Getenv("ProgramFiles(x86)"); pfx86 != "" {
		vswhereCandidates = append(vswhereCandidates, filepath.Join(pfx86, "Microsoft Visual Studio", "Installer", "vswhere.exe"))
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		vswhereCandidates = append(vswhereCandidates, filepath.Join(pf, "Microsoft Visual Studio", "Installer", "vswhere.exe"))
	}

	for _, vswhere := range vswhereCandidates {
		if fi, err := os.Stat(vswhere); err != nil || fi.IsDir() {
			continue
		}
		// Try VC component filter first, then fallback to all products.
		queries := [][]string{
			{"-all", "-products", "*", "-requires", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64", "-property", "installationPath"},
			{"-all", "-products", "*", "-property", "installationPath"},
		}
		for _, q := range queries {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			out, err := exec.CommandContext(ctx, vswhere, q...).Output()
			cancel()
			if err != nil || len(out) == 0 {
				continue
			}
			for _, rawDir := range strings.Split(string(out), "\n") {
				installDir := strings.TrimSpace(rawDir)
				if installDir == "" {
					continue
				}
				candidate := filepath.Join(installDir, "VC", "Auxiliary", "Build", "vcvars64.bat")
				if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
					return candidate
				}
			}
		}
	}

	// 2. Fallback to standard installation locations across VS 18/2026/2022/2019.
	var roots []string
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		roots = append(roots, filepath.Join(pf, "Microsoft Visual Studio"))
	}
	if pfx86 := os.Getenv("ProgramFiles(x86)"); pfx86 != "" {
		roots = append(roots, filepath.Join(pfx86, "Microsoft Visual Studio"))
	}

	versions := []string{"18", "2026", "2022", "2019"}
	editions := []string{"BuildTools", "Community", "Professional", "Enterprise"}

	for _, root := range roots {
		for _, ver := range versions {
			for _, ed := range editions {
				candidate := filepath.Join(root, ver, ed, "VC", "Auxiliary", "Build", "vcvars64.bat")
				if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
					return candidate
				}
			}
		}
		if matches, err := filepath.Glob(filepath.Join(root, "*", "*", "VC", "Auxiliary", "Build", "vcvars64.bat")); err == nil {
			for _, m := range matches {
				if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
					return m
				}
			}
		}
	}

	return ""
}

// buildMergedEnv overlays cached MSVC vars and optional extra overrides onto os.Environ()
// (case-insensitive key matching on Windows).
func buildMergedEnv(msvcVars, extraVars map[string]string) []string {
	base := os.Environ()
	if len(msvcVars) == 0 && len(extraVars) == 0 {
		return base
	}

	type entry struct {
		key string
		val string
	}
	var ordered []entry
	indexByKey := make(map[string]int, len(base)+len(msvcVars)+len(extraVars))

	normKey := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}

	put := func(k, v string) {
		if k == "" || strings.HasPrefix(k, "=") {
			return
		}
		nk := normKey(k)
		if idx, exists := indexByKey[nk]; exists {
			ordered[idx].val = v
			return
		}
		indexByKey[nk] = len(ordered)
		ordered = append(ordered, entry{key: k, val: v})
	}

	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		put(kv[:eq], kv[eq+1:])
	}
	for k, v := range msvcVars {
		put(k, v)
	}
	for k, v := range extraVars {
		put(k, v)
	}

	out := make([]string, len(ordered))
	for i, e := range ordered {
		out[i] = e.key + "=" + e.val
	}
	return out
}
