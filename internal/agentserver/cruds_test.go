package agentserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/asymmetric-effort/convocate/internal/config"
	"github.com/asymmetric-effort/convocate/internal/container"
	"github.com/asymmetric-effort/convocate/internal/session"
	"github.com/asymmetric-effort/convocate/internal/statusproto"
	"github.com/asymmetric-effort/convocate/internal/user"
)

// recordingPublisher captures every Event the orchestrator emits so assertions
// can walk the sequence. Concurrency-safe because CRUD ops can fan out via
// RPC goroutines in other tests that share this helper.
type recordingPublisher struct {
	mu     sync.Mutex
	events []statusproto.Event
}

func (p *recordingPublisher) Publish(ev statusproto.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
}

func (p *recordingPublisher) snapshot() []statusproto.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]statusproto.Event, len(p.events))
	copy(out, p.events)
	return out
}

// newTestSessionsDir returns (base, skel) paths for a session.Manager that
// works in a temp dir — enough for CRUD ops that don't actually start docker.
func newTestSessionsDir(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	skel := filepath.Join(base, "skel")
	if err := os.MkdirAll(skel, 0750); err != nil {
		t.Fatal(err)
	}
	return base, skel
}

func testUserInfo() user.Info {
	return user.Info{UID: 1337, GID: 1337, Username: "convocate", HomeDir: "/home/convocate"}
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

// fakeOrch is a minimal Orchestrator that records every call and returns
// canned results. Lets us exercise the RPC layer without touching real
// sessions/containers.
type fakeOrch struct {
	listErr      error
	sessions     []session.Metadata
	getErr       error
	getMeta      session.Metadata
	createErr    error
	createCalled session.CreateOptions
	createName   string
	updateErr    error
	updateID     string
	updateOpts   session.UpdateOptions
	cloneSrc     string
	cloneName    string
	deleted      string
	overridden   string
	killed       string
	backgrounded string
	restarted    string
	restartErr   error
}

func (f *fakeOrch) List() ([]session.Metadata, error) { return f.sessions, f.listErr }
func (f *fakeOrch) Get(id string) (session.Metadata, error) {
	if f.getErr != nil {
		return session.Metadata{}, f.getErr
	}
	return f.getMeta, nil
}
func (f *fakeOrch) Create(opts session.CreateOptions, name string) (session.Metadata, error) {
	f.createCalled = opts
	f.createName = name
	if f.createErr != nil {
		return session.Metadata{}, f.createErr
	}
	return session.Metadata{UUID: "new-uuid", Name: name, Port: opts.Port, Protocol: opts.Protocol, DNSName: opts.DNSName}, nil
}
func (f *fakeOrch) Update(id string, opts session.UpdateOptions) (session.Metadata, error) {
	f.updateID = id
	f.updateOpts = opts
	if f.updateErr != nil {
		return session.Metadata{}, f.updateErr
	}
	return session.Metadata{UUID: id, Name: opts.Name, Port: opts.Port, Protocol: opts.Protocol, DNSName: opts.DNSName}, nil
}
func (f *fakeOrch) Clone(src, name string) (session.Metadata, error) {
	f.cloneSrc = src
	f.cloneName = name
	return session.Metadata{UUID: "cloned", Name: name}, nil
}
func (f *fakeOrch) Delete(id string) error       { f.deleted = id; return nil }
func (f *fakeOrch) OverrideLock(id string) error { f.overridden = id; return nil }
func (f *fakeOrch) Kill(id string) error         { f.killed = id; return nil }
func (f *fakeOrch) Background(id string) error   { f.backgrounded = id; return nil }
func (f *fakeOrch) Restart(id string) error      { f.restarted = id; return f.restartErr }

// callOp drives the dispatcher the same way an SSH client would — JSON
// request in, JSON response out.
func callOp(t *testing.T, d *Dispatcher, op string, params any) Response {
	t.Helper()
	body := Request{Op: op}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		body.Params = raw
	}
	reqBytes, _ := json.Marshal(body)
	var out bytes.Buffer
	d.Handle(bytes.NewReader(reqBytes), &out)
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (raw=%q)", err, out.String())
	}
	return resp
}

func newTestDispatcher(o Orchestrator) *Dispatcher {
	d := NewDispatcher()
	RegisterCoreOps(d, "agent-id", "v-test")
	RegisterCRUDOps(d, o)
	return d
}

func TestCRUD_List(t *testing.T) {
	f := &fakeOrch{sessions: []session.Metadata{{UUID: "a", Name: "one"}, {UUID: "b", Name: "two"}}}
	d := newTestDispatcher(f)
	resp := callOp(t, d, "list", nil)
	if !resp.OK {
		t.Fatalf("list failed: %s", resp.Error)
	}
	var metas []session.Metadata
	if err := json.Unmarshal(resp.Result, &metas); err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Errorf("got %d metas, want 2", len(metas))
	}
}

func TestCRUD_List_Error(t *testing.T) {
	f := &fakeOrch{listErr: errors.New("disk gone")}
	d := newTestDispatcher(f)
	resp := callOp(t, d, "list", nil)
	if resp.OK {
		t.Fatal("expected error")
	}
	if !strings.Contains(resp.Error, "disk gone") {
		t.Errorf("error = %q", resp.Error)
	}
}

func TestCRUD_Get(t *testing.T) {
	f := &fakeOrch{getMeta: session.Metadata{UUID: "u1", Name: "pname", Port: 8080, Protocol: "tcp"}}
	d := newTestDispatcher(f)
	resp := callOp(t, d, "get", IDRequest{ID: "u1"})
	if !resp.OK {
		t.Fatalf("get failed: %s", resp.Error)
	}
	var meta session.Metadata
	if err := json.Unmarshal(resp.Result, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.UUID != "u1" || meta.Port != 8080 {
		t.Errorf("unexpected: %+v", meta)
	}
}

func TestCRUD_Create_PassesAllFields(t *testing.T) {
	f := &fakeOrch{}
	d := newTestDispatcher(f)
	params := CreateRequest{Name: "dns", Port: 53, Protocol: "udp", DNSName: "dns.local"}
	resp := callOp(t, d, "create", params)
	if !resp.OK {
		t.Fatalf("create failed: %s", resp.Error)
	}
	if f.createName != "dns" {
		t.Errorf("name = %q", f.createName)
	}
	if f.createCalled.Port != 53 || f.createCalled.Protocol != "udp" || f.createCalled.DNSName != "dns.local" {
		t.Errorf("create opts = %+v", f.createCalled)
	}
}

func TestCRUD_Edit(t *testing.T) {
	f := &fakeOrch{}
	d := newTestDispatcher(f)
	params := EditRequest{ID: "u1", Name: "renamed", Port: 9090, Protocol: "tcp", DNSName: "svc.local"}
	resp := callOp(t, d, "edit", params)
	if !resp.OK {
		t.Fatalf("edit: %s", resp.Error)
	}
	if f.updateID != "u1" {
		t.Errorf("id = %q", f.updateID)
	}
	if f.updateOpts.Name != "renamed" || f.updateOpts.DNSName != "svc.local" {
		t.Errorf("opts = %+v", f.updateOpts)
	}
}

func TestCRUD_Clone(t *testing.T) {
	f := &fakeOrch{}
	d := newTestDispatcher(f)
	resp := callOp(t, d, "clone", CloneRequest{SourceID: "src", Name: "new"})
	if !resp.OK {
		t.Fatalf("clone: %s", resp.Error)
	}
	if f.cloneSrc != "src" || f.cloneName != "new" {
		t.Errorf("clone args src=%q name=%q", f.cloneSrc, f.cloneName)
	}
}

func TestCRUD_Delete_Kill_Background_Override_Restart(t *testing.T) {
	// These ops all share the same IDRequest shape — sweep them together.
	f := &fakeOrch{}
	d := newTestDispatcher(f)
	ops := []struct {
		op     string
		assign func() string
	}{
		{"delete", func() string { return f.deleted }},
		{"kill", func() string { return f.killed }},
		{"background", func() string { return f.backgrounded }},
		{"override", func() string { return f.overridden }},
		{"restart", func() string { return f.restarted }},
	}
	for _, op := range ops {
		resp := callOp(t, d, op.op, IDRequest{ID: "the-id"})
		if !resp.OK {
			t.Fatalf("%s: %s", op.op, resp.Error)
		}
		if op.assign() != "the-id" {
			t.Errorf("%s: orchestrator id = %q, want 'the-id'", op.op, op.assign())
		}
	}
}

func TestCRUD_Restart_PropagatesError(t *testing.T) {
	f := &fakeOrch{restartErr: errors.New("already running")}
	d := newTestDispatcher(f)
	resp := callOp(t, d, "restart", IDRequest{ID: "x"})
	if resp.OK {
		t.Fatal("expected error")
	}
	if !strings.Contains(resp.Error, "already running") {
		t.Errorf("error = %q", resp.Error)
	}
}

func TestCRUD_RejectsUnknownFields(t *testing.T) {
	d := newTestDispatcher(&fakeOrch{})
	// Hand-crafted JSON with a typo'd field — must not silently succeed.
	reqBytes := []byte(`{"op":"get","params":{"id":"u1","typo":"boom"}}`)
	var out bytes.Buffer
	d.Handle(bytes.NewReader(reqBytes), &out)
	var resp Response
	_ = json.Unmarshal(out.Bytes(), &resp)
	if resp.OK {
		t.Error("expected unknown-field rejection")
	}
}

func TestCRUD_SettingsPlaceholders(t *testing.T) {
	d := newTestDispatcher(&fakeOrch{})
	for _, op := range []string{"settings-get", "settings-set"} {
		resp := callOp(t, d, op, nil)
		if !resp.OK {
			t.Errorf("%s returned ok=false: %s", op, resp.Error)
		}
	}
}

// --- SessionOrchestrator integration tests --------------------------------
//
// These drive the production SessionOrchestrator against a real
// session.Manager on a temp dir, but stub out container-starting so docker
// isn't required.

func TestSessionOrchestrator_CRUDRoundTrip(t *testing.T) {
	base, skelDir := newTestSessionsDir(t)
	mgr := session.NewManager(base, skelDir)
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "test-agent", nil)

	// Create
	meta, err := o.Create(session.CreateOptions{Port: 8080, Protocol: "tcp"}, "svc")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.Port != 8080 {
		t.Errorf("Port = %d, want 8080", meta.Port)
	}

	// Get
	got, err := o.Get(meta.UUID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "svc" {
		t.Errorf("Name = %q", got.Name)
	}

	// List
	list, err := o.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("list len = %d, want 1", len(list))
	}

	// Update
	upd, err := o.Update(meta.UUID, session.UpdateOptions{Name: "renamed", Port: 8081, Protocol: "tcp"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Name != "renamed" || upd.Port != 8081 {
		t.Errorf("update result = %+v", upd)
	}

	// Clone
	cloned, err := o.Clone(meta.UUID, "cloned")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if cloned.UUID == meta.UUID {
		t.Error("clone should have a new UUID")
	}

	// Delete (need to delete clone first to leave the source, then source)
	if err := o.Delete(cloned.UUID); err != nil {
		t.Errorf("Delete clone: %v", err)
	}
	if err := o.Delete(meta.UUID); err != nil {
		t.Errorf("Delete source: %v", err)
	}
}

func TestSessionOrchestrator_List_StampsRunningFromProbe(t *testing.T) {
	base, skelDir := newTestSessionsDir(t)
	mgr := session.NewManager(base, skelDir)
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "test-agent", nil)
	// Replace the default probe with a deterministic stub.
	o.IsRunningFn = func(id string) bool { return id == "running-id" }

	// Create two sessions — the probe will flag only one.
	m1, err := o.Create(session.CreateOptions{}, "one")
	if err != nil {
		t.Fatal(err)
	}
	// Rename the UUID of the second "running" session by hand is awkward;
	// instead, override the probe based on the real UUID after Create.
	m2, err := o.Create(session.CreateOptions{}, "two")
	if err != nil {
		t.Fatal(err)
	}
	o.IsRunningFn = func(id string) bool { return id == m2.UUID }

	list, err := o.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d sessions, want 2", len(list))
	}
	for _, meta := range list {
		want := meta.UUID == m2.UUID
		if meta.Running != want {
			t.Errorf("%s: Running = %v, want %v", meta.UUID[:8], meta.Running, want)
		}
	}

	// Get should stamp Running too.
	g1, err := o.Get(m1.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if g1.Running {
		t.Error("Get(m1) Running = true, want false")
	}
	g2, err := o.Get(m2.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if !g2.Running {
		t.Error("Get(m2) Running = false, want true")
	}
}

func TestSessionOrchestrator_List_NilProbe_LeavesRunningFalse(t *testing.T) {
	base, skelDir := newTestSessionsDir(t)
	mgr := session.NewManager(base, skelDir)
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "test-agent", nil)
	o.IsRunningFn = nil
	if _, err := o.Create(session.CreateOptions{}, "x"); err != nil {
		t.Fatal(err)
	}
	list, _ := o.List()
	if list[0].Running {
		t.Error("with nil probe, Running should be false")
	}
}

func TestSessionOrchestrator_AttachCounterDrivesMetadata(t *testing.T) {
	base, skelDir := newTestSessionsDir(t)
	mgr := session.NewManager(base, skelDir)
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "test-agent", nil)
	o.IsRunningFn = func(id string) bool { return true }

	meta, err := o.Create(session.CreateOptions{}, "svc")
	if err != nil {
		t.Fatal(err)
	}

	// Baseline: not attached.
	got, _ := o.Get(meta.UUID)
	if got.Attached {
		t.Error("pre-track: Attached should be false")
	}

	// Simulate an attach starting.
	o.TrackAttach(meta.UUID)
	got, _ = o.Get(meta.UUID)
	if !got.Attached {
		t.Error("after TrackAttach: Attached should be true")
	}

	// Overlap: second attacher.
	o.TrackAttach(meta.UUID)
	o.TrackDetach(meta.UUID)
	got, _ = o.Get(meta.UUID)
	if !got.Attached {
		t.Error("one attach still open: Attached should be true")
	}
	o.TrackDetach(meta.UUID)
	got, _ = o.Get(meta.UUID)
	if got.Attached {
		t.Error("after both detached: Attached should be false")
	}

	// List mirrors Get.
	o.TrackAttach(meta.UUID)
	list, _ := o.List()
	if len(list) != 1 || !list[0].Attached {
		t.Errorf("list should reflect attach state: %+v", list)
	}
	o.TrackDetach(meta.UUID)
}

func TestSessionOrchestrator_KillBackground_UseHooks(t *testing.T) {
	var killed, detached string
	o := &SessionOrchestrator{
		StopFn:   func(id string) error { killed = id; return nil },
		DetachFn: func(id string) error { detached = id; return nil },
	}
	if err := o.Kill("abc"); err != nil {
		t.Fatal(err)
	}
	if err := o.Background("def"); err != nil {
		t.Fatal(err)
	}
	if killed != "abc" || detached != "def" {
		t.Errorf("Kill=%q Background=%q", killed, detached)
	}
}

func TestSessionOrchestrator_Restart_MissingSession(t *testing.T) {
	base, skelDir := newTestSessionsDir(t)
	mgr := session.NewManager(base, skelDir)
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "test-agent", nil)
	err := o.Restart("missing-uuid")
	if err == nil {
		t.Error("expected error restarting missing session")
	}
}

// --- emit hook tests -------------------------------------------------------
//
// These verify that each CRUD op fires the right status event when a
// publisher is wired. The AgentID in every event must match the orchestrator's
// configured AgentID so the shell can route per-agent.

func TestEmit_NilPublisherIsNoOp(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	// Explicitly nil publisher — must not panic, must be a silent no-op.
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "agent-x", nil)
	if _, err := o.Create(session.CreateOptions{Port: 0, Protocol: "tcp"}, "no-pub"); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestEmit_CreateEmitsContainerCreated(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	pub := &recordingPublisher{}
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "agent-1", pub)

	meta, err := o.Create(session.CreateOptions{Port: 7070, Protocol: "tcp"}, "svc")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	events := pub.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Type != statusproto.TypeContainerCreated {
		t.Errorf("Type = %q, want %q", ev.Type, statusproto.TypeContainerCreated)
	}
	if ev.AgentID != "agent-1" {
		t.Errorf("AgentID = %q", ev.AgentID)
	}
	if ev.SessionID != meta.UUID {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, meta.UUID)
	}
	if len(ev.Data) == 0 {
		t.Error("expected Data payload (encoded Metadata)")
	}
	// Decode the data back — should round-trip to the same metadata.
	var got session.Metadata
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if got.Port != 7070 || got.Name != "svc" {
		t.Errorf("data = %+v", got)
	}
}

func TestEmit_UpdateEmitsContainerEdited(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	pub := &recordingPublisher{}
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "agent-1", pub)

	meta, err := o.Create(session.CreateOptions{Port: 8080, Protocol: "tcp"}, "orig")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Update(meta.UUID, session.UpdateOptions{Name: "renamed", Port: 8081, Protocol: "tcp"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	events := pub.snapshot()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (created+edited)", len(events))
	}
	if events[1].Type != statusproto.TypeContainerEdited {
		t.Errorf("events[1].Type = %q, want %q", events[1].Type, statusproto.TypeContainerEdited)
	}
	if events[1].SessionID != meta.UUID {
		t.Errorf("SessionID = %q", events[1].SessionID)
	}
}

func TestEmit_CloneEmitsContainerCreated(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	pub := &recordingPublisher{}
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "agent-1", pub)

	src, err := o.Create(session.CreateOptions{Port: 0, Protocol: "tcp"}, "src")
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := o.Clone(src.UUID, "dup")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	events := pub.snapshot()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[1].Type != statusproto.TypeContainerCreated {
		t.Errorf("events[1].Type = %q, want %q", events[1].Type, statusproto.TypeContainerCreated)
	}
	if events[1].SessionID != cloned.UUID {
		t.Errorf("SessionID = %q, want %q", events[1].SessionID, cloned.UUID)
	}
}

func TestEmit_DeleteEmitsContainerDeleted(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	pub := &recordingPublisher{}
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "agent-1", pub)

	meta, err := o.Create(session.CreateOptions{Port: 0, Protocol: "tcp"}, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Delete(meta.UUID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	events := pub.snapshot()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[1].Type != statusproto.TypeContainerDeleted {
		t.Errorf("events[1].Type = %q", events[1].Type)
	}
	if events[1].SessionID != meta.UUID {
		t.Errorf("SessionID = %q", events[1].SessionID)
	}
	// container.deleted has no payload — Data should be empty/null.
	if len(events[1].Data) != 0 {
		t.Errorf("expected empty Data for delete, got %q", events[1].Data)
	}
}

func TestEmit_KillEmitsContainerStopped(t *testing.T) {
	pub := &recordingPublisher{}
	o := &SessionOrchestrator{
		AgentID:   "agent-k",
		Publisher: pub,
		StopFn:    func(id string) error { return nil },
	}
	if err := o.Kill("target-id"); err != nil {
		t.Fatal(err)
	}
	events := pub.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Type != statusproto.TypeContainerStopped {
		t.Errorf("Type = %q", events[0].Type)
	}
	if events[0].SessionID != "target-id" {
		t.Errorf("SessionID = %q", events[0].SessionID)
	}
	if events[0].AgentID != "agent-k" {
		t.Errorf("AgentID = %q", events[0].AgentID)
	}
}

func TestEmit_KillFailureDoesNotEmit(t *testing.T) {
	pub := &recordingPublisher{}
	o := &SessionOrchestrator{
		AgentID:   "agent-k",
		Publisher: pub,
		StopFn:    func(id string) error { return errors.New("no such container") },
	}
	if err := o.Kill("x"); err == nil {
		t.Fatal("expected error")
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("expected no events on failure, got %d", len(pub.snapshot()))
	}
}

func TestEmit_DeleteFailureDoesNotEmit(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	pub := &recordingPublisher{}
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "agent-1", pub)
	if err := o.Delete("does-not-exist"); err == nil {
		t.Fatal("expected error")
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("expected no events, got %d", len(pub.snapshot()))
	}
}

func TestCRUD_OpListIsStable(t *testing.T) {
	d := newTestDispatcher(&fakeOrch{})
	ops := d.Ops()
	// Spot-check: must include every op we promised the client.
	required := []string{
		"ping", "list", "get", "create", "edit", "clone",
		"delete", "kill", "background", "override", "restart",
		"settings-get", "settings-set",
	}
	for _, r := range required {
		found := false
		for _, o := range ops {
			if o == r {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing op %q in registered set: %v", r, ops)
		}
	}
}

// --- Production SessionOrchestrator: Restart + OverrideLock paths --------

// fakeExecBuilder returns an ExecFunc that scripts the docker calls
// produced by container.Runner. Maps subcommand → exit-code/output:
//
//	"inspect" → echo "<isRunning>"   (no error → IsRunning OK)
//	"run"     → true / false         (StartDetached success/failure)
//	"stop"    → true                 (Stop unconditionally succeeds)
func fakeExecBuilder(isRunning, startOK bool) container.ExecFunc {
	return func(name string, args ...string) *exec.Cmd {
		// docker inspect ... → echo true|false
		for _, a := range args {
			if a == "inspect" {
				if isRunning {
					return exec.Command("echo", "true")
				}
				return exec.Command("echo", "false")
			}
		}
		// docker run ... or docker stop ... → success/failure based on startOK
		if startOK {
			return exec.Command("true")
		}
		return exec.Command("false")
	}
}

func TestSessionOrchestrator_Restart_HappyPath(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	pub := &recordingPublisher{}
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "8.8.8.8", "agent-r", pub)
	// Stub the runner factory so docker isn't required.
	o.NewRunner = func(id, dir string, u user.Info, p config.Paths) *container.Runner {
		return container.NewRunnerWithExec(id, dir, u, p, fakeExecBuilder(false, true))
	}

	meta, err := o.Create(session.CreateOptions{Port: 9090, Protocol: "tcp"}, "rs")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pub.snapshot() // discard create event

	if err := o.Restart(meta.UUID); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	// Restart must publish ContainerStarted. Find it among events fired
	// after Create (which already emitted ContainerCreated).
	events := pub.snapshot()
	var sawStarted bool
	for _, ev := range events {
		if ev.Type == statusproto.TypeContainerStarted && ev.SessionID == meta.UUID {
			sawStarted = true
		}
	}
	if !sawStarted {
		t.Errorf("Restart did not publish container.started; events=%+v", events)
	}
}

func TestSessionOrchestrator_Restart_RefusesAlreadyRunning(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "agent-r", nil)
	o.NewRunner = func(id, dir string, u user.Info, p config.Paths) *container.Runner {
		// Pretend container is already up — Restart must refuse.
		return container.NewRunnerWithExec(id, dir, u, p, fakeExecBuilder(true, true))
	}

	meta, err := o.Create(session.CreateOptions{}, "rs2")
	if err != nil {
		t.Fatal(err)
	}
	err = o.Restart(meta.UUID)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("expected already-running error, got %v", err)
	}
}

func TestSessionOrchestrator_Restart_StartDetachedFails(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "agent-r", nil)
	o.NewRunner = func(id, dir string, u user.Info, p config.Paths) *container.Runner {
		return container.NewRunnerWithExec(id, dir, u, p, fakeExecBuilder(false, false))
	}

	meta, err := o.Create(session.CreateOptions{}, "rs3")
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Restart(meta.UUID); err == nil {
		t.Error("expected start-detached error to propagate")
	}
}

func TestSessionOrchestrator_OverrideLock_RemovesStaleLock(t *testing.T) {
	base, skel := newTestSessionsDir(t)
	mgr := session.NewManager(base, skel)
	o := NewSessionOrchestrator(mgr, testUserInfo(), testPaths(), "", "agent-o", nil)

	meta, err := o.Create(session.CreateOptions{}, "ovr")
	if err != nil {
		t.Fatal(err)
	}
	// Plant a stale lock owned by a definitely-dead PID. Don't call
	// mgr.IsLocked beforehand — it would drop the stale lock itself
	// and the test would race the deletion.
	lockPath := filepath.Join(base, meta.UUID+config.LockFileExtension)
	if err := os.WriteFile(lockPath, []byte("999999\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := o.OverrideLock(meta.UUID); err != nil {
		t.Fatalf("OverrideLock: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("lock file still present: %v", err)
	}
}

// --- ptyRWC + Server.Close trivial wrappers --------------------------------

func TestPtyRWC_RoundTrip(t *testing.T) {
	// ptyRWC is a thin os.File adapter — give it a temp file and
	// verify Read/Write/Close pass through.
	dir := t.TempDir()
	path := filepath.Join(dir, "pty-stub")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	rwc := ptyRWC{f: f}

	if _, err := rwc.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := rwc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open to verify content (tempfile-based round-trip).
	r, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rwc2 := ptyRWC{f: r}
	buf := make([]byte, 16)
	n, err := rwc2.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "hello\n" {
		t.Errorf("read = %q, want %q", buf[:n], "hello\n")
	}
}

func TestParseStringPayload(t *testing.T) {
	// uint32 length + string bytes.
	if got := parseStringPayload([]byte{0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'}); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
	if got := parseStringPayload(nil); got != "" {
		t.Errorf("nil payload should decode to empty, got %q", got)
	}
	if got := parseStringPayload([]byte{0, 0, 1}); got != "" {
		t.Errorf("short header should decode to empty, got %q", got)
	}
	if got := parseStringPayload([]byte{0, 0, 0, 99, 'x'}); got != "" {
		t.Errorf("overflow length should decode to empty, got %q", got)
	}
}

func TestServer_Close_NoOp(t *testing.T) {
	dir := t.TempDir()
	hostKeyPath := filepath.Join(dir, "host_key")
	authPath := filepath.Join(dir, "auth")
	if err := os.WriteFile(authPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher()
	RegisterCoreOps(d, "x", "v")
	srv, err := New(Config{
		HostKeyPath:        hostKeyPath,
		AuthorizedKeysPath: authPath,
		Listen:             "127.0.0.1:0",
		Dispatcher:         d,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Errorf("Close should be a no-op, got %v", err)
	}
}
