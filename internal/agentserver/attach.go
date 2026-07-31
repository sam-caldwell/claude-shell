package agentserver

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"

	"github.com/asymmetric-effort/convocate/internal/config"
)

// AttachSubsystem is the SSH subsystem name that selects pty-relay mode.
// When a client requests this subsystem the channel stops being a JSON-RPC
// stream and becomes a raw byte pipe to a specific container's tmux session.
const AttachSubsystem = "convocate-agent-attach"

// AttachTarget resolves an attach request to a container name, then runs
// the docker-exec that hooks its stdin/stdout to the returned PTY file. The
// interface is small on purpose: production uses DockerAttachTarget,
// tests substitute a pipe-based stand-in that doesn't need docker.
type AttachTarget interface {
	// Start launches docker exec against the session identified by sessionID,
	// with stdin/stdout/stderr attached to a freshly-allocated PTY. Initial
	// window size is set from cols/rows. Returns:
	//   - master: the host-side end of the PTY (read from docker, write to docker)
	//   - resize: function to change the window size (bound to the PTY fd)
	//   - wait:   blocks until docker exec exits, returns its error
	//   - kill:   kills the child process (used on client disconnect)
	Start(ctx context.Context, sessionID string, cols, rows uint16) (master io.ReadWriteCloser, resize func(cols, rows uint16), wait func() error, kill func(), err error)
}

// AttachRequest is the JSON header a client writes as the first line of an
// attach channel. cols/rows carry the initial terminal size; subsequent
// window-change SSH requests update it in flight.
type AttachRequest struct {
	ID   string `json:"id"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// AttachHooks let the server notify the orchestrator when a pty channel
// opens or closes so list/get can report Attached state. Nil fields are
// ignored — tests often don't care.
type AttachHooks struct {
	OnAttach func(sessionID string)
	OnDetach func(sessionID string)
}

// HandleAttach runs the attach subsystem handshake + byte pump on ch. The
// reqs channel provides SSH channel-level requests (window-change in
// particular); anything else is refused.
//
// Flow:
//  1. Read one line of JSON from ch — the AttachRequest.
//  2. Ask target.Start for a PTY bound to that container.
//  3. Bridge ch <-> pty until either side closes.
//  4. Forward window-change requests to the pty resize fn.
func HandleAttach(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, target AttachTarget, hooks AttachHooks) {
	defer ch.Close()

	req, err := readAttachHeader(ch)
	if err != nil {
		writeAttachError(ch, fmt.Sprintf("bad attach header: %v", err))
		return
	}
	if req.Cols == 0 {
		req.Cols = 80
	}
	if req.Rows == 0 {
		req.Rows = 24
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if hooks.OnAttach != nil {
		hooks.OnAttach(req.ID)
	}
	if hooks.OnDetach != nil {
		defer hooks.OnDetach(req.ID)
	}

	master, resize, wait, kill, err := target.Start(ctx, req.ID, req.Cols, req.Rows)
	if err != nil {
		writeAttachError(ch, fmt.Sprintf("attach: %v", err))
		return
	}
	defer kill()
	defer master.Close()

	// Channel-level request pump: forward window-change to the pty.
	go func() {
		for r := range reqs {
			if r.Type == "window-change" && len(r.Payload) >= 8 {
				cols := binary.BigEndian.Uint32(r.Payload[0:4])
				rows := binary.BigEndian.Uint32(r.Payload[4:8])
				resize(uint16(cols), uint16(rows))
			}
			_ = r.Reply(false, nil)
		}
	}()

	// Bidirectional copy. When either side finishes, cancel context so the
	// other goroutine unblocks via the underlying close.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(master, ch)
		// Client closed stdin; send EOF to the container but keep the
		// master open so we can still drain its output.
		if c, ok := master.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, master)
		// docker exec closed stdout — the attach is finished.
		cancel()
	}()

	waitErr := wait()
	_ = master.Close()
	wg.Wait()
	if waitErr != nil {
		writeAttachError(ch, fmt.Sprintf("exec exited: %v", waitErr))
	}
}

func readAttachHeader(r io.Reader) (AttachRequest, error) {
	// Single newline-terminated JSON line. Using bufio.Reader.ReadBytes('\n')
	// cleanly gives us the boundary between header and raw pty traffic.
	br := bufio.NewReader(r)
	line, err := br.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return AttachRequest{}, err
	}
	var req AttachRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return AttachRequest{}, fmt.Errorf("decode header: %w", err)
	}
	if req.ID == "" {
		return AttachRequest{}, fmt.Errorf("missing id")
	}
	return req, nil
}

func writeAttachError(w io.Writer, msg string) {
	// Keep the on-wire error format consistent with the RPC subsystem so
	// client code has one shape to parse.
	resp := Response{OK: false, Error: msg}
	_ = json.NewEncoder(w).Encode(resp)
}

// --- Production AttachTarget ----------------------------------------------

// DockerAttachTarget is the production AttachTarget: resolves the session
// UUID to a container name, runs
//
//	docker exec -it convocate-session-<uuid> sudo -u claude -- tmux attach-session -t claude
//
// in a local PTY, and returns the PTY master for byte relay.
//
// ExistsFn lets the target refuse attach for sessions the agent doesn't
// know about (returning a clear error before we even invoke docker).
type DockerAttachTarget struct {
	ExistsFn func(sessionID string) bool
}

// Start implements AttachTarget.
func (t *DockerAttachTarget) Start(ctx context.Context, sessionID string, cols, rows uint16) (io.ReadWriteCloser, func(uint16, uint16), func() error, func(), error) {
	if t.ExistsFn != nil && !t.ExistsFn(sessionID) {
		return nil, nil, nil, nil, fmt.Errorf("session %q not found", sessionID)
	}
	containerName := config.ContainerName(sessionID)
	cmd := exec.CommandContext(ctx, "docker", "exec", "-it",
		containerName,
		"sudo", "-E", "-u", config.ConvocateUser, "-H", "--",
		"tmux", "attach-session", "-t", config.TmuxSessionName,
	)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("pty start: %w", err)
	}
	resize := func(c, r uint16) {
		_ = pty.Setsize(f, &pty.Winsize{Cols: c, Rows: r})
	}
	wait := func() error { return cmd.Wait() }
	kill := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	return ptyRWC{f}, resize, wait, kill, nil
}

// ptyRWC adapts an *os.File from creack/pty to the ReadWriteCloser interface
// without exposing *os.File's extra methods to callers.
type ptyRWC struct{ f *os.File }

func (p ptyRWC) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p ptyRWC) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p ptyRWC) Close() error                { return p.f.Close() }
