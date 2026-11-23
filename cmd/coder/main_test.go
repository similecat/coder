package main

import (
	"bytes"
	"strings"
	"testing"

	"coder/internal/cli"
)

func TestRunVersionAndBenchWiring(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("run(--version) = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if got, want := strings.TrimSpace(stdout.String()), "coder v"+cli.Version; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-p", "copilot", "bench", "--list", "--lang", "cpp"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("run(bench --list --lang cpp) = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "cpp_lru_ttl") {
		t.Errorf("bench --list output missing cpp_lru_ttl:\n%s", out)
	}
	if strings.Contains(out, "py_expr_engine") {
		t.Errorf("bench --list --lang cpp unexpectedly included py_expr_engine:\n%s", out)
	}
}
