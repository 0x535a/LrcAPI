package util

import (
	"os"
	"path/filepath"
	"testing"
)

// withTestEnvVars 记录并临时清空 LLM 相关环境变量，测试结束后恢复。
func withTestEnvVars(t *testing.T, kv map[string]string) {
	t.Helper()
	keys := []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL"}
	for _, k := range keys {
		old, had := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
	for k, v := range kv {
		_ = os.Setenv(k, v)
	}
}

// chdirTmp 切换到临时目录，测试结束后切回并清理。
func chdirTmp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	return dir
}

func TestLoadEnvFallsBackToAssetsDir(t *testing.T) {
	dir := chdirTmp(t)
	withTestEnvVars(t, nil)

	// 工作目录没有 .env，只有 assets/.env（对应生产容器挂载方式）
	assets := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(assets, ".env")
	content := "OPENAI_API_KEY=test-key-123\nOPENAI_BASE_URL=https://example.com\nOPENAI_MODEL=test-model\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	LoadEnv("")

	if OpenAIAPIKey != "test-key-123" {
		t.Errorf("OpenAIAPIKey = %q, want %q", OpenAIAPIKey, "test-key-123")
	}
	if OpenAIBaseURL != "https://example.com" {
		t.Errorf("OpenAIBaseURL = %q, want %q", OpenAIBaseURL, "https://example.com")
	}
	if OpenAIModel != "test-model" {
		t.Errorf("OpenAIModel = %q, want %q", OpenAIModel, "test-model")
	}
	if !LLMEnabled() {
		t.Error("LLMEnabled() = false, want true after loading assets/.env")
	}
}

func TestLoadEnvPrefersWorkingDirFile(t *testing.T) {
	dir := chdirTmp(t)
	withTestEnvVars(t, nil)

	assets := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, ".env"), []byte("OPENAI_MODEL=from-assets\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_MODEL=from-root\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	LoadEnv("")

	if OpenAIModel != "from-root" {
		t.Errorf("OpenAIModel = %q, want %q (工作目录 .env 应优先)", OpenAIModel, "from-root")
	}
}

func TestLoadEnvNoFilesKeepsZeroValues(t *testing.T) {
	chdirTmp(t)
	withTestEnvVars(t, nil)

	LoadEnv("")

	if LLMEnabled() {
		t.Error("LLMEnabled() = true, want false with no .env anywhere")
	}
}

func TestLoadEnvEnvVarsTakePrecedence(t *testing.T) {
	dir := chdirTmp(t)
	withTestEnvVars(t, map[string]string{"OPENAI_MODEL": "from-env"})

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_MODEL=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	LoadEnv("")

	if OpenAIModel != "from-env" {
		t.Errorf("OpenAIModel = %q, want %q (已有环境变量不应被 .env 覆盖)", OpenAIModel, "from-env")
	}
}

func TestLoadEnvExplicitPath(t *testing.T) {
	dir := chdirTmp(t)
	withTestEnvVars(t, nil)

	envFile := filepath.Join(dir, "custom.env")
	if err := os.WriteFile(envFile, []byte("OPENAI_API_KEY=explicit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	LoadEnv(envFile)

	if OpenAIAPIKey != "explicit" {
		t.Errorf("OpenAIAPIKey = %q, want %q", OpenAIAPIKey, "explicit")
	}
}
