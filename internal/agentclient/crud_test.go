package agentclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/asymmetric-effort/convocate/internal/agentserver"
	"github.com/asymmetric-effort/convocate/internal/session"
	"github.com/asymmetric-effort/convocate/internal/sshutil"
)

// spinUpAgent boots an in-process agentserver.Server on a free port and
// returns (addr, cancel, orch) where orch is the fake Orchestrator the test
// can inspect or pre-populate. Client keys are written to clientKey + its
// pubkey is authorized on the server.
func spinUpAgent(t *testing.T, orch agentserver.Orchestrator) (addr, clientKeyPath string, cancel context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	hostKeyPath := filepath.Join(dir, "host_key")
	authPath := filepath.Join(dir, "authorized_keys")
	clientKeyPath = filepath.Join(dir, "client_key")

	// Generate the client keypair. LoadOrCreateHostKey writes an OpenSSH
	// private key to disk and returns a signer whose public key we can
	// authorize on the server.
	signer, err := sshutil.LoadOrCreateHostKey(clientKeyPath)
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	authBytes := ssh.MarshalAuthorizedKey(signer.PublicKey())
	if err := os.WriteFile(authPath, authBytes, 0600); err != nil {
		t.Fatal(err)
	}

	d := agentserver.NewDispatcher()
	agentserver.RegisterCoreOps(d, "agent-test", "v0.0.0")
	agentserver.RegisterCRUDOps(d, orch)

	// Find a free port and hand it to New.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = ln.Addr().String()
	_ = ln.Close()

	srv, err := agentserver.New(agentserver.Config{
		HostKeyPath:        hostKeyPath,
		AuthorizedKeysPath: authPath,
		Listen:             addr,
		Dispatcher:         d,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ctx, c := context.WithCancel(context.Background())
	cancel = c
	go func() { _ = srv.Serve(ctx) }()
	// Give the listener a moment to bind.
	time.Sleep(50 * time.Millisecond)
	return addr, clientKeyPath, cancel
}

// fakeOrch is a minimal Orchestrator that returns canned data — enough to
// exercise the client's serialization round-trips without needing real
// sessions or a real docker daemon.
type fakeOrch struct {
	sessions   []session.Metadata
	createOpts session.CreateOptions
	createName string
	updateID   string
	updateOpts session.UpdateOptions
	clonedSrc  string
	clonedName string
	deleted    string
	killed     string
	backgrnd   string
	overridden string
	restarted  string
}

func (f *fakeOrch) List() ([]session.Metadata, error) { return f.sessions, nil }
func (f *fakeOrch) Get(id string) (session.Metadata, error) {
	for _, s := range f.sessions {
		if s.UUID == id {
			return s, nil
		}
	}
	return session.Metadata{}, nil
}
func (f *fakeOrch) Create(opts session.CreateOptions, name string) (session.Metadata, error) {
	f.createOpts = opts
	f.createName = name
	return session.Metadata{UUID: "new", Name: name, Port: opts.Port}, nil
}
func (f *fakeOrch) Update(id string, opts session.UpdateOptions) (session.Metadata, error) {
	f.updateID = id
	f.updateOpts = opts
	return session.Metadata{UUID: id, Name: opts.Name, Port: opts.Port}, nil
}
func (f *fakeOrch) Clone(src, name string) (session.Metadata, error) {
	f.clonedSrc = src
	f.clonedName = name
	return session.Metadata{UUID: "clone-uuid", Name: name}, nil
}
func (f *fakeOrch) Delete(id string) error       { f.deleted = id; return nil }
func (f *fakeOrch) OverrideLock(id string) error { f.overridden = id; return nil }
func (f *fakeOrch) Kill(id string) error         { f.killed = id; return nil }
func (f *fakeOrch) Background(id string) error   { f.backgrnd = id; return nil }
func (f *fakeOrch) Restart(id string) error      { f.restarted = id; return nil }

// --- tests -----------------------------------------------------------------

func TestCRUDClient_NewRequiresHost(t *testing.T) {
	_, err := NewCRUDClient(CRUDConfig{})
	if err == nil {
		t.Fatal("expected error without AgentHost")
	}
}

func TestCRUDClient_EndToEnd_Ping(t *testing.T) {
	orch := &fakeOrch{}
	addr, keyPath, cancel := spinUpAgent(t, orch)
	defer cancel()

	host, port := splitHostPort(t, addr)
	c, err := NewCRUDClient(CRUDConfig{
		AgentHost:      host,
		AgentPort:      port,
		PrivateKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("NewCRUDClient: %v", err)
	}
	defer c.Close()

	resp, err := c.Ping()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if resp.AgentID != "agent-test" {
		t.Errorf("AgentID = %q", resp.AgentID)
	}
	if resp.Version != "v0.0.0" {
		t.Errorf("Version = %q", resp.Version)
	}
	if resp.ServerTime == "" {
		t.Error("ServerTime empty")
	}
}

func TestCRUDClient_ListGet(t *testing.T) {
	orch := &fakeOrch{
		sessions: []session.Metadata{
			{UUID: "a", Name: "alpha", Port: 80, Protocol: "tcp"},
			{UUID: "b", Name: "beta", Port: 0, Protocol: "tcp"},
		},
	}
	addr, keyPath, cancel := spinUpAgent(t, orch)
	defer cancel()
	host, port := splitHostPort(t, addr)
	c, err := NewCRUDClient(CRUDConfig{AgentHost: host, AgentPort: port, PrivateKeyPath: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	list, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].UUID != "a" || list[1].Name != "beta" {
		t.Errorf("unexpected list: %+v", list)
	}

	got, err := c.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "alpha" || got.Port != 80 {
		t.Errorf("get = %+v", got)
	}
}

func TestCRUDClient_WriteOps(t *testing.T) {
	orch := &fakeOrch{}
	addr, keyPath, cancel := spinUpAgent(t, orch)
	defer cancel()
	host, port := splitHostPort(t, addr)
	c, err := NewCRUDClient(CRUDConfig{AgentHost: host, AgentPort: port, PrivateKeyPath: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Create(agentserver.CreateRequest{Name: "svc", Port: 9090, Protocol: "tcp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if orch.createName != "svc" || orch.createOpts.Port != 9090 {
		t.Errorf("orch create = %+v, %q", orch.createOpts, orch.createName)
	}

	if _, err := c.Edit(agentserver.EditRequest{ID: "x", Name: "renamed", Port: 8080, Protocol: "tcp"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if orch.updateID != "x" || orch.updateOpts.Name != "renamed" {
		t.Errorf("orch update = %q, %+v", orch.updateID, orch.updateOpts)
	}

	if _, err := c.Clone("src", "new"); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if orch.clonedSrc != "src" || orch.clonedName != "new" {
		t.Errorf("orch clone src=%q name=%q", orch.clonedSrc, orch.clonedName)
	}

	for _, tc := range []struct {
		name string
		call func() error
		got  *string
	}{
		{"delete", func() error { return c.Delete("d1") }, &orch.deleted},
		{"kill", func() error { return c.Kill("k1") }, &orch.killed},
		{"background", func() error { return c.Background("b1") }, &orch.backgrnd},
		{"override", func() error { return c.Override("o1") }, &orch.overridden},
		{"restart", func() error { return c.Restart("r1") }, &orch.restarted},
	} {
		if err := tc.call(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if *tc.got == "" {
			t.Errorf("%s: orch didn't record the call", tc.name)
		}
	}
}

func TestCRUDClient_HeartbeatKeepsHealthy(t *testing.T) {
	orch := &fakeOrch{}
	addr, keyPath, cancel := spinUpAgent(t, orch)
	defer cancel()

	host, port := splitHostPort(t, addr)
	c, err := NewCRUDClient(CRUDConfig{
		AgentHost:         host,
		AgentPort:         port,
		PrivateKeyPath:    keyPath,
		HeartbeatInterval: 80 * time.Millisecond,
		ReconnectBackoff:  20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Give the heartbeat a couple of ticks to fire.
	time.Sleep(250 * time.Millisecond)
	if !c.Healthy() {
		t.Error("client should be healthy after successful heartbeats")
	}
}

func TestCRUDClient_ReconnectRestoresCall(t *testing.T) {
	// End-to-end: initial conn works, we yank the SSH conn out from
	// under the client, the heartbeat goroutine detects the breakage
	// and redials, and follow-up Call invocations start succeeding
	// again. Doesn't rely on catching the transient unhealthy window —
	// that's logged but too race-sensitive to poll reliably.
	orch := &fakeOrch{sessions: []session.Metadata{{UUID: "alive"}}}
	addr, keyPath, cancel := spinUpAgent(t, orch)
	defer cancel()

	host, port := splitHostPort(t, addr)
	c, err := NewCRUDClient(CRUDConfig{
		AgentHost:           host,
		AgentPort:           port,
		PrivateKeyPath:      keyPath,
		HeartbeatInterval:   60 * time.Millisecond,
		ReconnectBackoff:    20 * time.Millisecond,
		MaxReconnectBackoff: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Baseline: ping works.
	if _, err := c.Ping(); err != nil {
		t.Fatalf("initial ping: %v", err)
	}

	// Force-close the connection. Immediate calls will fail; the
	// background heartbeat picks up the pieces and reconnects.
	c.mu.RLock()
	broken := c.conn
	c.mu.RUnlock()
	if broken != nil {
		_ = broken.Close()
	}

	// Poll until a List call succeeds again — proof the conn was
	// replaced by the heartbeat's reconnect path.
	deadline := time.Now().Add(3 * time.Second)
	var listErr error
	for time.Now().Before(deadline) {
		if _, err := c.List(); err == nil {
			return
		} else {
			listErr = err
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("List never recovered after forced conn close; last err=%v", listErr)
}

func TestCRUDClient_CallAfterCloseFails(t *testing.T) {
	orch := &fakeOrch{}
	addr, keyPath, cancel := spinUpAgent(t, orch)
	defer cancel()

	host, port := splitHostPort(t, addr)
	c, err := NewCRUDClient(CRUDConfig{AgentHost: host, AgentPort: port, PrivateKeyPath: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	if _, err := c.Ping(); err == nil {
		t.Error("Ping after Close should fail")
	}
}

func TestCRUDClient_BadKeyRejected(t *testing.T) {
	orch := &fakeOrch{}
	addr, _, cancel := spinUpAgent(t, orch)
	defer cancel()

	// Write a different key that the server doesn't authorize.
	dir := t.TempDir()
	badKey := filepath.Join(dir, "bad")
	if _, err := sshutil.LoadOrCreateHostKey(badKey); err != nil {
		t.Fatal(err)
	}
	host, port := splitHostPort(t, addr)
	_, err := NewCRUDClient(CRUDConfig{AgentHost: host, AgentPort: port, PrivateKeyPath: badKey})
	if err == nil {
		t.Fatal("expected auth to fail with unauthorized key")
	}
}

// splitHostPort splits a "host:port" addr into (host, int port). Fails the
// test on a malformed addr.
func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var p int
	for _, c := range portStr {
		if c < '0' || c > '9' {
			t.Fatalf("bad port: %q", portStr)
		}
		p = p*10 + int(c-'0')
	}
	return host, p
}
