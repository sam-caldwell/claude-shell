package agentserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// --- Header parsing --------------------------------------------------------

func TestReadAttachHeader_Valid(t *testing.T) {
	r := bytes.NewReader([]byte(`{"id":"abc","cols":120,"rows":40}` + "\n"))
	req, err := readAttachHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	if req.ID != "abc" || req.Cols != 120 || req.Rows != 40 {
		t.Errorf("unexpected: %+v", req)
	}
}

func TestReadAttachHeader_MalformedJSON(t *testing.T) {
	r := bytes.NewReader([]byte("not-json\n"))
	_, err := readAttachHeader(r)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadAttachHeader_MissingID(t *testing.T) {
	r := bytes.NewReader([]byte(`{"cols":80,"rows":24}` + "\n"))
	_, err := readAttachHeader(r)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "missing id") {
		t.Errorf("err = %v", err)
	}
}

// --- Attach byte pump using a fake target ---------------------------------

// fakeTarget is an AttachTarget backed by in-memory pipes so we can exercise
// the byte pump end-to-end without docker. Bytes written by the client
// should arrive at clientOut (the fake "container stdin"); bytes written to
// containerIn should be readable by the client side.
type fakeTarget struct {
	mu sync.Mutex

	startErr error
	sawID    string
	initCols uint16
	initRows uint16

	// Exposed so tests can drive both ends.
	toContainer   io.Writer
	fromContainer io.Reader

	// Internal: bytes from container-side are written here and read by the
	// io.Copy in HandleAttach via the "master" interface.
	master *duplex

	resizeCalls []resizeCall
	killed      atomic.Bool
	waited      chan struct{}
}

type resizeCall struct{ cols, rows uint16 }

// duplex is a pair of buffered pipes wrapped as an io.ReadWriteCloser,
// simulating a bidirectional PTY where one end is the container and the
// other is the master.
type duplex struct {
	in  *io.PipeReader // master reads data originating from the container
	out *io.PipeWriter // master writes data destined for the container
}

func (d *duplex) Read(b []byte) (int, error)  { return d.in.Read(b) }
func (d *duplex) Write(b []byte) (int, error) { return d.out.Write(b) }
func (d *duplex) Close() error {
	_ = d.in.Close()
	_ = d.out.Close()
	return nil
}

func newFakeTarget() (*fakeTarget, *io.PipeReader, *io.PipeWriter) {
	// Two pipes:
	//   clientToContainer — what the client writes flows out here
	//   containerToClient — what the container produces flows in here
	cOut, cIn := io.Pipe() // master <- container
	tIn, tOut := io.Pipe() // client -> container

	ft := &fakeTarget{
		master:        &duplex{in: cOut, out: tOut},
		fromContainer: tIn, // tests read this to see what client sent
		toContainer:   cIn, // tests write this to simulate container output
		waited:        make(chan struct{}),
	}
	return ft, tIn, cIn
}

func (f *fakeTarget) Start(ctx context.Context, id string, cols, rows uint16) (io.ReadWriteCloser, func(uint16, uint16), func() error, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, nil, nil, nil, f.startErr
	}
	f.sawID = id
	f.initCols = cols
	f.initRows = rows
	resize := func(c, r uint16) {
		f.mu.Lock()
		f.resizeCalls = append(f.resizeCalls, resizeCall{c, r})
		f.mu.Unlock()
	}
	wait := func() error {
		<-f.waited
		return nil
	}
	kill := func() {
		f.killed.Store(true)
		select {
		case <-f.waited:
		default:
			close(f.waited)
		}
	}
	return f.master, resize, wait, kill, nil
}

// --- Channel stand-in ------------------------------------------------------

// fakeChannel implements enough of ssh.Channel to drive HandleAttach without
// a real SSH session. In/Out are in-memory buffers / pipes exposed to tests.
type fakeChannel struct {
	// input: what HandleAttach reads as the client's bytes.
	in *io.PipeReader
	// output: what HandleAttach writes. Test reads here.
	out     *io.PipeWriter
	outRead *io.PipeReader

	closed atomic.Bool
	extraW *bytes.Buffer
}

func newFakeChannel() (*fakeChannel, *io.PipeWriter, *io.PipeReader) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return &fakeChannel{
		in:      inR,
		out:     outW,
		outRead: outR,
		extraW:  &bytes.Buffer{},
	}, inW, outR
}

func (c *fakeChannel) Read(b []byte) (int, error)  { return c.in.Read(b) }
func (c *fakeChannel) Write(b []byte) (int, error) { return c.out.Write(b) }
func (c *fakeChannel) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = c.in.Close()
	_ = c.out.Close()
	return nil
}
func (c *fakeChannel) CloseWrite() error { return c.out.Close() }
func (c *fakeChannel) SendRequest(_ string, _ bool, _ []byte) (bool, error) {
	return false, nil
}
func (c *fakeChannel) Stderr() io.ReadWriter { return c.extraW }

// windowChangePayload returns the 8-byte SSH window-change payload prefix
// (cols + rows uint32 big-endian); remaining fields are zero for this test.
func windowChangePayload(cols, rows uint32) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint32(b[0:4], cols)
	binary.BigEndian.PutUint32(b[4:8], rows)
	return b
}

func TestHandleAttach_HappyPath(t *testing.T) {
	ft, containerIn, clientOut := newFakeTarget()
	ch, clientIn, serverOut := newFakeChannel()

	reqs := make(chan *ssh.Request)

	go func() {
		HandleAttach(context.Background(), ch, reqs, ft, AttachHooks{})
	}()

	// Send header + some client bytes.
	go func() {
		_, _ = clientIn.Write([]byte(`{"id":"sess-1","cols":100,"rows":30}` + "\n"))
		_, _ = clientIn.Write([]byte("hello"))
		_ = clientIn.Close()
	}()

	// Agent side should eventually see the bytes.
	buf := make([]byte, 5)
	_, err := io.ReadFull(containerIn, buf)
	if err != nil {
		t.Fatalf("read from container stdin: %v", err)
	}
	if string(buf) != "hello" {
		t.Errorf("got %q, want 'hello'", buf)
	}

	// Now write container output and verify it reaches the client.
	go func() {
		_, _ = clientOut.Write([]byte("world"))
		_ = clientOut.Close()
	}()
	outBuf := make([]byte, 5)
	_, err = io.ReadFull(serverOut, outBuf)
	if err != nil {
		t.Fatalf("read from server out: %v", err)
	}
	if string(outBuf) != "world" {
		t.Errorf("got %q, want 'world'", outBuf)
	}

	// Terminate.
	close(reqs)

	// Let goroutines settle.
	time.Sleep(30 * time.Millisecond)
	if ft.sawID != "sess-1" {
		t.Errorf("target saw id %q, want 'sess-1'", ft.sawID)
	}
	if ft.initCols != 100 || ft.initRows != 30 {
		t.Errorf("initial size = %dx%d, want 100x30", ft.initCols, ft.initRows)
	}
}

func TestHandleAttach_WindowChange(t *testing.T) {
	ft, _, _ := newFakeTarget()
	ch, clientIn, _ := newFakeChannel()

	reqs := make(chan *ssh.Request, 1)
	go HandleAttach(context.Background(), ch, reqs, ft, AttachHooks{})

	_, _ = clientIn.Write([]byte(`{"id":"x","cols":80,"rows":24}` + "\n"))

	// Let HandleAttach enter the request loop.
	time.Sleep(20 * time.Millisecond)

	reqs <- &ssh.Request{Type: "window-change", Payload: windowChangePayload(132, 50)}
	time.Sleep(20 * time.Millisecond)

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.resizeCalls) == 0 {
		t.Fatal("no resize recorded")
	}
	last := ft.resizeCalls[len(ft.resizeCalls)-1]
	if last.cols != 132 || last.rows != 50 {
		t.Errorf("resize = %+v, want 132x50", last)
	}
}

func TestHandleAttach_BadHeader_SurfaceErrorToClient(t *testing.T) {
	ft, _, _ := newFakeTarget()
	ch, clientIn, serverOut := newFakeChannel()

	done := make(chan struct{})
	go func() {
		HandleAttach(context.Background(), ch, make(chan *ssh.Request), ft, AttachHooks{})
		close(done)
	}()

	_, _ = clientIn.Write([]byte("garbage\n"))
	_ = clientIn.Close()

	data, _ := io.ReadAll(serverOut)
	var resp Response
	if err := json.Unmarshal(bytes.TrimSpace(data), &resp); err != nil {
		t.Fatalf("decode response: %v (raw=%q)", err, data)
	}
	if resp.OK {
		t.Error("expected ok=false for bad header")
	}
	<-done
}

func TestHandleAttach_TargetStartError(t *testing.T) {
	ft, _, _ := newFakeTarget()
	ft.startErr = errors.New("no such session")
	ch, clientIn, serverOut := newFakeChannel()

	done := make(chan struct{})
	go func() {
		HandleAttach(context.Background(), ch, make(chan *ssh.Request), ft, AttachHooks{})
		close(done)
	}()

	_, _ = clientIn.Write([]byte(`{"id":"missing"}` + "\n"))
	_ = clientIn.Close()

	data, _ := io.ReadAll(serverOut)
	var resp Response
	_ = json.Unmarshal(bytes.TrimSpace(data), &resp)
	if resp.OK {
		t.Error("expected ok=false")
	}
	if !strings.Contains(resp.Error, "no such session") {
		t.Errorf("error = %q", resp.Error)
	}
	<-done
}

// --- DockerAttachTarget (unit tests that don't spawn docker) --------------

func TestDockerAttachTarget_ExistsGate(t *testing.T) {
	called := false
	tgt := &DockerAttachTarget{
		ExistsFn: func(id string) bool { called = true; return false },
	}
	_, _, _, _, err := tgt.Start(context.Background(), "who-dis", 80, 24)
	if err == nil {
		t.Fatal("expected error when ExistsFn rejects id")
	}
	if !called {
		t.Error("ExistsFn not consulted")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
}
