package multihost

import (
	"errors"
	"strings"
	"testing"

	"github.com/asymmetric-effort/convocate/internal/agentclient"
	"github.com/asymmetric-effort/convocate/internal/agentserver"
	"github.com/asymmetric-effort/convocate/internal/session"
)

// --- stubs ------------------------------------------------------------------

type fakeLocal struct {
	sessions   []session.Metadata
	deleted    string
	overridden string
	updated    string
	cloneSrc   string
	cloneName  string
	listErr    error
	locked     map[string]bool
}

func (f *fakeLocal) List() ([]session.Metadata, error) { return f.sessions, f.listErr }
func (f *fakeLocal) Get(id string) (session.Metadata, error) {
	for _, s := range f.sessions {
		if s.UUID == id {
			return s, nil
		}
	}
	return session.Metadata{}, errors.New("not found")
}
func (f *fakeLocal) Delete(id string) error       { f.deleted = id; return nil }
func (f *fakeLocal) OverrideLock(id string) error { f.overridden = id; return nil }
func (f *fakeLocal) UpdateWithOptions(id string, opts session.UpdateOptions) (session.Metadata, error) {
	f.updated = id
	return session.Metadata{UUID: id, Name: opts.Name, Port: opts.Port, Protocol: opts.Protocol, DNSName: opts.DNSName}, nil
}
func (f *fakeLocal) CreateWithOptions(name string, opts session.CreateOptions) (session.Metadata, error) {
	return session.Metadata{UUID: "new", Name: name, Port: opts.Port}, nil
}
func (f *fakeLocal) Clone(src, name string) (session.Metadata, error) {
	f.cloneSrc = src
	f.cloneName = name
	return session.Metadata{UUID: "local-clone", Name: name}, nil
}
func (f *fakeLocal) IsLocked(id string) bool { return f.locked[id] }

type fakeAgentClient struct {
	sessions   []session.Metadata
	listErr    error
	deleted    string
	killed     string
	backgrnd   string
	overridden string
	restarted  string
	editID     string
	editOpts   agentserver.EditRequest
	cloneSrc   string
	cloneName  string
	cloneErr   error
}

func (f *fakeAgentClient) List() ([]session.Metadata, error) { return f.sessions, f.listErr }
func (f *fakeAgentClient) Create(req agentserver.CreateRequest) (session.Metadata, error) {
	return session.Metadata{UUID: "agent-new", Name: req.Name}, nil
}
func (f *fakeAgentClient) Edit(req agentserver.EditRequest) (session.Metadata, error) {
	f.editID = req.ID
	f.editOpts = req
	return session.Metadata{UUID: req.ID, Name: req.Name}, nil
}
func (f *fakeAgentClient) Clone(src, name string) (session.Metadata, error) {
	f.cloneSrc = src
	f.cloneName = name
	if f.cloneErr != nil {
		return session.Metadata{}, f.cloneErr
	}
	return session.Metadata{UUID: "remote-clone", Name: name}, nil
}
func (f *fakeAgentClient) Delete(id string) error     { f.deleted = id; return nil }
func (f *fakeAgentClient) Kill(id string) error       { f.killed = id; return nil }
func (f *fakeAgentClient) Background(id string) error { f.backgrnd = id; return nil }
func (f *fakeAgentClient) Override(id string) error   { f.overridden = id; return nil }
func (f *fakeAgentClient) Restart(id string) error    { f.restarted = id; return nil }
func (f *fakeAgentClient) Close() error               { return nil }

// --- tests -----------------------------------------------------------------

func TestRouter_ListNoAgents_IsLocalOnly(t *testing.T) {
	local := &fakeLocal{sessions: []session.Metadata{{UUID: "a", Name: "one"}, {UUID: "b", Name: "two"}}}
	r := &Router{Local: local}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
	for _, s := range got {
		if s.IsRemote() {
			t.Errorf("unexpected remote flag on %s", s.UUID)
		}
	}
}

func TestRouter_ListStampsAgentIdentity(t *testing.T) {
	local := &fakeLocal{sessions: []session.Metadata{{UUID: "l1", Name: "local"}}}
	ag := &fakeAgentClient{sessions: []session.Metadata{
		{UUID: "r1", Name: "remote-one"},
		{UUID: "r2", Name: "remote-two"},
	}}
	r := &Router{
		Local: local,
		Agents: []AgentRef{{
			Record: agentclient.AgentRecord{ID: "alpha", Host: "a.host"},
			Client: ag,
		}},
	}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	// First entry is local (no stamp).
	if got[0].IsRemote() {
		t.Errorf("local entry marked remote")
	}
	// Remote entries stamped with agent id + host.
	for _, s := range got[1:] {
		if s.AgentID != "alpha" || s.AgentHost != "a.host" {
			t.Errorf("%s: agent=(%q,%q)", s.UUID, s.AgentID, s.AgentHost)
		}
	}
}

func TestRouter_OpsRouteByID(t *testing.T) {
	local := &fakeLocal{sessions: []session.Metadata{{UUID: "l1"}}}
	ag := &fakeAgentClient{sessions: []session.Metadata{{UUID: "r1"}}}
	r := &Router{
		Local: local,
		Agents: []AgentRef{{
			Record: agentclient.AgentRecord{ID: "A", Host: "h"},
			Client: ag,
		}},
	}
	// Build the routing map.
	if _, err := r.List(); err != nil {
		t.Fatal(err)
	}

	// Delete remote → goes to agent.
	if err := r.Delete("r1"); err != nil {
		t.Fatal(err)
	}
	if ag.deleted != "r1" || local.deleted != "" {
		t.Errorf("delete routing wrong: agent=%q local=%q", ag.deleted, local.deleted)
	}

	// Delete local → goes to Manager (still allowed as orphan cleanup).
	if err := r.Delete("l1"); err != nil {
		t.Fatal(err)
	}
	if local.deleted != "l1" {
		t.Errorf("local delete not routed: %q", local.deleted)
	}

	// Kill remote → agent.Kill.
	if err := r.Kill("r1"); err != nil {
		t.Fatal(err)
	}
	if ag.killed != "r1" {
		t.Errorf("agent kill not invoked: %q", ag.killed)
	}
	// Kill local orphan → migration error.
	if err := r.Kill("l1"); err == nil || !strings.Contains(err.Error(), "orphan") {
		t.Errorf("kill on orphan should return orphan error, got %v", err)
	}

	// Override remote → agent.Override.
	if err := r.OverrideLock("r1"); err != nil {
		t.Fatal(err)
	}
	if ag.overridden != "r1" {
		t.Errorf("agent override not invoked: %q", ag.overridden)
	}

	// Restart remote → agent.Restart.
	if err := r.Restart("r1"); err != nil {
		t.Fatal(err)
	}
	if ag.restarted != "r1" {
		t.Errorf("agent restart not invoked")
	}

	// Update remote → agent.Edit with the right fields.
	if err := r.Update("r1", "new-name", "tcp", "svc.local", 8080); err != nil {
		t.Fatal(err)
	}
	if ag.editID != "r1" || ag.editOpts.Name != "new-name" || ag.editOpts.Port != 8080 {
		t.Errorf("agent edit wrong: %+v", ag.editOpts)
	}

	// Clone remote → agent.Clone.
	cloned, err := r.Clone("r1", "dup")
	if err != nil {
		t.Fatal(err)
	}
	if cloned.AgentID != "A" || cloned.Name != "dup" {
		t.Errorf("clone result: %+v", cloned)
	}
}

func TestRouter_UnknownIDFallsLocal(t *testing.T) {
	local := &fakeLocal{}
	r := &Router{Local: local}
	if err := r.Delete("brand-new"); err != nil {
		t.Fatal(err)
	}
	if local.deleted != "brand-new" {
		t.Errorf("unknown id should fall through to local delete, got %q", local.deleted)
	}
}

func TestRouter_UnreachableAgent_SurfacesInList(t *testing.T) {
	local := &fakeLocal{sessions: []session.Metadata{{UUID: "l1"}}}
	ag := &fakeAgentClient{listErr: errors.New("dial tcp: refused")}
	r := &Router{
		Local: local,
		Agents: []AgentRef{{
			Record: agentclient.AgentRecord{ID: "broken", Host: "down.example"},
			Client: ag,
		}},
	}
	got, err := r.List()
	if err != nil {
		t.Fatalf("List should not error on unreachable agent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (local + sentinel)", len(got))
	}
	sentinel := got[1]
	if !strings.Contains(sentinel.UUID, "agent-unreachable") {
		t.Errorf("sentinel UUID = %q", sentinel.UUID)
	}
	if sentinel.AgentID != "broken" {
		t.Errorf("sentinel AgentID = %q", sentinel.AgentID)
	}
	if !strings.Contains(sentinel.Name, "refused") {
		t.Errorf("sentinel Name = %q; want error text", sentinel.Name)
	}
}

func TestRouter_IsRunningReflectsSources(t *testing.T) {
	local := &fakeLocal{sessions: []session.Metadata{{UUID: "l1"}}, locked: map[string]bool{"l1": true}}
	// r1 is running on the remote agent; r2 is not. The agent's list
	// op stamps Running per entry before the router aggregates.
	ag := &fakeAgentClient{sessions: []session.Metadata{
		{UUID: "r1", Running: true},
		{UUID: "r2", Running: false},
	}}
	r := &Router{
		Local: local,
		Agents: []AgentRef{{
			Record: agentclient.AgentRecord{ID: "A"},
			Client: ag,
		}},
	}
	_, _ = r.List()

	// Local orphans always report false under v2.x — no docker probing.
	if r.IsRunning("l1") {
		t.Error("local orphan should report Running=false")
	}
	if !r.IsRunning("r1") {
		t.Error("remote r1 should be running (agent stamped it)")
	}
	if r.IsRunning("r2") {
		t.Error("remote r2 should not be running")
	}
	// Orphan lock state collapses to false — no more local-lock semantics.
	if r.IsLocked("l1") {
		t.Error("orphan lock should be false in v2.x")
	}
	if r.IsLocked("r1") {
		t.Error("remote lock should be false")
	}
}

func TestRouter_ListStampsRunningOnAggregatedMetadata(t *testing.T) {
	// The session.Metadata entries returned by List must carry Running,
	// not just have it in the router's internal map — the TUI reads
	// Running off the Metadata directly to render status columns.
	local := &fakeLocal{sessions: []session.Metadata{{UUID: "l1"}}}
	ag := &fakeAgentClient{sessions: []session.Metadata{{UUID: "r1", Running: true}}}
	r := &Router{
		Local:  local,
		Agents: []AgentRef{{Record: agentclient.AgentRecord{ID: "A"}, Client: ag}},
	}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	var remote session.Metadata
	for _, s := range got {
		if s.UUID == "r1" {
			remote = s
		}
	}
	if !remote.Running {
		t.Errorf("remote Metadata.Running = false, want true")
	}
}

func TestRouter_OrphanWritesRejected(t *testing.T) {
	local := &fakeLocal{sessions: []session.Metadata{{UUID: "orphan-1"}}}
	r := &Router{Local: local}
	if _, err := r.List(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Kill", func() error { return r.Kill("orphan-1") }},
		{"Background", func() error { return r.Background("orphan-1") }},
		{"Restart", func() error { return r.Restart("orphan-1") }},
		{"OverrideLock", func() error { return r.OverrideLock("orphan-1") }},
		{"Update", func() error { return r.Update("orphan-1", "x", "tcp", "", 0) }},
		{"Clone", func() error { _, err := r.Clone("orphan-1", "x"); return err }},
	} {
		if err := tc.call(); err == nil || !strings.Contains(err.Error(), "orphan") {
			t.Errorf("%s on orphan: got %v, want orphan error", tc.name, err)
		}
	}
}

func TestRouter_CreateRequiresAgentID(t *testing.T) {
	r := &Router{Local: &fakeLocal{}}
	_, err := r.Create("", session.CreateOptions{}, "name")
	if err == nil || !strings.Contains(err.Error(), "agent-id required") {
		t.Errorf("expected agent-id required, got %v", err)
	}
}

func TestRouter_CreateUnknownAgent_Errors(t *testing.T) {
	r := &Router{Local: &fakeLocal{}}
	_, err := r.Create("nonexistent", session.CreateOptions{}, "x")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("expected not-registered, got %v", err)
	}
}

func TestRouter_AgentFor_Exported(t *testing.T) {
	local := &fakeLocal{}
	ag := &fakeAgentClient{sessions: []session.Metadata{{UUID: "r1"}}}
	r := &Router{Local: local, Agents: []AgentRef{{Record: agentclient.AgentRecord{ID: "A"}, Client: ag}}}
	_, _ = r.List()
	if got := r.AgentFor("r1"); got == nil || got.Record.ID != "A" {
		t.Errorf("AgentFor returned %+v", got)
	}
	if got := r.AgentFor("unknown"); got != nil {
		t.Errorf("AgentFor(unknown) = %+v, want nil", got)
	}
}

func TestRouter_Background_Remote(t *testing.T) {
	local := &fakeLocal{}
	ag := &fakeAgentClient{sessions: []session.Metadata{{UUID: "r1"}}}
	r := &Router{Local: local, Agents: []AgentRef{{Record: agentclient.AgentRecord{ID: "A"}, Client: ag}}}
	_, _ = r.List()
	if err := r.Background("r1"); err != nil {
		t.Fatal(err)
	}
	if ag.backgrnd != "r1" {
		t.Errorf("agent.Background not invoked: %q", ag.backgrnd)
	}
}

func TestRouter_Create_RegistersInRouting(t *testing.T) {
	local := &fakeLocal{}
	ag := &fakeAgentClient{}
	r := &Router{Local: local, Agents: []AgentRef{{Record: agentclient.AgentRecord{ID: "A", Host: "a-host"}, Client: ag}}}
	meta, err := r.Create("A", session.CreateOptions{Port: 8080, Protocol: "tcp"}, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if meta.AgentID != "A" || meta.AgentHost != "a-host" {
		t.Errorf("meta stamps wrong: %+v", meta)
	}
	// Follow-up op on the new UUID must route to the agent without
	// needing an explicit List refresh.
	if err := r.Kill(meta.UUID); err != nil {
		t.Fatal(err)
	}
	if ag.killed != meta.UUID {
		t.Errorf("Kill didn't route to agent: %q", ag.killed)
	}
}

func TestRouter_List_LocalListFails(t *testing.T) {
	r := &Router{Local: &fakeLocal{listErr: errors.New("disk gone")}}
	if _, err := r.List(); err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Errorf("expected local-list error propagation, got %v", err)
	}
}

func TestRouter_AgentIDs(t *testing.T) {
	r := &Router{Local: &fakeLocal{}}
	if got := r.AgentIDs(); len(got) != 0 {
		t.Errorf("empty router AgentIDs = %v, want []", got)
	}

	r.Agents = []AgentRef{
		{Record: agentclient.AgentRecord{ID: "alpha"}},
		{Record: agentclient.AgentRecord{ID: "beta"}},
		{Record: agentclient.AgentRecord{ID: "gamma"}},
	}
	got := r.AgentIDs()
	if len(got) != 3 || got[0] != "alpha" || got[1] != "beta" || got[2] != "gamma" {
		t.Errorf("AgentIDs = %v, want [alpha beta gamma] in order", got)
	}
}

func TestRouter_Clone_AgentErrorPropagates(t *testing.T) {
	ag := &fakeAgentClient{
		sessions: []session.Metadata{{UUID: "r1"}},
		cloneErr: errors.New("agent disk full"),
	}
	r := &Router{Local: &fakeLocal{}, Agents: []AgentRef{
		{Record: agentclient.AgentRecord{ID: "A"}, Client: ag},
	}}
	_, _ = r.List() // populate routing

	_, err := r.Clone("r1", "copy")
	if err == nil || !strings.Contains(err.Error(), "agent disk full") {
		t.Errorf("expected agent error to propagate, got %v", err)
	}
}

func TestRouter_Clone_OrphanRejected(t *testing.T) {
	// Local-only session that isn't routed to any agent must be rejected
	// — the user is expected to migrate it to an agent first.
	r := &Router{Local: &fakeLocal{sessions: []session.Metadata{{UUID: "orphan"}}}}
	_, err := r.Clone("orphan", "copy")
	if err == nil || !strings.Contains(err.Error(), "orphan") {
		t.Errorf("expected orphan rejection, got %v", err)
	}
}
