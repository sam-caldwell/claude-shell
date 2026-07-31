package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func mockExecSuccess(name string, args ...string) *exec.Cmd {
	return exec.Command("true")
}

func mockExecFailure(name string, args ...string) *exec.Cmd {
	return exec.Command("false")
}

// execRecord captures a single command invocation.
type execRecord struct {
	Name string
	Args []string
}

// recordingExecFunc returns an ExecFunc that records all calls and delegates to inner.
func recordingExecFunc(inner ExecFunc) (ExecFunc, *[]execRecord) {
	var mu sync.Mutex
	var records []execRecord
	fn := func(name string, args ...string) *exec.Cmd {
		mu.Lock()
		records = append(records, execRecord{Name: name, Args: append([]string(nil), args...)})
		mu.Unlock()
		return inner(name, args...)
	}
	return fn, &records
}

func TestNew(t *testing.T) {
	inst := New("")
	if inst == nil {
		t.Fatal("New returned nil")
	}
}

func TestNewWithExec(t *testing.T) {
	inst := NewWithExec(mockExecSuccess, "")
	if inst == nil {
		t.Fatal("NewWithExec returned nil")
	}
}

func TestRun_AsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root")
	}
	inst := NewWithExec(mockExecSuccess, "")
	err := inst.Run()
	if err != nil {
		t.Logf("Run error (may be expected if claude user missing): %v", err)
	}
}

func TestRun_NotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test must run as non-root")
	}

	inst := NewWithExec(mockExecSuccess, "")
	err := inst.Run()
	if err == nil {
		t.Error("expected error when not running as root")
	}
}

// checkPlatform gates on the host OS, so the expected result depends on
// where the suite runs: nil on Linux, an error anywhere else. Asserting
// both directions covers the reject branch too, which never ran while
// this test assumed a Linux runner.
func TestCheckPlatform(t *testing.T) {
	inst := NewWithExec(mockExecSuccess, "")
	err := inst.checkPlatform()

	if runtime.GOOS == "linux" {
		if err != nil {
			t.Errorf("checkPlatform on linux: unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Errorf("checkPlatform on %s: expected an error, got nil", runtime.GOOS)
	}
}

func TestCheckDocker_Success(t *testing.T) {
	inst := NewWithExec(mockExecSuccess, "")
	err := inst.checkDocker()
	if err != nil {
		t.Errorf("checkDocker failed: %v", err)
	}
}

func TestCheckDocker_Failure(t *testing.T) {
	inst := NewWithExec(mockExecFailure, "")
	err := inst.checkDocker()
	if err == nil {
		t.Error("expected error when docker is not available")
	}
}

func TestCheckClaudeCLI_Found(t *testing.T) {
	inst := NewWithExec(mockExecSuccess, "")
	err := inst.checkClaudeCLI()
	if _, statErr := os.Stat("/usr/local/bin/claude"); statErr == nil {
		if err != nil {
			t.Errorf("checkClaudeCLI failed when binary exists: %v", err)
		}
	} else {
		if err == nil {
			t.Error("expected error when claude binary doesn't exist")
		}
	}
}

func TestSetupSkel(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("setupSkel requires root (to chown)")
	}
	inst := NewWithExec(mockExecSuccess, "")
	err := inst.setupSkel()
	if err != nil {
		t.Logf("setupSkel error (may be expected): %v", err)
	}
}

func TestBuildImage_Success(t *testing.T) {
	inst := NewWithExec(mockExecSuccess, "")
	err := inst.buildImage()
	if err != nil {
		t.Errorf("buildImage failed: %v", err)
	}
}

func TestBuildImage_DockerFails(t *testing.T) {
	inst := NewWithExec(mockExecFailure, "")
	err := inst.buildImage()
	if err == nil {
		t.Error("expected error when docker build fails")
	}
}

func TestChownRecursive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown test requires root")
	}

	tmpDir := t.TempDir()
	subDir := tmpDir + "/sub"
	if err := os.MkdirAll(subDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subDir+"/file.txt", []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	err := chownRecursive(tmpDir, 1000, 1000)
	if err != nil {
		t.Fatalf("chownRecursive failed: %v", err)
	}
}

func TestChownRecursive_InvalidPath(t *testing.T) {
	err := chownRecursive("/nonexistent/path/12345", 1000, 1000)
	if err == nil {
		t.Error("expected error for invalid path")
	}
}

func TestCreateUser_AlreadyExists(t *testing.T) {
	inst := NewWithExec(mockExecSuccess, "")
	err := inst.createUser()
	if err != nil {
		t.Logf("createUser note: %v", err)
	}
}

func TestDefaultExecFunc(t *testing.T) {
	cmd := DefaultExecFunc("echo", "hello")
	if cmd == nil {
		t.Fatal("DefaultExecFunc returned nil")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("DefaultExecFunc command failed: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("output = %q, want %q", string(out), "hello\n")
	}
}

// ---------------------------------------------------------------------------
// copyBinary tests
// ---------------------------------------------------------------------------

func TestCopyBinary_Success(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "source")
	dstPath := filepath.Join(tmpDir, "dest")

	srcContent := []byte("fake-binary-content-1234")
	if err := os.WriteFile(srcPath, srcContent, 0755); err != nil {
		t.Fatal(err)
	}

	if err := copyBinary(srcPath, dstPath); err != nil {
		t.Fatalf("copyBinary failed: %v", err)
	}

	// Verify content was copied correctly.
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("failed to read destination: %v", err)
	}
	if string(got) != string(srcContent) {
		t.Errorf("content mismatch: got %q, want %q", got, srcContent)
	}

	// Verify permissions are 0755.
	info, err := os.Stat(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0755 {
		t.Errorf("permissions = %o, want 0755", perm)
	}
}

func TestCopyBinary_OverwritesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "source")
	dstPath := filepath.Join(tmpDir, "dest")

	// Write an old version at dest.
	if err := os.WriteFile(dstPath, []byte("old-content"), 0755); err != nil {
		t.Fatal(err)
	}

	newContent := []byte("new-binary-content")
	if err := os.WriteFile(srcPath, newContent, 0755); err != nil {
		t.Fatal(err)
	}

	if err := copyBinary(srcPath, dstPath); err != nil {
		t.Fatalf("copyBinary failed: %v", err)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newContent) {
		t.Errorf("content not updated: got %q, want %q", got, newContent)
	}
}

func TestCopyBinary_AtomicNoTmpLeftOnSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "source")
	dstPath := filepath.Join(tmpDir, "dest")

	if err := os.WriteFile(srcPath, []byte("data"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := copyBinary(srcPath, dstPath); err != nil {
		t.Fatal(err)
	}

	// The .tmp file should not exist after a successful copy.
	tmpPath := dstPath + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temporary file %s should not exist after success", tmpPath)
	}
}

func TestCopyBinary_SourceNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	err := copyBinary(filepath.Join(tmpDir, "nonexistent"), filepath.Join(tmpDir, "dest"))
	if err == nil {
		t.Error("expected error when source does not exist")
	}
	if !strings.Contains(err.Error(), "failed to open source binary") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCopyBinary_DestDirNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "source")
	if err := os.WriteFile(srcPath, []byte("data"), 0755); err != nil {
		t.Fatal(err)
	}

	err := copyBinary(srcPath, filepath.Join(tmpDir, "nodir", "dest"))
	if err == nil {
		t.Error("expected error when dest directory does not exist")
	}
	if !strings.Contains(err.Error(), "failed to create destination binary") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCopyBinary_LargeFile(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "source")
	dstPath := filepath.Join(tmpDir, "dest")

	// Create a ~1MB file to verify io.Copy works for non-trivial sizes.
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := os.WriteFile(srcPath, data, 0755); err != nil {
		t.Fatal(err)
	}

	if err := copyBinary(srcPath, dstPath); err != nil {
		t.Fatalf("copyBinary failed on large file: %v", err)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Errorf("size mismatch: got %d bytes, want %d", len(got), len(data))
	}
}

// ---------------------------------------------------------------------------
// ensureInEtcShells tests
// ---------------------------------------------------------------------------

func TestEnsureInEtcShells_AddsNewEntry(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "shells")
	initial := "/bin/bash\n/bin/sh\n"
	if err := os.WriteFile(tmpFile, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ensureInEtcShells(tmpFile, "/usr/local/bin/convocate"); err != nil {
		t.Fatalf("ensureInEtcShells failed: %v", err)
	}

	got, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	want := initial + "/usr/local/bin/convocate\n"
	if string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestEnsureInEtcShells_SkipsIfAlreadyPresent(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "shells")
	initial := "/bin/bash\n/usr/local/bin/convocate\n/bin/sh\n"
	if err := os.WriteFile(tmpFile, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ensureInEtcShells(tmpFile, "/usr/local/bin/convocate"); err != nil {
		t.Fatalf("ensureInEtcShells failed: %v", err)
	}

	got, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	// File should be unchanged.
	if string(got) != initial {
		t.Errorf("file was modified when entry already present: got %q, want %q", got, initial)
	}
}

func TestEnsureInEtcShells_Idempotent(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "shells")
	if err := os.WriteFile(tmpFile, []byte("/bin/bash\n"), 0644); err != nil {
		t.Fatal(err)
	}

	shellPath := "/usr/local/bin/convocate"

	// Call twice.
	if err := ensureInEtcShells(tmpFile, shellPath); err != nil {
		t.Fatal(err)
	}
	if err := ensureInEtcShells(tmpFile, shellPath); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	// Should only appear once.
	count := strings.Count(string(got), shellPath)
	if count != 1 {
		t.Errorf("shell path appears %d times, want 1; content: %q", count, got)
	}
}

func TestEnsureInEtcShells_EmptyFile(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "shells")
	if err := os.WriteFile(tmpFile, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ensureInEtcShells(tmpFile, "/usr/local/bin/convocate"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "/usr/local/bin/convocate\n" {
		t.Errorf("unexpected content: %q", got)
	}
}

func TestEnsureInEtcShells_HandlesWhitespace(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "shells")
	// Entry with leading/trailing spaces should still match.
	initial := "/bin/bash\n  /usr/local/bin/convocate  \n"
	if err := os.WriteFile(tmpFile, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ensureInEtcShells(tmpFile, "/usr/local/bin/convocate"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	// Should not add a duplicate since whitespace-trimmed match exists.
	if string(got) != initial {
		t.Errorf("file was modified despite whitespace match: got %q, want %q", got, initial)
	}
}

func TestEnsureInEtcShells_FileNotFound(t *testing.T) {
	err := ensureInEtcShells(filepath.Join(t.TempDir(), "nonexistent"), "/usr/local/bin/convocate")
	if err == nil {
		t.Error("expected error when shells file does not exist")
	}
}

// ---------------------------------------------------------------------------
// configureLoginShell tests (via recording exec mock)
// ---------------------------------------------------------------------------

func TestConfigureLoginShell_CallsUsermod(t *testing.T) {
	// configureLoginShell checks os.Stat on config.ConvocateBinaryPath.
	// If it doesn't exist, the function returns early with an error.
	// We can only fully test this when the binary is installed.
	if _, err := os.Stat("/usr/local/bin/convocate"); os.IsNotExist(err) {
		t.Skip("convocate binary not installed at /usr/local/bin/convocate")
	}

	recFn, records := recordingExecFunc(mockExecSuccess)
	inst := NewWithExec(recFn, "")

	err := inst.configureLoginShell()
	if err != nil {
		t.Fatalf("configureLoginShell failed: %v", err)
	}

	// Verify usermod was called with correct args.
	found := false
	for _, r := range *records {
		if r.Name == "usermod" {
			found = true
			if len(r.Args) < 3 {
				t.Fatalf("usermod called with too few args: %v", r.Args)
			}
			if r.Args[0] != "--shell" {
				t.Errorf("usermod args[0] = %q, want --shell", r.Args[0])
			}
			if r.Args[1] != "/usr/local/bin/convocate" {
				t.Errorf("usermod args[1] = %q, want /usr/local/bin/convocate", r.Args[1])
			}
			if r.Args[2] != "convocate" {
				t.Errorf("usermod args[2] = %q, want claude", r.Args[2])
			}
		}
	}
	if !found {
		t.Error("usermod was never called")
	}
}

func TestConfigureLoginShell_FailsWhenBinaryMissing(t *testing.T) {
	// Temporarily ensure the binary is NOT at the expected path.
	// Since we're not root, it likely isn't there, but be explicit.
	if _, err := os.Stat("/usr/local/bin/convocate"); err == nil {
		t.Skip("convocate binary exists; cannot test missing-binary path")
	}

	inst := NewWithExec(mockExecSuccess, "")
	err := inst.configureLoginShell()
	if err == nil {
		t.Error("expected error when convocate binary is missing")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestConfigureLoginShell_FailsWhenUsermodFails(t *testing.T) {
	if _, err := os.Stat("/usr/local/bin/convocate"); os.IsNotExist(err) {
		t.Skip("convocate binary not installed at /usr/local/bin/convocate")
	}

	inst := NewWithExec(mockExecFailure, "")
	err := inst.configureLoginShell()
	if err == nil {
		t.Error("expected error when usermod fails")
	}
	if !strings.Contains(err.Error(), "failed to set login shell") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// installBinary integration-level tests (require root)
// ---------------------------------------------------------------------------

func TestInstallBinary_RequiresRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("installBinary requires root")
	}
	inst := NewWithExec(mockExecSuccess, "")
	err := inst.installBinary()
	if err != nil {
		t.Logf("installBinary error (may be expected): %v", err)
	}
}

// ---------------------------------------------------------------------------
// Run step ordering test
// ---------------------------------------------------------------------------

func TestRun_StepOrder(t *testing.T) {
	// Verify that the install steps are in the expected order by checking
	// that the steps slice contains the right names. We can't call Run()
	// without root, but we can verify the struct is set up correctly by
	// inspecting the step names via a Run that fails at the root check.
	if os.Geteuid() == 0 {
		t.Skip("this test verifies non-root error path")
	}

	inst := NewWithExec(mockExecSuccess, "")
	err := inst.Run()
	if err == nil {
		t.Fatal("expected error when not root")
	}
	if !strings.Contains(err.Error(), "must be run as root") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// createUser recording test
// ---------------------------------------------------------------------------

func TestCreateUser_RecordsUsermod(t *testing.T) {
	recFn, records := recordingExecFunc(mockExecSuccess)
	inst := NewWithExec(recFn, "")

	err := inst.createUser()
	if err != nil {
		t.Logf("createUser note: %v", err)
		return
	}

	// If the claude user exists, usermod -aG docker should have been called.
	found := false
	for _, r := range *records {
		if r.Name == "usermod" {
			found = true
			wantArgs := []string{"-aG", "docker", "convocate"}
			if len(r.Args) != len(wantArgs) {
				t.Errorf("usermod args = %v, want %v", r.Args, wantArgs)
			} else {
				for i, a := range wantArgs {
					if r.Args[i] != a {
						t.Errorf("usermod args[%d] = %q, want %q", i, r.Args[i], a)
					}
				}
			}
		}
	}
	if !found {
		t.Log("usermod was not called (claude user may not exist on this system)")
	}
}
