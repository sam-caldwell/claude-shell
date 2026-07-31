package container

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asymmetric-effort/convocate/internal/config"
	"github.com/asymmetric-effort/convocate/internal/user"
)

func testUserInfo() user.Info {
	return user.Info{
		UID:      1337,
		GID:      1337,
		Username: "convocate",
		HomeDir:  "/home/convocate",
	}
}

func testPaths() config.Paths {
	return config.Paths{
		ConvocateHome:   "/home/convocate",
		SessionsBase:    "/home/convocate",
		SkelDir:         "/home/convocate/.skel",
		ConvocateConfig: "/home/convocate/.claude",
		SSHDir:          "/home/convocate/.ssh",
		GitConfig:       "/home/convocate/.gitconfig",
	}
}

func TestNewRunner(t *testing.T) {
	r := NewRunner("test-uuid", "/tmp/session", testUserInfo(), testPaths())
	if r == nil {
		t.Fatal("NewRunner returned nil")
	}
	if r.sessionID != "test-uuid" {
		t.Errorf("sessionID = %q, want %q", r.sessionID, "test-uuid")
	}
	if r.sessionDir != "/tmp/session" {
		t.Errorf("sessionDir = %q, want %q", r.sessionDir, "/tmp/session")
	}
}

func TestNewRunnerWithExec(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("echo", "mock")
	}
	r := NewRunnerWithExec("test-uuid", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	if r == nil {
		t.Fatal("NewRunnerWithExec returned nil")
	}
}

func TestBuildRunArgs(t *testing.T) {
	tmpDir := t.TempDir()
	sshDir := filepath.Join(tmpDir, ".ssh")
	gitConfig := filepath.Join(tmpDir, ".gitconfig")
	convocateConfig := filepath.Join(tmpDir, ".claude")

	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitConfig, []byte("[user]"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(convocateConfig, 0700); err != nil {
		t.Fatal(err)
	}

	paths := config.Paths{
		ConvocateHome:   tmpDir,
		SessionsBase:    tmpDir,
		SkelDir:         filepath.Join(tmpDir, ".skel"),
		ConvocateConfig: convocateConfig,
		SSHDir:          sshDir,
		GitConfig:       gitConfig,
	}

	sessionDir := filepath.Join(tmpDir, "test-session")
	r := NewRunner("abcdef12-3456-7890-abcd-ef1234567890", sessionDir, testUserInfo(), paths)

	args := r.buildRunArgs("convocate-session-test")

	argStr := strings.Join(args, " ")

	checks := []struct {
		name    string
		pattern string
	}{
		{"--rm", "--rm"},
		{"--detach", "--detach"},
		{"-w", "-w /home/convocate"},
		{"--name", "--name convocate-session-test"},
		{"--hostname", "--hostname convocate-abcdef12"},
		{"session home", sessionDir + ":/home/convocate"},
		{"docker socket", config.DockerSocket + ":" + config.DockerSocket},
		{"SSH mount", sshDir + ":/home/convocate/.ssh:ro"},
		{"gitconfig mount", gitConfig + ":/home/convocate/.gitconfig:ro"},
		{"claude binary", config.ClaudeBinaryPath + ":" + config.ClaudeBinaryPath + ":ro"},
		{"CONVOCATE_UID", "CONVOCATE_UID=1337"},
		{"CONVOCATE_GID", "CONVOCATE_GID=1337"},
		{"image", config.ContainerImage()},
		{"claude-shared", config.ClaudeSharedDir + ":ro"},
	}

	for _, c := range checks {
		if !strings.Contains(argStr, c.pattern) {
			t.Errorf("missing %s in args: %s", c.name, argStr)
		}
	}
}

func TestBuildRunArgs_NoSSH(t *testing.T) {
	paths := config.Paths{
		ConvocateHome:   "/nonexistent",
		SessionsBase:    "/nonexistent",
		ConvocateConfig: "/nonexistent/.claude",
		SSHDir:          "/nonexistent/.ssh",
		GitConfig:       "/nonexistent/.gitconfig",
	}

	r := NewRunner("test-uuid", "/tmp/session", testUserInfo(), paths)
	args := r.buildRunArgs("test-container")
	argStr := strings.Join(args, " ")

	if strings.Contains(argStr, ".ssh:ro") {
		t.Error("SSH should not be mounted when dir doesn't exist")
	}
	if strings.Contains(argStr, ".gitconfig:ro") {
		t.Error("gitconfig should not be mounted when file doesn't exist")
	}
}

func TestBuildRunArgs_DifferentUIDs(t *testing.T) {
	info := user.Info{UID: 5000, GID: 5000, Username: "testuser", HomeDir: "/home/testuser"}
	r := NewRunner("test-uuid", "/tmp/session", info, testPaths())
	args := r.buildRunArgs("test-container")
	argStr := strings.Join(args, " ")

	if !strings.Contains(argStr, "CONVOCATE_UID=5000") {
		t.Error("missing CONVOCATE_UID=5000")
	}
	if !strings.Contains(argStr, "CONVOCATE_GID=5000") {
		t.Error("missing CONVOCATE_GID=5000")
	}
}

func TestIsRunning_NotRunning(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	}

	r := NewRunnerWithExec("test-uuid", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	running, err := r.IsRunning()
	if err != nil {
		t.Fatalf("IsRunning failed: %v", err)
	}
	if running {
		t.Error("expected not running")
	}
}

func TestIsRunning_Running(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("echo", "true")
	}

	r := NewRunnerWithExec("test-uuid", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	running, err := r.IsRunning()
	if err != nil {
		t.Fatalf("IsRunning failed: %v", err)
	}
	if !running {
		t.Error("expected running")
	}
}

func TestIsRunning_FalseOutput(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("echo", "false")
	}

	r := NewRunnerWithExec("test-uuid", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	running, err := r.IsRunning()
	if err != nil {
		t.Fatalf("IsRunning failed: %v", err)
	}
	if running {
		t.Error("expected not running when output is 'false'")
	}
}

func TestImageExists_NotExists(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	}

	exists, err := ImageExists(mockExec)
	if err != nil {
		t.Fatalf("ImageExists failed: %v", err)
	}
	if exists {
		t.Error("expected image not to exist")
	}
}

func TestImageExists_Exists(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("true")
	}

	exists, err := ImageExists(mockExec)
	if err != nil {
		t.Fatalf("ImageExists failed: %v", err)
	}
	if !exists {
		t.Error("expected image to exist")
	}
}

func TestImageExists_NilExec(t *testing.T) {
	_, err := ImageExists(nil)
	if err != nil {
		t.Fatalf("ImageExists with nil exec should not error: %v", err)
	}
}

func TestStop_Success(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("true")
	}

	r := NewRunnerWithExec("test-uuid", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	err := r.Stop()
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
}

func TestStop_Failure(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	}

	r := NewRunnerWithExec("test-uuid", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	err := r.Stop()
	if err == nil {
		t.Error("expected error from failed stop")
	}
}

func TestDefaultExecFunc(t *testing.T) {
	cmd := DefaultExecFunc("echo", "test")
	if cmd == nil {
		t.Fatal("DefaultExecFunc returned nil")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "test" {
		t.Errorf("output = %q, want %q", string(out), "test")
	}
}

func TestStart_DockerRunFailure(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	}

	r := NewRunnerWithExec("test-uuid-1234567890", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	err := r.Start()
	if err == nil {
		t.Error("expected error from failed docker run")
	}
	if !strings.Contains(err.Error(), "failed to start container") {
		t.Errorf("expected 'failed to start container' error, got: %v", err)
	}
}

func TestStart_AttachTmuxArgs(t *testing.T) {
	var capturedCalls [][]string
	callCount := 0
	mockExec := func(name string, args ...string) *exec.Cmd {
		capturedCalls = append(capturedCalls, append([]string{name}, args...))
		callCount++
		if callCount == 1 {
			// docker run --detach succeeds
			return exec.Command("echo", "container-id")
		}
		// docker exec (attachTmux) - use "true" to succeed
		return exec.Command("true")
	}

	r := NewRunnerWithExec("test-uuid-1234567890", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	err := r.Start()
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if len(capturedCalls) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(capturedCalls))
	}

	// Verify first call is docker run --detach
	runArgs := capturedCalls[0]
	runStr := strings.Join(runArgs, " ")
	if !strings.Contains(runStr, "--detach") {
		t.Errorf("docker run should include --detach, got: %s", runStr)
	}
	if strings.Contains(runStr, "--interactive") {
		t.Errorf("docker run should not include --interactive, got: %s", runStr)
	}

	// Verify second call is docker exec with tmux attach
	execArgs := capturedCalls[1]
	execStr := strings.Join(execArgs, " ")
	if !strings.Contains(execStr, "exec") {
		t.Errorf("second call should be docker exec, got: %s", execStr)
	}
	if !strings.Contains(execStr, "tmux") {
		t.Errorf("exec should include tmux, got: %s", execStr)
	}
	if !strings.Contains(execStr, "attach-session") {
		t.Errorf("exec should include attach-session, got: %s", execStr)
	}
	if !strings.Contains(execStr, "-t convocate") {
		t.Errorf("exec should target tmux session 'convocate', got: %s", execStr)
	}
}

func TestAttach_UsesTmux(t *testing.T) {
	var capturedArgs []string
	mockExec := func(name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string{name}, args...)
		return exec.Command("true")
	}

	r := NewRunnerWithExec("test-uuid-1234567890", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	err := r.Attach()
	if err != nil {
		t.Fatalf("Attach failed: %v", err)
	}

	argStr := strings.Join(capturedArgs, " ")
	if !strings.Contains(argStr, "docker exec -it") {
		t.Errorf("Attach should use 'docker exec -it', got: %s", argStr)
	}
	if !strings.Contains(argStr, "tmux attach-session -t convocate") {
		t.Errorf("Attach should use 'tmux attach-session -t convocate', got: %s", argStr)
	}
}

func TestAttach_Failure(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	}

	r := NewRunnerWithExec("test-uuid-1234567890", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	err := r.Attach()
	if err == nil {
		t.Error("expected error from failed attach")
	}
}

func TestBuildRunArgs_PortPublished(t *testing.T) {
	r := NewRunner("abcdef12-3456-7890-abcd-ef1234567890", "/tmp/session", testUserInfo(), testPaths())
	r.SetPort(8080)
	args := r.buildRunArgs("test-container")
	argStr := strings.Join(args, " ")

	// Default protocol when unset is tcp.
	if !strings.Contains(argStr, "-p 8080:8080/tcp") {
		t.Errorf("expected '-p 8080:8080/tcp' in args, got: %s", argStr)
	}
}

func TestBuildRunArgs_UDPProtocol(t *testing.T) {
	r := NewRunner("abcdef12-3456-7890-abcd-ef1234567890", "/tmp/session", testUserInfo(), testPaths())
	r.SetPort(53)
	r.SetProtocol("udp")
	args := r.buildRunArgs("test-container")
	argStr := strings.Join(args, " ")

	if !strings.Contains(argStr, "-p 53:53/udp") {
		t.Errorf("expected '-p 53:53/udp' in args, got: %s", argStr)
	}
}

func TestBuildRunArgs_TCPProtocolExplicit(t *testing.T) {
	r := NewRunner("abcdef12-3456-7890-abcd-ef1234567890", "/tmp/session", testUserInfo(), testPaths())
	r.SetPort(8080)
	r.SetProtocol("tcp")
	args := r.buildRunArgs("test-container")
	argStr := strings.Join(args, " ")

	if !strings.Contains(argStr, "-p 8080:8080/tcp") {
		t.Errorf("expected '-p 8080:8080/tcp' in args, got: %s", argStr)
	}
}

func TestBuildRunArgs_NoPortByDefault(t *testing.T) {
	r := NewRunner("abcdef12-3456-7890-abcd-ef1234567890", "/tmp/session", testUserInfo(), testPaths())
	args := r.buildRunArgs("test-container")
	argStr := strings.Join(args, " ")

	if strings.Contains(argStr, " -p ") {
		t.Errorf("expected no '-p' flag when port not set, got: %s", argStr)
	}
}

func TestBuildRunArgs_DNSServerFlag(t *testing.T) {
	r := NewRunner("abcdef12-3456-7890-abcd-ef1234567890", "/tmp/session", testUserInfo(), testPaths())
	r.SetDNSServer("192.168.3.90")
	args := r.buildRunArgs("test-container")
	argStr := strings.Join(args, " ")
	if !strings.Contains(argStr, "--dns 192.168.3.90") {
		t.Errorf("expected '--dns 192.168.3.90' in args, got: %s", argStr)
	}
}

func TestBuildRunArgs_NoDNSFlagByDefault(t *testing.T) {
	r := NewRunner("abcdef12-3456-7890-abcd-ef1234567890", "/tmp/session", testUserInfo(), testPaths())
	args := r.buildRunArgs("test-container")
	argStr := strings.Join(args, " ")
	if strings.Contains(argStr, "--dns") {
		t.Errorf("expected no --dns flag without SetDNSServer, got: %s", argStr)
	}
}

func TestBuildRunArgs_NoInteractiveTty(t *testing.T) {
	r := NewRunner("abcdef12-3456-7890-abcd-ef1234567890", "/tmp/session", testUserInfo(), testPaths())
	args := r.buildRunArgs("test-container")
	argStr := strings.Join(args, " ")

	if strings.Contains(argStr, "--interactive") {
		t.Error("buildRunArgs should not include --interactive")
	}
	if strings.Contains(argStr, "--tty") {
		t.Error("buildRunArgs should not include --tty")
	}
	if !strings.Contains(argStr, "--detach") {
		t.Error("buildRunArgs should include --detach")
	}
}

// --- Package-level helper tests ---

func withPkgExec(t *testing.T, fn ExecFunc) {
	t.Helper()
	orig := pkgExecFn
	pkgExecFn = fn
	t.Cleanup(func() { pkgExecFn = orig })
}

func TestIsContainerRunning_True(t *testing.T) {
	withPkgExec(t, func(name string, args ...string) *exec.Cmd {
		return exec.Command("echo", "true")
	})
	if !IsContainerRunning("x") {
		t.Error("expected IsContainerRunning=true when docker reports 'true'")
	}
}

func TestIsContainerRunning_False(t *testing.T) {
	withPkgExec(t, func(name string, args ...string) *exec.Cmd {
		return exec.Command("echo", "false")
	})
	if IsContainerRunning("x") {
		t.Error("expected IsContainerRunning=false when docker reports 'false'")
	}
}

func TestIsContainerRunning_CommandError(t *testing.T) {
	withPkgExec(t, func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	})
	if IsContainerRunning("x") {
		t.Error("expected IsContainerRunning=false when command errors")
	}
}

func TestStopContainer_Success(t *testing.T) {
	withPkgExec(t, func(name string, args ...string) *exec.Cmd {
		return exec.Command("true")
	})
	if err := StopContainer("x"); err != nil {
		t.Errorf("StopContainer returned error: %v", err)
	}
}

func TestStopContainer_Error(t *testing.T) {
	withPkgExec(t, func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	})
	if err := StopContainer("x"); err == nil {
		t.Error("expected StopContainer error")
	}
}

func TestDetachClients_Success(t *testing.T) {
	withPkgExec(t, func(name string, args ...string) *exec.Cmd {
		return exec.Command("true")
	})
	if err := DetachClients("x"); err != nil {
		t.Errorf("DetachClients returned error: %v", err)
	}
}

func TestDetachClients_Error(t *testing.T) {
	withPkgExec(t, func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	})
	err := DetachClients("x")
	if err == nil {
		t.Error("expected DetachClients error")
	}
	if !strings.Contains(err.Error(), "failed to detach") {
		t.Errorf("error = %q, want 'failed to detach'", err.Error())
	}
}

// --- Runner.StartDetached (background start) ---

func TestRunner_StartDetached_Success(t *testing.T) {
	var calls [][]string
	mockExec := func(name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return exec.Command("true")
	}
	r := NewRunnerWithExec("uuid-1234", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	if err := r.StartDetached(); err != nil {
		t.Fatalf("StartDetached failed: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 docker call (run only), got %d: %v", len(calls), calls)
	}
	joined := strings.Join(calls[0], " ")
	if !strings.Contains(joined, "--detach") {
		t.Errorf("expected --detach in args: %s", joined)
	}
	if strings.Contains(joined, "tmux attach") {
		t.Errorf("StartDetached must not attach to tmux: %s", joined)
	}
}

func TestRunner_StartDetached_Failure(t *testing.T) {
	mockExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	}
	r := NewRunnerWithExec("uuid-1234", "/tmp/session", testUserInfo(), testPaths(), mockExec)
	if err := r.StartDetached(); err == nil {
		t.Error("expected StartDetached error when docker run fails")
	}
}

func TestRunner_SetImageOverridesDefault(t *testing.T) {
	r := NewRunner("12345678-aaaa-bbbb-cccc-ddddeeeefffff", "/dir", testUserInfo(), testPaths())
	if got := r.imageRef(); got == "" {
		t.Error("default image should not be empty")
	}
	r.SetImage("convocate:v2.1.0")
	if got := r.imageRef(); got != "convocate:v2.1.0" {
		t.Errorf("imageRef after SetImage = %q", got)
	}
}

func TestRunner_BuildRunArgs_UsesSetImage(t *testing.T) {
	r := NewRunner("12345678-aaaa-bbbb-cccc-ddddeeeefffff", "/dir", testUserInfo(), testPaths())
	r.SetImage("convocate:v2.3.4")
	args := r.buildRunArgs("convocate-session-test")
	last := args[len(args)-1]
	if last != "convocate:v2.3.4" {
		t.Errorf("last arg = %q, want the configured image tag", last)
	}
}

func TestRunner_BuildRunArgs_EmitsCgroupParent(t *testing.T) {
	r := NewRunner("12345678-aaaa-bbbb-cccc-ddddeeeefffff", "/dir", testUserInfo(), testPaths())
	args := r.buildRunArgs("convocate-session-test")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cgroup-parent "+DefaultCgroupParent) {
		t.Errorf("default cgroup-parent missing from args: %v", args)
	}
}

func TestRunner_SetCgroupParentOverrides(t *testing.T) {
	r := NewRunner("12345678-aaaa-bbbb-cccc-ddddeeeefffff", "/dir", testUserInfo(), testPaths())
	r.SetCgroupParent("custom.slice")
	args := r.buildRunArgs("convocate-session-test")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cgroup-parent custom.slice") {
		t.Errorf("custom cgroup-parent missing from args: %v", args)
	}
	if strings.Contains(joined, "--cgroup-parent "+DefaultCgroupParent) {
		t.Error("default slice should have been overridden")
	}
}
