package hypervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/asymmetric-effort/convocate/internal/hostinstall"
)

// Runner is the abstraction the create-vm orchestrator drives. The
// real implementation wraps an SSH connection to the hypervisor; tests
// substitute a recording mock so the multi-step flow can be exercised
// without any networking.
type Runner interface {
	// Run executes cmd on the remote host. opts.Sudo wraps with
	// `sudo -n --` so the connecting user must already have NOPASSWD
	// sudo (matches the rest of the project). Stdout / Stderr are
	// streamed through.
	Run(ctx context.Context, cmd string, opts RunOptions) error

	// CopyFile uploads src (a local path) to dst on the remote host
	// with the given mode. Used for pushing the ISO + cloud-init
	// seed.
	CopyFile(ctx context.Context, src, dst string, mode os.FileMode) error

	// ReadFile pulls the contents of a remote file via `cat`. Capped
	// to "small" config files (e.g. /proc/cpuinfo, /etc/os-release) —
	// not appropriate for an ISO.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// Target returns the user@host string for log lines.
	Target() string

	// Close releases the underlying connection.
	Close() error
}

// RunOptions mirrors hostinstall.RunOptions so the wrapper can pass it
// through directly. Aliased here to keep callers from importing
// hostinstall just for one struct.
type RunOptions = hostinstall.RunOptions

// dialRunner constructs the production Runner against opts.Hypervisor /
// opts.Username. Overridable from tests so the orchestrator can be
// driven against a mock without touching a real SSH server.
var dialRunner = func(ctx context.Context, opts *CreateVMOptions) (Runner, error) {
	cfg := hostinstall.SSHConfig{
		Host: opts.Hypervisor,
		User: opts.Username,
	}
	if opts.PromptPassword != nil {
		cfg.PasswordPrompt = opts.PromptPassword
	}
	r, err := hostinstall.NewSSHRunner(cfg)
	if err != nil {
		return nil, fmt.Errorf("dial %s@%s: %w", opts.Username, opts.Hypervisor, err)
	}
	return &sshRunner{ssh: r}, nil
}

// sshRunner adapts hostinstall.Runner to the hypervisor.Runner shape
// (which adds ReadFile). Production code only — tests use a mock.
type sshRunner struct {
	ssh hostinstall.Runner
}

func (r *sshRunner) Run(ctx context.Context, cmd string, opts RunOptions) error {
	return r.ssh.Run(ctx, cmd, opts)
}

func (r *sshRunner) CopyFile(ctx context.Context, src, dst string, mode os.FileMode) error {
	return r.ssh.CopyFile(ctx, src, dst, mode)
}

func (r *sshRunner) ReadFile(ctx context.Context, path string) ([]byte, error) {
	var buf bytes.Buffer
	err := r.ssh.Run(ctx, "cat "+shellQuoteArg(path), RunOptions{Stdout: &buf})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return buf.Bytes(), nil
}

func (r *sshRunner) Target() string { return r.ssh.Target() }
func (r *sshRunner) Close() error   { return r.ssh.Close() }

// shellQuoteArg single-quotes s, escaping embedded quotes so it's safe
// inside a `bash -c '<cmd>'` envelope.
func shellQuoteArg(s string) string {
	return "'" + replaceAll(s, "'", `'"'"'`) + "'"
}

// replaceAll is a tiny strings.ReplaceAll inlined to avoid the import
// in this otherwise low-dep file.
func replaceAll(s, old, new string) string {
	if old == "" || s == "" {
		return s
	}
	out := make([]byte, 0, len(s))
	for {
		i := indexOf(s, old)
		if i < 0 {
			out = append(out, s...)
			return string(out)
		}
		out = append(out, s[:i]...)
		out = append(out, new...)
		s = s[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// --- key install + sshd hardening --------------------------------------------

// InstallOperatorKey appends the operator's local public key to the
// hypervisor's ~/.ssh/authorized_keys for the connecting user. After
// this, future SSH connections succeed without the password prompt.
//
// keyPath defaults to ~/.ssh/id_ed25519.pub when empty — falling back
// to id_rsa.pub if that's missing. Returns a clear error when no
// operator key is found rather than installing nothing silently.
func InstallOperatorKey(ctx context.Context, r Runner, keyPath string) error {
	pub, src, err := readOperatorPubKey(keyPath)
	if err != nil {
		return err
	}

	// Idempotent install: ensure the dir exists, write a temp file
	// with the pubkey, then `cat ... >> authorized_keys` only if the
	// fingerprint isn't already present. The deduplication is done
	// via grep -F -x so identical lines aren't appended twice.
	cmd := fmt.Sprintf(`set -e
mkdir -p ~/.ssh
chmod 700 ~/.ssh
touch ~/.ssh/authorized_keys
chmod 600 ~/.ssh/authorized_keys
if ! grep -F -x %s ~/.ssh/authorized_keys >/dev/null 2>&1; then
  printf '%%s\n' %s >> ~/.ssh/authorized_keys
fi
`, shellQuoteArg(string(pub)), shellQuoteArg(string(pub)))

	if err := r.Run(ctx, cmd, RunOptions{}); err != nil {
		return fmt.Errorf("install operator key (%s): %w", src, err)
	}
	return nil
}

// readOperatorPubKey resolves keyPath (or the default candidates) and
// returns the trimmed pubkey bytes plus the source path. Errors when
// no candidate exists.
var readOperatorPubKey = func(keyPath string) ([]byte, string, error) {
	if keyPath != "" {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, "", fmt.Errorf("read pubkey %s: %w", keyPath, err)
		}
		return trimPubKey(data), keyPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	for _, name := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
		p := filepath.Join(home, ".ssh", name)
		data, err := os.ReadFile(p)
		if err == nil {
			return trimPubKey(data), p, nil
		}
	}
	return nil, "", errors.New("no operator pubkey found in ~/.ssh (id_ed25519.pub / id_ecdsa.pub / id_rsa.pub)")
}

// trimPubKey strips the trailing newline OpenSSH appends to .pub files
// — authorized_keys lines are written one-per-line by our heredoc.
func trimPubKey(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// HardenSSHD writes /etc/ssh/sshd_config.d/10-convocate-hardening.conf
// with directives drawn from the CIS Ubuntu Linux 22.04 LTS Benchmark
// (sections 5.2.x). The drop-in is chosen because Ubuntu's stock
// sshd_config.d ordering means our values win over the
// 50-cloud-init.conf shipped on cloud images. After writing, the SSH
// daemon is reloaded.
//
// The hardening is intentionally conservative — it does NOT change
// the listen port (some operator workflows depend on :22), and it
// keeps PubkeyAuthentication on so we don't lock ourselves out
// immediately after installing the operator's key.
func HardenSSHD(ctx context.Context, r Runner) error {
	const cfg = `# Managed by convocate-host create-vm. Aligned with CIS Ubuntu 22.04 v1.0.0
# (sections 5.2.x). Do not edit by hand.
Protocol 2
PermitRootLogin no
PasswordAuthentication no
PermitEmptyPasswords no
PubkeyAuthentication yes
ChallengeResponseAuthentication no
KbdInteractiveAuthentication no
UsePAM yes
X11Forwarding no
AllowTcpForwarding no
PermitUserEnvironment no
ClientAliveInterval 300
ClientAliveCountMax 3
LoginGraceTime 60
MaxAuthTries 4
MaxSessions 4
MaxStartups 10:30:60
LogLevel VERBOSE
Banner /etc/issue.net
HostbasedAuthentication no
IgnoreRhosts yes
GSSAPIAuthentication no
KerberosAuthentication no
`
	const path = "/etc/ssh/sshd_config.d/10-convocate-hardening.conf"
	cmd := fmt.Sprintf(`set -e
install -D -m 0644 /dev/stdin %s <<'CIS_EOF'
%sCIS_EOF
sshd -t
systemctl reload ssh || systemctl reload sshd
`, shellQuoteArg(path), cfg)

	return r.Run(ctx, cmd, RunOptions{Sudo: true})
}
