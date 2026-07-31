package hostinstall

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/asymmetric-effort/convocate/internal/tlsutil"
)

// seedRsyslogCA stages a CA cert + key at <etcDir>/rsyslog-ca/ so
// init-agent's client-config step can read it. Returns nothing — the
// files land in place.
func seedRsyslogCA(t *testing.T, etcDir string) {
	t.Helper()
	ca, err := tlsutil.GenerateCA("test-ca", 2)
	if err != nil {
		t.Fatal(err)
	}
	caDir := filepath.Join(etcDir, "rsyslog-ca")
	if err := os.MkdirAll(caDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caDir, "ca.crt"), ca.CertPEM, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caDir, "ca.key"), ca.KeyPEM, 0600); err != nil {
		t.Fatal(err)
	}
}

// testInitAgentOpts returns options pointed at a temp binary + temp shell
// etc dir, ready to pass to InitAgent for unit tests.
func testInitAgentOpts(t *testing.T) (InitAgentOptions, string) {
	t.Helper()
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "convocate-agent")
	if err := os.WriteFile(bin, []byte("#!fake-agent-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	etcDir := t.TempDir()
	// Provide a stub `docker` on PATH so TransferImage (now part of
	// init-agent) can run `docker save` without a real daemon. Each
	// test gets its own PATH so parallel execution is safe.
	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "docker")
	script := "#!/bin/sh\ncase \"$1\" in\n  save) printf 'fake-image-bytes\\n' ;;\n  *) exit 1 ;;\nesac\n"
	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)
	return InitAgentOptions{
		BinaryPath:       bin,
		ShellHost:        "shell.example.com",
		LocalShellEtcDir: etcDir,
		ImageTag:         "convocate:v9.9.9",
	}, etcDir
}

func newAgentMockRunner(agentID string) *mockRunner {
	return &mockRunner{
		cmdStdout: map[string]string{
			// init-agent reads the agent-id via `cat`.
			"cat /etc/convocate-agent/agent-id": agentID + "\n",
		},
	}
}

func TestInitAgent_RequiresShellHost(t *testing.T) {
	opts, _ := testInitAgentOpts(t)
	opts.ShellHost = ""
	m := newAgentMockRunner("whatever")
	err := InitAgent(context.Background(), m, nil, opts, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--shell-host") {
		t.Errorf("expected shell-host-required error, got %v", err)
	}
}

func TestInitAgent_MissingBinary(t *testing.T) {
	opts := InitAgentOptions{
		BinaryPath: "/does/not/exist/convocate-agent",
		ShellHost:  "shell.example.com",
		ImageTag:   "convocate:v0",
	}
	m := newAgentMockRunner("x")
	err := InitAgent(context.Background(), m, nil, opts, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "locate convocate-agent") {
		t.Errorf("expected locate error, got %v", err)
	}
}

func TestInitAgent_RequiresImageTag(t *testing.T) {
	opts, _ := testInitAgentOpts(t)
	opts.ImageTag = ""
	m := newAgentMockRunner("whatever")
	err := InitAgent(context.Background(), m, nil, opts, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--image-tag") {
		t.Errorf("expected image-tag-required error, got %v", err)
	}
}

func TestInitAgent_EndToEnd(t *testing.T) {
	opts, etcDir := testInitAgentOpts(t)
	seedRsyslogCA(t, etcDir)
	m := newAgentMockRunner("abcdef012345")
	var log bytes.Buffer
	if err := InitAgent(context.Background(), m, nil, opts, &log); err != nil {
		t.Fatalf("InitAgent failed: %v\nlog:\n%s", err, log.String())
	}

	// Binary upload + three peering + four rsyslog + one image tarball
	// + one current-image pointer = 10 copies.
	if len(m.copies) != 10 {
		t.Fatalf("expected 10 copies, got %d: %+v", len(m.copies), copyDsts(m.copies))
	}
	wantDests := map[string]os.FileMode{
		"/usr/local/bin/convocate-agent":                  0755,
		"/home/convocate/.ssh/authorized_keys":            0600,
		"/etc/convocate-agent/agent_to_shell_ed25519_key": 0600,
		"/etc/convocate-agent/shell-host":                 0644,
		"/etc/convocate-agent/rsyslog-tls/ca.crt":         0644,
		"/etc/convocate-agent/rsyslog-tls/client.crt":     0644,
		"/etc/convocate-agent/rsyslog-tls/client.key":     0600,
		"/etc/rsyslog.d/10-convocate-client.conf":         0644,
		"/etc/convocate-agent/current-image":              0644,
	}
	for _, c := range m.copies {
		// The image tarball lands at /tmp/convocate-image-<digest>.tar.gz;
		// digest varies per run so match by prefix instead of exact.
		if strings.HasPrefix(c.Dst, "/tmp/convocate-image-") {
			if c.Mode != 0600 {
				t.Errorf("image tarball mode = %o, want 0600", c.Mode)
			}
			continue
		}
		mode, ok := wantDests[c.Dst]
		if !ok {
			t.Errorf("unexpected copy dest %q", c.Dst)
			continue
		}
		if c.Mode != mode {
			t.Errorf("%s: mode = %o, want %o", c.Dst, c.Mode, mode)
		}
	}

	// current-image must carry the tag we asked init-agent to push.
	ci := findCopy(m.copies, "/etc/convocate-agent/current-image")
	if ci == nil {
		t.Fatal("current-image pointer not written")
	}
	if strings.TrimSpace(string(ci.Content)) != "convocate:v9.9.9" {
		t.Errorf("current-image content = %q", ci.Content)
	}

	// The rsyslog client config must embed the agent-id + shell-host.
	cfg := findCopy(m.copies, "/etc/rsyslog.d/10-convocate-client.conf")
	if cfg == nil {
		t.Fatal("client rsyslog config not uploaded")
	}
	body := string(cfg.Content)
	if !strings.Contains(body, "$LocalHostName abcdef012345") {
		t.Errorf("client config missing agent-id stamp:\n%s", body)
	}
	if !strings.Contains(body, `target="shell.example.com"`) {
		t.Errorf("client config missing shell-host target:\n%s", body)
	}

	// Verify the shell-host file carries the trimmed value.
	shellHostCopy := findCopy(m.copies, "/etc/convocate-agent/shell-host")
	if shellHostCopy == nil || strings.TrimSpace(string(shellHostCopy.Content)) != "shell.example.com" {
		t.Errorf("shell-host copy content = %q", shellHostCopy.Content)
	}

	// The private key pushed to the agent must parse as an SSH key and must
	// be tagged with the agent-id.
	privCopy := findCopy(m.copies, "/etc/convocate-agent/agent_to_shell_ed25519_key")
	if privCopy == nil {
		t.Fatal("agent->shell private key not copied")
	}
	if _, err := ssh.ParsePrivateKey(privCopy.Content); err != nil {
		t.Errorf("agent->shell key doesn't parse: %v", err)
	}

	// authorized_keys on the agent must contain a valid public key line
	// tagged with the matching comment.
	authCopy := findCopy(m.copies, "/home/convocate/.ssh/authorized_keys")
	if authCopy == nil {
		t.Fatal("authorized_keys not copied")
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey(authCopy.Content)
	if err != nil {
		t.Fatalf("auth key parse: %v", err)
	}
	if pub == nil {
		t.Error("nil pub key")
	}
	if !strings.Contains(comment, "abcdef012345") {
		t.Errorf("comment %q missing agent id", comment)
	}

	// First six commands are the fixed peering sequence; everything after
	// is the rsyslog step which we check for key phrases rather than
	// exact ordering (apt-get, mkdir, chowns, restart rsyslog).
	wantPeering := []string{
		"/usr/local/bin/convocate-agent install",
		"cat /etc/convocate-agent/agent-id",
		"chown convocate:convocate '/home/convocate/.ssh/authorized_keys'",
		"chown convocate:convocate '/etc/convocate-agent/agent_to_shell_ed25519_key'",
		"chown root:root '/etc/convocate-agent/shell-host'",
		"systemctl restart convocate-agent.service",
	}
	for i, want := range wantPeering {
		if !strings.Contains(m.cmds[i].Cmd, want) {
			t.Errorf("cmd[%d] = %q, want substring %q", i, firstLine(m.cmds[i].Cmd), want)
		}
	}
	rsyslogPhrases := []string{
		"mkdir -p /etc/convocate-agent/rsyslog-tls",
		"rsyslog-gnutls",
		"/var/spool/rsyslog",
		"systemctl restart rsyslog",
	}
	joined := allCmds(m.cmds)
	for _, want := range rsyslogPhrases {
		if !strings.Contains(joined, want) {
			t.Errorf("missing rsyslog phrase %q", want)
		}
	}
	for i, c := range m.cmds {
		if !c.Opts.Sudo {
			t.Errorf("cmd[%d] should be sudo", i)
		}
	}

	// --- shell-side local files -------------------------------------------
	// status_authorized_keys should have been created with exactly one
	// public-key line (the agent->shell pubkey we generated).
	authPath := filepath.Join(etcDir, "status_authorized_keys")
	authData, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read %s: %v", authPath, err)
	}
	sshPub, authComment, _, _, err := ssh.ParseAuthorizedKey(authData)
	if err != nil {
		t.Fatalf("parse local authorized_keys: %v", err)
	}
	if sshPub == nil {
		t.Error("nil authorized key")
	}
	if !strings.Contains(authComment, "abcdef012345") {
		t.Errorf("comment = %q, expected agent id", authComment)
	}

	// shell->agent private key was staged under agent-keys/<id>/.
	keyPath := filepath.Join(etcDir, "agent-keys", "abcdef012345", "shell_to_agent_ed25519_key")
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read %s: %v", keyPath, err)
	}
	if _, err := ssh.ParsePrivateKey(keyData); err != nil {
		t.Errorf("shell->agent private key doesn't parse: %v", err)
	}
	// Mode must be 0600 — this key grants full CRUD of the agent's
	// containers and should never be world-readable.
	info, _ := os.Stat(keyPath)
	if info.Mode().Perm() != 0600 {
		t.Errorf("shell->agent key mode = %o, want 0600", info.Mode().Perm())
	}

	// agent-host file records where to dial.
	hostPath := filepath.Join(etcDir, "agent-keys", "abcdef012345", "agent-host")
	hostData, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read %s: %v", hostPath, err)
	}
	if strings.TrimSpace(string(hostData)) != "mock" {
		t.Errorf("agent-host = %q, want 'mock' (mockRunner.Target())", hostData)
	}
}

func TestInitAgent_AppendsToExistingAuthorizedKeys(t *testing.T) {
	// Prepopulate status_authorized_keys with one line, then run init-agent
	// and verify the file grows rather than being clobbered.
	opts, etcDir := testInitAgentOpts(t)
	seedRsyslogCA(t, etcDir)
	authPath := filepath.Join(etcDir, "status_authorized_keys")
	existing := []byte("# existing\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGzZxHIT0xU4WIaMDIbj5DD/exxxxxxxxxxxxxxxxxxx existing\n")
	if err := os.WriteFile(authPath, existing, 0644); err != nil {
		t.Fatal(err)
	}
	m := newAgentMockRunner("zzz")
	if err := InitAgent(context.Background(), m, nil, opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, existing) {
		t.Error("pre-existing content was clobbered")
	}
	if !bytes.Contains(after, []byte("agent=zzz")) {
		t.Error("new key entry missing")
	}
}

func TestInitAgent_EmptyAgentIDIsError(t *testing.T) {
	opts, _ := testInitAgentOpts(t)
	m := newAgentMockRunner("") // cat returns empty
	err := InitAgent(context.Background(), m, nil, opts, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "agent-id") {
		t.Errorf("expected agent-id error, got %v", err)
	}
}

// --- small helpers ---------------------------------------------------------

func findCopy(copies []mockCopy, dst string) *mockCopy {
	for i := range copies {
		if copies[i].Dst == dst {
			return &copies[i]
		}
	}
	return nil
}

func copyDsts(copies []mockCopy) []string {
	out := make([]string, len(copies))
	for i, c := range copies {
		out[i] = c.Dst
	}
	return out
}
