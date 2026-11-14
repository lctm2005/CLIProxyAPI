package test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func traeQuickstartPath(t *testing.T) string {
	t.Helper()
	repoRoot, errAbs := filepath.Abs("..")
	if errAbs != nil {
		t.Fatalf("resolve repository root: %v", errAbs)
	}
	return filepath.Join(repoRoot, "trae-quickstart.sh")
}

func runTraeQuickstart(t *testing.T, stateDir string, overrides []string, commandName string) (string, error) {
	t.Helper()
	bashPath, errLookPath := exec.LookPath("bash")
	if errLookPath != nil {
		t.Skip("bash is required for quickstart lifecycle tests")
	}
	command := exec.Command(bashPath, traeQuickstartPath(t), commandName)
	command.Env = append(os.Environ(), append([]string{
		"TRAE_PROXY_STATE_DIR=" + stateDir,
		"TRAE_HOME=" + filepath.Join(stateDir, "trae-home"),
	}, overrides...)...)
	output, errRun := command.CombinedOutput()
	return string(output), errRun
}

func TestTraeQuickstartStoppedStatusProvidesNextSteps(t *testing.T) {
	output, errStatus := runTraeQuickstart(t, t.TempDir(), nil, "status")
	if errStatus == nil {
		t.Fatal("status for a stopped proxy must return a non-zero exit status")
	}
	for _, expected := range []string{
		"stopped",
		"./trae-quickstart.sh start",
		"./trae-quickstart.sh foreground",
		"TRAE_DEPLOY_CN.md",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("stopped status output missing %q:\n%s", expected, output)
		}
	}
}

func TestTraeQuickstartStatusRecognizesExternallyManagedHealthyProxy(t *testing.T) {
	if _, errLookPath := exec.LookPath("curl"); errLookPath != nil {
		t.Skip("curl is required by trae-quickstart.sh readiness checks")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer test-api-key" {
			http.Error(w, "unexpected request", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	serverURL, errParse := url.Parse(server.URL)
	if errParse != nil {
		t.Fatalf("parse test server URL: %v", errParse)
	}
	host, port, errSplit := net.SplitHostPort(serverURL.Host)
	if errSplit != nil {
		t.Fatalf("split test server address: %v", errSplit)
	}
	stateDir := t.TempDir()
	if errWrite := os.WriteFile(filepath.Join(stateDir, "api-key"), []byte("test-api-key\n"), 0o600); errWrite != nil {
		t.Fatalf("write test API key: %v", errWrite)
	}

	output, errStatus := runTraeQuickstart(t, stateDir, []string{
		"TRAE_PROXY_HOST=" + host,
		"TRAE_PROXY_PORT=" + port,
	}, "status")
	if errStatus != nil {
		t.Fatalf("status for externally managed healthy proxy failed: %v\n%s", errStatus, output)
	}
	for _, expected := range []string{"healthy", "not tracked by the quickstart PID file", "systemd"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("external proxy status output missing %q:\n%s", expected, output)
		}
	}
}

func TestTraeQuickstartVerifiesBackgroundStabilityAndWarnsNonInteractiveUsers(t *testing.T) {
	content, errRead := os.ReadFile(traeQuickstartPath(t))
	if errRead != nil {
		t.Fatalf("read trae-quickstart.sh: %v", errRead)
	}
	script := string(content)
	stabilityCall := `verify_background_stability "${pid}"`
	readyMessage := `info "Proxy is ready at $(base_url) (PID ${pid})."`
	if callIndex, readyIndex := strings.Index(script, stabilityCall), strings.Index(script, readyMessage); callIndex < 0 || readyIndex < 0 || callIndex > readyIndex {
		t.Fatalf("quickstart must verify background stability before reporting ready")
	}
	for _, expected := range []string{
		`if [[ ! -t 0 || ! -t 1 ]]; then`,
		"may terminate background child processes",
		"./trae-quickstart.sh foreground",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("quickstart missing non-interactive background warning %q", expected)
		}
	}
}

func TestTraeDeploymentGuideContainsCompleteUserSystemdWorkflow(t *testing.T) {
	repoRoot, errAbs := filepath.Abs("..")
	if errAbs != nil {
		t.Fatalf("resolve repository root: %v", errAbs)
	}
	content, errRead := os.ReadFile(filepath.Join(repoRoot, "TRAE_DEPLOY_CN.md"))
	if errRead != nil {
		t.Fatalf("read TRAE_DEPLOY_CN.md: %v", errRead)
	}
	doc := string(content)
	for _, expected := range []string{
		"### 用户级 systemd 托管（Linux）",
		"./trae-quickstart.sh prepare",
		"[Unit]",
		"WorkingDirectory=${repo_dir}",
		"ExecStart=",
		"systemctl --user enable --now cliproxy-trae.service",
		"journalctl --user -u cliproxy-trae.service -f",
		"systemctl --user disable --now cliproxy-trae.service",
	} {
		if !strings.Contains(doc, expected) {
			t.Fatalf("TRAE deployment guide missing systemd workflow fragment %q", expected)
		}
	}

	scriptContent, errScript := os.ReadFile(traeQuickstartPath(t))
	if errScript != nil {
		t.Fatalf("read trae-quickstart.sh: %v", errScript)
	}
	script := string(scriptContent)
	if !strings.Contains(script, "  prepare     Configure and build the proxy without starting it") || !strings.Contains(script, "prepare) prepare_runtime ;;") {
		t.Fatal("quickstart must expose the prepare command used by the systemd workflow")
	}
}
