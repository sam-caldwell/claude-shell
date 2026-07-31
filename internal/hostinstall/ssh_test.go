package hostinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asymmetric-effort/convocate/internal/sshutil"
)

// generateEd25519PEM writes a fresh ed25519 OpenSSH private key to a temp
// file via sshutil.LoadOrCreateHostKey and returns the on-disk bytes.
func generateEd25519PEM(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	if _, err := sshutil.LoadOrCreateHostKey(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"a 'b' c", `'a '"'"'b'"'"' c'`},
		{"$var && rm -rf /", `'$var && rm -rf /'`},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCollectAuthMethods_AlwaysIncludesPasswordFallback(t *testing.T) {
	// With no agent + a fresh empty $HOME, the password fallback is
	// the only method registered. Confirms collectAuthMethods doesn't
	// silently drop the prompt when keys are absent.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("SSH_AUTH_SOCK", "")

	methods := collectAuthMethods(SSHConfig{Host: "h", User: "u"})
	if len(methods) != 1 {
		t.Errorf("expected 1 method (password only), got %d", len(methods))
	}
}

func TestCollectAuthMethods_PicksUpDefaultKey(t *testing.T) {
	dir := t.TempDir()
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Generate a real ed25519 key on disk so the parse path runs.
	pemBytes := generateEd25519PEM(t)
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519"), pemBytes, 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", dir)
	t.Setenv("SSH_AUTH_SOCK", "")

	methods := collectAuthMethods(SSHConfig{Host: "h", User: "u"})
	// Now: 1 PublicKey (id_ed25519) + 1 password fallback.
	if len(methods) != 2 {
		t.Errorf("expected 2 methods (key + password), got %d", len(methods))
	}
}

func TestCollectAuthMethods_SkipsBadKey(t *testing.T) {
	dir := t.TempDir()
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519"), []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("SSH_AUTH_SOCK", "")

	methods := collectAuthMethods(SSHConfig{Host: "h", User: "u"})
	// Bad key is skipped silently; only the password fallback remains.
	if len(methods) != 1 {
		t.Errorf("expected 1 method when only invalid key present, got %d", len(methods))
	}
}

func TestNewSSHRunner_RequiresHost(t *testing.T) {
	_, err := NewSSHRunner(SSHConfig{})
	if err == nil || !strings.Contains(err.Error(), "host required") {
		t.Errorf("expected host-required error, got %v", err)
	}
}

func TestNewSSHRunner_DefaultsApplied(t *testing.T) {
	// Don't actually dial — point at an unreachable host so dial fails
	// after defaults have been applied. We just want to confirm the
	// defaulting branches run.
	_, err := NewSSHRunner(SSHConfig{
		Host:    "127.0.0.1",
		Port:    0,  // default 22
		User:    "", // default $USER
		Timeout: 0,  // default 15s
	})
	if err == nil {
		t.Error("expected dial to fail against unreachable host")
	}
}

// SSHRunner_Close on a nil-client is a no-op.
func TestSSHRunner_Close_NilClientIsNoop(t *testing.T) {
	r := &SSHRunner{client: nil}
	if err := r.Close(); err != nil {
		t.Errorf("Close on nil-client should be no-op, got %v", err)
	}
	// Target reads from cfg, also safe on a zero-value runner.
	if got := r.Target(); !strings.Contains(got, "@") {
		t.Errorf("Target = %q, want user@host shape", got)
	}
}
