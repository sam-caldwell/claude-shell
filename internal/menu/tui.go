package menu

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/asymmetric-effort/convocate/internal/session"
)

// loadAverageReader is overridable for tests.
var loadAverageReader = func() (string, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return "", false
	}
	return fmt.Sprintf("%s %s %s", fields[0], fields[1], fields[2]), true
}

var (
	titleStyle      = tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlue)
	menuBarStyle    = tcell.StyleDefault.Foreground(tcell.ColorYellow).Background(tcell.ColorBlue)
	headerStyle     = tcell.StyleDefault.Foreground(tcell.ColorYellow).Bold(true)
	normalStyle     = tcell.StyleDefault
	selectedStyle   = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorWhite)
	separatorStyle  = tcell.StyleDefault.Foreground(tcell.ColorGray)
	dialogStyle     = tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorDarkBlue)
	dialogErrStyle  = tcell.StyleDefault.Foreground(tcell.ColorRed).Background(tcell.ColorDarkBlue)
	dialogWarnStyle = tcell.StyleDefault.Foreground(tcell.ColorYellow).Background(tcell.ColorRed)
	inputStyle      = tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlack)
)

type tuiMode int

const (
	modeMenu tuiMode = iota
	modeCreateDialog
	modeCloneDialog
	modeDeleteConfirm
	modeLockedDialog
	modeOverrideConfirm
	modeKillConfirm
	modeNotRunningDialog
	modeKillInitiated
	modeBackgroundConfirm
	modeBackgroundInitiated
	modeNotConnectedDialog
	modeSettingsDialog
	modeRestartConfirm
	modeRestartInitiated
	modeAlreadyRunningDialog
	modeEditDialog
	modeEditRestartPrompt
	modeSelectAgent
	modeNoAgents
)

type tui struct {
	screen         tcell.Screen
	sessions       []session.Metadata
	cursor         int
	offset         int
	tickInterval   time.Duration
	reloadInterval time.Duration
	mode           tuiMode
	inputBuf       []rune
	inputBufPort   []rune
	inputBufDNS    []rune
	inputProtocol  string // "tcp" or "udp"
	activeField    int    // 0=Name, 1=Protocol, 2=Port, 3=DNSName
	dialogErr      string

	// agents + selectedAgent are the state for the "pick an agent to
	// host the new session" dialog. agents is populated from
	// DisplayOptions.Agents on each Display call; selectedAgent is the
	// index into that slice that's currently highlighted. Carried into
	// the Selection as AgentID when the Create dialog saves.
	agents        []string
	selectedAgent int
	chosenAgent   string // locked in after modeSelectAgent → modeCreateDialog

	isLockedFunc     func(id string) bool
	isRunningFunc    func(id string) bool
	reloadFunc       func() ([]session.Metadata, error)
	overrideLockFunc func(id string) error
	killFunc         func(id string) error
	backgroundFunc   func(id string) error
	restartFunc      func(id string) error
	editSaveFunc     func(id, name, protocol, dnsName string, port int) error
}

func (t *tui) isLocked(id string) bool {
	if t.isLockedFunc == nil {
		return false
	}
	return t.isLockedFunc(id)
}

func (t *tui) isRunning(id string) bool {
	if t.isRunningFunc == nil {
		return false
	}
	return t.isRunningFunc(id)
}

// isConnected reports whether a user terminal is currently attached to a
// running container for the given session. A connection requires both the
// container to be running and the session lock to be held (the lock is
// acquired for the duration of an active attach).
func (t *tui) isConnected(id string) bool {
	return t.isRunning(id) && t.isLocked(id)
}

// screenFactory creates and initializes a tcell.Screen. Override in tests.
var screenFactory func() (tcell.Screen, error) = func() (tcell.Screen, error) {
	screen, err := tcell.NewScreen()
	if err != nil {
		return nil, fmt.Errorf("failed to create screen: %w", err)
	}
	if err := screen.Init(); err != nil {
		return nil, fmt.Errorf("failed to init screen: %w", err)
	}
	return screen, nil
}

// DisplayOptions configures the TUI display.
type DisplayOptions struct {
	IsLocked          func(string) bool
	IsRunning         func(string) bool
	Reload            func() ([]session.Metadata, error)
	OverrideLock      func(string) error
	KillSession       func(string) error
	BackgroundSession func(string) error
	RestartSession    func(string) error
	// SaveSessionEdit persists edited session metadata fields to disk. It is
	// called with the session ID, the new name, the new protocol ("tcp" or
	// "udp"), the new DNS name (may be empty), and the new port.
	SaveSessionEdit func(id, name, protocol, dnsName string, port int) error

	// Agents is the list of registered convocate-agent IDs the user can
	// target when creating a new session. When empty, pressing 'N' shows
	// a "no agents registered" dialog instead of the create form.
	Agents []string
}

// Display renders the TUI session menu and returns the user's selection.
func Display(sessions []session.Metadata, opts DisplayOptions) (Selection, error) {
	screen, err := screenFactory()
	if err != nil {
		return Selection{}, err
	}
	defer screen.Fini()

	return DisplayWithScreen(sessions, screen, opts)
}

// DisplayWithScreen renders the TUI on a provided screen (for testing).
func DisplayWithScreen(sessions []session.Metadata, screen tcell.Screen, opts DisplayOptions) (Selection, error) {
	return displayWithOptions(sessions, screen, 1*time.Second, 15*time.Second, opts)
}

func displayWithOptions(sessions []session.Metadata, screen tcell.Screen, tickInterval, reloadInterval time.Duration, opts DisplayOptions) (Selection, error) {
	done := make(chan struct{})
	defer close(done)

	isLocked := opts.IsLocked
	if isLocked == nil {
		isLocked = func(string) bool { return false }
	}

	isRunning := opts.IsRunning
	if isRunning == nil {
		isRunning = func(string) bool { return false }
	}

	t := &tui{
		screen:           screen,
		sessions:         sessions,
		tickInterval:     tickInterval,
		reloadInterval:   reloadInterval,
		isLockedFunc:     isLocked,
		isRunningFunc:    isRunning,
		reloadFunc:       opts.Reload,
		overrideLockFunc: opts.OverrideLock,
		killFunc:         opts.KillSession,
		backgroundFunc:   opts.BackgroundSession,
		restartFunc:      opts.RestartSession,
		editSaveFunc:     opts.SaveSessionEdit,
		agents:           opts.Agents,
	}

	// Tick periodically to keep the clock updated
	go func() {
		ticker := time.NewTicker(t.tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				screen.PostEvent(tcell.NewEventInterrupt(nil))
			}
		}
	}()

	// Reload sessions periodically
	if t.reloadFunc != nil {
		go func() {
			ticker := time.NewTicker(t.reloadInterval)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					reloadFlag := true
					screen.PostEvent(tcell.NewEventInterrupt(reloadFlag))
				}
			}
		}()
	}

	return t.run()
}

func (t *tui) run() (Selection, error) {
	for {
		t.draw()
		t.screen.Show()

		ev := t.screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventKey:
			sel, done := t.handleKey(ev)
			if done {
				return sel, nil
			}
		case *tcell.EventResize:
			t.screen.Sync()
		case *tcell.EventInterrupt:
			if _, ok := ev.Data().(bool); ok && t.reloadFunc != nil {
				if sessions, err := t.reloadFunc(); err == nil {
					t.sessions = sessions
					if t.cursor >= len(t.sessions) {
						t.cursor = len(t.sessions) - 1
					}
					if t.cursor < 0 {
						t.cursor = 0
					}
				}
			}
		}
	}
}

func (t *tui) draw() {
	t.screen.Clear()
	width, height := t.screen.Size()
	if height < 4 || width < 20 {
		return
	}

	t.drawTitleBar(width)
	t.drawSessionTable(width, height)
	t.drawMenuBar(width, height)

	switch t.mode {
	case modeCreateDialog:
		t.drawCreateDialog(width, height)
	case modeCloneDialog:
		t.drawCloneDialog(width, height)
	case modeDeleteConfirm:
		t.drawDeleteDialog(width, height)
	case modeLockedDialog:
		t.drawLockedDialog(width, height)
	case modeOverrideConfirm:
		t.drawOverrideDialog(width, height)
	case modeKillConfirm:
		t.drawKillDialog(width, height)
	case modeNotRunningDialog:
		t.drawNotRunningDialog(width, height)
	case modeKillInitiated:
		t.drawKillInitiatedDialog(width, height)
	case modeBackgroundConfirm:
		t.drawBackgroundDialog(width, height)
	case modeBackgroundInitiated:
		t.drawBackgroundInitiatedDialog(width, height)
	case modeNotConnectedDialog:
		t.drawNotConnectedDialog(width, height)
	case modeSettingsDialog:
		t.drawSettingsDialog(width, height)
	case modeRestartConfirm:
		t.drawRestartDialog(width, height)
	case modeRestartInitiated:
		t.drawRestartInitiatedDialog(width, height)
	case modeAlreadyRunningDialog:
		t.drawAlreadyRunningDialog(width, height)
	case modeEditDialog:
		t.drawEditDialog(width, height)
	case modeEditRestartPrompt:
		t.drawEditRestartPromptDialog(width, height)
	case modeSelectAgent:
		t.drawSelectAgentDialog(width, height)
	case modeNoAgents:
		t.drawNoAgentsDialog(width, height)
	}
}

func (t *tui) drawTitleBar(width int) {
	fillRow(t.screen, 0, width, titleStyle)
	drawString(t.screen, 1, 0, "convocate", titleStyle)

	clock := time.Now().Format("2006-01-02 15:04:05")
	right := clock
	if load, ok := loadAverageReader(); ok {
		right = load + "  " + clock
	}
	x := width - len(right) - 1
	if x > 13 {
		drawString(t.screen, x, 0, right, titleStyle)
	}
}

func (t *tui) drawSessionTable(width, height int) {
	headerRow := 2
	sepRow := 3
	sessionsStart := 4
	menuBarRow := height - 1

	// Column header
	header := fmt.Sprintf("  %-4s| %-20s | %-36s | %-12s | %-13s | %-10s | %s",
		"#", "Name", "Session ID", "Created", "Last Accessed", "Port/Proto", "S")
	drawString(t.screen, 0, headerRow, clipToWidth(header, width), headerStyle)

	// Separator
	drawString(t.screen, 0, sepRow, clipToWidth(strings.Repeat("\u2500", width), width), separatorStyle)

	visibleRows := menuBarRow - sessionsStart
	if visibleRows < 1 {
		return
	}

	// Clamp cursor
	if len(t.sessions) > 0 {
		if t.cursor >= len(t.sessions) {
			t.cursor = len(t.sessions) - 1
		}
		if t.cursor < 0 {
			t.cursor = 0
		}
	}

	// Adjust scroll offset to keep cursor visible
	if t.cursor < t.offset {
		t.offset = t.cursor
	}
	if t.cursor >= t.offset+visibleRows {
		t.offset = t.cursor - visibleRows + 1
	}

	if len(t.sessions) == 0 {
		drawString(t.screen, 2, sessionsStart, "No sessions. Press (N) to create one.", normalStyle)
		return
	}

	for i := 0; i < visibleRows && i+t.offset < len(t.sessions); i++ {
		idx := i + t.offset
		s := t.sessions[idx]

		style := normalStyle
		if idx == t.cursor {
			style = selectedStyle
		}

		running := t.isRunning(s.UUID)
		locked := t.isLocked(s.UUID)
		// A session with no AgentID is a local orphan (pre-v2 leftover
		// whose container isn't managed by any registered agent). It
		// can't be attached / killed / restarted until the operator
		// runs `convocate-host migrate-session`. The O marker makes this
		// visually distinct from remote sessions so "Enter does
		// nothing" isn't a surprise.
		orphan := s.AgentID == "" && s.UUID != ""
		statusIndicator := "-"
		switch {
		case orphan:
			statusIndicator = "O"
		case running && locked:
			statusIndicator = "C"
		case running:
			statusIndicator = "R"
		case locked:
			statusIndicator = "L"
		}

		portLabel := "-"
		if s.Port > 0 {
			portLabel = fmt.Sprintf("%d/%s", s.Port, s.EffectiveProtocol())
		}
		line := fmt.Sprintf("  %-4s| %-20s | %s | %-12s | %-13s | %-10s | %s",
			strconv.Itoa(idx+1),
			truncate(s.Name, 20),
			s.UUID,
			s.CreatedAt.Format("2006-01-02"),
			s.LastAccessed.Format("2006-01-02"),
			portLabel,
			statusIndicator,
		)

		row := sessionsStart + i
		fillRow(t.screen, row, width, style)
		drawString(t.screen, 0, row, clipToWidth(line, width), style)
	}
}

func (t *tui) drawMenuBar(width, height int) {
	row := height - 1
	fillRow(t.screen, row, width, menuBarStyle)
	drawString(t.screen, 1, row,
		"(N)ew | (C)lone | (E)dit | (B)ackground | (D)elete | (K)ill | (O)verride | (S)ettings | (R)estart | (Q)uit",
		menuBarStyle)
}

func (t *tui) drawCreateDialog(width, height int) {
	title := "Create New Session"
	if t.chosenAgent != "" {
		title = "Create on agent " + t.chosenAgent
	}
	t.drawSessionFormDialog(width, height, title)
}

// startCreateDialog transitions from the agent picker (or the single-agent
// shortcut) into the form, resetting inputs. chosenAgent must already be
// populated by the caller.
func (t *tui) startCreateDialog() {
	t.mode = modeCreateDialog
	t.inputBuf = nil
	t.inputBufPort = nil
	t.inputBufDNS = nil
	t.inputProtocol = session.ProtocolTCP
	t.activeField = 0
	t.dialogErr = ""
}

// drawSelectAgentDialog renders a compact picker showing every registered
// convocate-agent. Up/Down moves the highlight, Enter commits the choice,
// Esc cancels back to the menu.
func (t *tui) drawSelectAgentDialog(width, height int) {
	const minW = 48
	dialogWidth := minW
	// Widen for long agent IDs.
	for _, id := range t.agents {
		if n := len(id) + 6; n > dialogWidth {
			dialogWidth = n
		}
	}
	if dialogWidth > width-2 {
		dialogWidth = width - 2
	}
	// Height: title + blank + N agent rows + blank + hint.
	rows := len(t.agents)
	if rows < 1 {
		rows = 1
	}
	dialogHeight := 4 + rows

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}
	title := " Select Agent for New Session "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	for i, id := range t.agents {
		style := dialogStyle
		prefix := "  "
		if i == t.selectedAgent {
			style = dialogStyle.Reverse(true)
			prefix = "> "
		}
		drawString(t.screen, x0+2, y0+2+i, prefix+id, style)
	}
	hint := "Up/Down=Move  Enter=Select  Esc=Cancel"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

// drawNoAgentsDialog warns that no convocate-agents are registered — session
// creation requires one.
func (t *tui) drawNoAgentsDialog(width, height int) {
	msg := "No convocate-agent hosts are registered."
	hint := "Run 'convocate-host init-agent' to add one. Press any key."
	dialogWidth := len(hint) + 4
	dialogHeight := 5

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	for row := y0; row < y0+dialogHeight; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}
	drawString(t.screen, x0+(dialogWidth-len(" No Agents "))/2, y0, " No Agents ", dialogStyle.Bold(true))
	drawString(t.screen, x0+2, y0+2, msg, dialogErrStyle)
	drawString(t.screen, x0+2, y0+4, hint, dialogStyle)
}

// handleSelectAgentKey is the key handler for modeSelectAgent. Returns
// (selection, done) — selection is ignored by callers; done=true only
// when the user Esc'd out (Quit is never returned here because this
// dialog is purely navigational).
func (t *tui) handleSelectAgentKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyUp:
		if t.selectedAgent > 0 {
			t.selectedAgent--
		}
	case tcell.KeyDown:
		if t.selectedAgent < len(t.agents)-1 {
			t.selectedAgent++
		}
	case tcell.KeyEnter:
		if len(t.agents) == 0 {
			t.mode = modeMenu
			return Selection{}, false
		}
		t.chosenAgent = t.agents[t.selectedAgent]
		t.startCreateDialog()
	case tcell.KeyEscape:
		t.mode = modeMenu
	}
	return Selection{}, false
}

// handleNoAgentsKey dismisses the "no agents" notice on any key.
func (t *tui) handleNoAgentsKey(_ *tcell.EventKey) (Selection, bool) {
	t.mode = modeMenu
	return Selection{}, false
}

// drawSessionFormDialog renders the shared session form used for both create
// and edit flows. The only visible difference between the two is the title.
// Layout: Name, Protocol, Port, DNS Name + wrapped error + hint row.
func (t *tui) drawSessionFormDialog(width, height int, titleText string) {
	dialogWidth := sizeDialogForError(68, 68, t.dialogErr, width)
	errLines := wrapErrorText(t.dialogErr, dialogWidth-4)
	// rows: title(0), blank(1), name(2), blank(3), protocol(4), blank(5),
	// port(6), blank(7), dns(8), blank(9), errors..., blank, hint
	baseHeight := 12
	dialogHeight := baseHeight
	if len(errLines) > 0 {
		dialogHeight = baseHeight - 1 + len(errLines) + 1
	}

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2

	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	title := " " + titleText + " "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	// Name field
	drawString(t.screen, x0+2, y0+2, "Name:", dialogStyle)
	nameX := x0 + 12
	nameW := dialogWidth - 14
	t.drawInputField(nameX, y0+2, nameW, t.inputBuf, t.activeField == 0)

	// Protocol field (toggle). Use a fixed-width input-style slot showing the
	// current value; focus is indicated the same way as editable fields.
	drawString(t.screen, x0+2, y0+4, "Protocol:", dialogStyle)
	protoX := x0 + 12
	protoW := 5 // "tcp" / "udp" both fit in <=5 chars
	t.drawInputField(protoX, y0+4, protoW, []rune(t.inputProtocol), t.activeField == 1)
	drawString(t.screen, protoX+protoW+2, y0+4, "(t=tcp, u=udp, Space toggles)", dialogStyle)

	// Port field
	drawString(t.screen, x0+2, y0+6, "Port:", dialogStyle)
	portX := x0 + 12
	portW := 7
	t.drawInputField(portX, y0+6, portW, t.inputBufPort, t.activeField == 2)
	drawString(t.screen, portX+portW+2, y0+6, "(blank=none, 0=auto)", dialogStyle)

	// DNS Name field (optional)
	drawString(t.screen, x0+2, y0+8, "DNS Name:", dialogStyle)
	dnsX := x0 + 12
	dnsW := dialogWidth - 14
	t.drawInputField(dnsX, y0+8, dnsW, t.inputBufDNS, t.activeField == 3)

	// Error message (wrapped)
	for i, line := range errLines {
		drawString(t.screen, x0+2, y0+10+i, line, dialogErrStyle)
	}

	hint := "Tab=Next  Enter=Save  Esc=Cancel"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

// drawInputField renders an input field with its text and a cursor when
// focused. The field visually spans width cells starting at (x, y).
func (t *tui) drawInputField(x, y, width int, buf []rune, focused bool) {
	for col := x; col < x+width; col++ {
		t.screen.SetContent(col, y, ' ', nil, inputStyle)
	}
	text := string(buf)
	if len(text) > width {
		text = text[len(text)-width:]
	}
	drawString(t.screen, x, y, text, inputStyle)
	if !focused {
		return
	}
	cursorX := x + len(buf)
	if len(buf) > width {
		cursorX = x + width
	}
	t.screen.SetContent(cursorX, y, ' ', nil, inputStyle.Reverse(true))
}

func (t *tui) drawCloneDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	dialogWidth := sizeDialogForError(60, 60, t.dialogErr, width)
	errLines := wrapErrorText(t.dialogErr, dialogWidth-4)
	dialogHeight := 7
	if len(errLines) > 0 {
		dialogHeight = 5 + len(errLines) + 2
	}

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	title := " Clone Session "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	source := fmt.Sprintf("From: %s", truncate(s.Name, dialogWidth-10))
	drawString(t.screen, x0+2, y0+2, source, dialogStyle)

	drawString(t.screen, x0+2, y0+3, "Name:", dialogStyle)

	inputX := x0 + 8
	inputW := dialogWidth - 10
	for col := inputX; col < inputX+inputW; col++ {
		t.screen.SetContent(col, y0+3, ' ', nil, inputStyle)
	}

	inputText := string(t.inputBuf)
	if len(inputText) > inputW {
		inputText = inputText[len(inputText)-inputW:]
	}
	drawString(t.screen, inputX, y0+3, inputText, inputStyle)

	cursorX := inputX + len(t.inputBuf)
	if len(t.inputBuf) > inputW {
		cursorX = inputX + inputW
	}
	if cursorX < x0+dialogWidth-2 {
		t.screen.SetContent(cursorX, y0+3, ' ', nil, inputStyle.Reverse(true))
	}

	for i, line := range errLines {
		drawString(t.screen, x0+2, y0+5+i, line, dialogErrStyle)
	}

	hint := "Enter=Clone  Esc=Cancel"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) drawDeleteDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	prompt := fmt.Sprintf("Delete session %q?", name)
	dialogWidth := len(prompt) + 6
	if dialogWidth < 36 {
		dialogWidth = 36
	}
	const dialogHeight = 5

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	// Draw dialog background
	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	// Title
	title := " Confirm Delete "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	// Prompt
	drawString(t.screen, x0+2, y0+2, prompt, dialogStyle)

	// Hint
	hint := "(Y)es  (N)o"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) drawLockedDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	msg := fmt.Sprintf("Session %q is currently locked.", name)
	dialogWidth := len(msg) + 6
	if dialogWidth < 40 {
		dialogWidth = 40
	}
	const dialogHeight = 5

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	// Draw dialog background
	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	// Title
	title := " Session Locked "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	// Message
	drawString(t.screen, x0+2, y0+2, msg, dialogStyle)

	// Hint
	hint := "Press any key to continue"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleLockedDialogKey(ev *tcell.EventKey) (Selection, bool) {
	t.mode = modeMenu
	return Selection{}, false
}

func (t *tui) drawNotRunningDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	msg := fmt.Sprintf("Session %q is not running.", name)
	dialogWidth := len(msg) + 6
	if dialogWidth < 40 {
		dialogWidth = 40
	}
	const dialogHeight = 5

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	// Draw dialog background (red)
	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogWarnStyle)
		}
	}

	// Title
	title := " Not Running "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogWarnStyle.Bold(true))

	// Message
	drawString(t.screen, x0+2, y0+2, msg, dialogWarnStyle)

	// Hint
	hint := "Press any key to continue"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogWarnStyle)
}

func (t *tui) handleNotRunningDialogKey(ev *tcell.EventKey) (Selection, bool) {
	t.mode = modeMenu
	return Selection{}, false
}

func (t *tui) drawKillInitiatedDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	msg := fmt.Sprintf("Kill initiated for session %q.", name)
	dialogWidth := len(msg) + 6
	if dialogWidth < 40 {
		dialogWidth = 40
	}
	const dialogHeight = 5

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	// Draw dialog background
	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	// Title
	title := " Kill Initiated "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	// Message
	drawString(t.screen, x0+2, y0+2, msg, dialogStyle)

	// Hint
	hint := "Press any key to continue"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleKillInitiatedDialogKey(ev *tcell.EventKey) (Selection, bool) {
	t.mode = modeMenu
	return Selection{}, false
}

func (t *tui) drawBackgroundDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	prompt := fmt.Sprintf("Background session %q?", name)
	dialogWidth := sizeDialogForError(len(prompt)+6, 44, t.dialogErr, width)
	errLines := wrapErrorText(t.dialogErr, dialogWidth-4)
	dialogHeight := 5
	if len(errLines) > 0 {
		dialogHeight = 4 + len(errLines) + 2
	}

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	title := " Background Session "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	drawString(t.screen, x0+2, y0+2, prompt, dialogStyle)

	for i, line := range errLines {
		drawString(t.screen, x0+2, y0+4+i, line, dialogErrStyle)
	}

	hint := "(Y)es  (N)o"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleBackgroundDialogKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		t.mode = modeMenu
		t.dialogErr = ""
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'y', 'Y':
			if t.cursor < 0 || t.cursor >= len(t.sessions) {
				t.mode = modeMenu
				return Selection{}, false
			}
			s := t.sessions[t.cursor]
			if !t.isConnected(s.UUID) {
				t.mode = modeNotConnectedDialog
				t.dialogErr = ""
				return Selection{}, false
			}
			if t.backgroundFunc != nil {
				if err := t.backgroundFunc(s.UUID); err != nil {
					t.dialogErr = err.Error()
					return Selection{}, false
				}
			}
			t.mode = modeBackgroundInitiated
			t.dialogErr = ""
		case 'n', 'N':
			t.mode = modeMenu
			t.dialogErr = ""
		}
	}
	return Selection{}, false
}

func (t *tui) drawBackgroundInitiatedDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	msg := fmt.Sprintf("Session %q backgrounded.", name)
	dialogWidth := len(msg) + 6
	if dialogWidth < 44 {
		dialogWidth = 44
	}
	const dialogHeight = 5

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	title := " Backgrounded "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	drawString(t.screen, x0+2, y0+2, msg, dialogStyle)

	hint := "Press any key to continue"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleBackgroundInitiatedDialogKey(ev *tcell.EventKey) (Selection, bool) {
	t.mode = modeMenu
	return Selection{}, false
}

func (t *tui) drawNotConnectedDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	msg := fmt.Sprintf("Session %q is not connected.", name)
	dialogWidth := len(msg) + 6
	if dialogWidth < 44 {
		dialogWidth = 44
	}
	const dialogHeight = 5

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogWarnStyle)
		}
	}

	title := " Not Connected "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogWarnStyle.Bold(true))

	drawString(t.screen, x0+2, y0+2, msg, dialogWarnStyle)

	hint := "Press any key to continue"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogWarnStyle)
}

func (t *tui) handleNotConnectedDialogKey(ev *tcell.EventKey) (Selection, bool) {
	t.mode = modeMenu
	return Selection{}, false
}

func (t *tui) drawRestartDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	prompt := fmt.Sprintf("Restart session %q in background?", name)
	dialogWidth := sizeDialogForError(len(prompt)+6, 48, t.dialogErr, width)
	errLines := wrapErrorText(t.dialogErr, dialogWidth-4)
	dialogHeight := 5
	if len(errLines) > 0 {
		dialogHeight = 4 + len(errLines) + 2 // prompt, gap, err..., gap, hint
	}

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	title := " Restart Session "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	drawString(t.screen, x0+2, y0+2, prompt, dialogStyle)

	for i, line := range errLines {
		drawString(t.screen, x0+2, y0+4+i, line, dialogErrStyle)
	}

	hint := "(Y)es  (N)o"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleRestartDialogKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		t.mode = modeMenu
		t.dialogErr = ""
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'y', 'Y':
			if t.cursor < 0 || t.cursor >= len(t.sessions) {
				t.mode = modeMenu
				return Selection{}, false
			}
			s := t.sessions[t.cursor]
			if t.restartFunc != nil {
				if err := t.restartFunc(s.UUID); err != nil {
					t.dialogErr = err.Error()
					return Selection{}, false
				}
			}
			t.mode = modeRestartInitiated
			t.dialogErr = ""
		case 'n', 'N':
			t.mode = modeMenu
			t.dialogErr = ""
		}
	}
	return Selection{}, false
}

func (t *tui) drawRestartInitiatedDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	msg := fmt.Sprintf("Restarting session %q in the background...", name)
	dialogWidth := len(msg) + 6
	if dialogWidth < 48 {
		dialogWidth = 48
	}
	const dialogHeight = 5

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	title := " Restarting "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	drawString(t.screen, x0+2, y0+2, msg, dialogStyle)

	hint := "Press Enter to continue"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleRestartInitiatedDialogKey(ev *tcell.EventKey) (Selection, bool) {
	if ev.Key() == tcell.KeyEnter {
		t.mode = modeMenu
	}
	return Selection{}, false
}

func (t *tui) drawAlreadyRunningDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	msg := fmt.Sprintf("Session %q is already running.", name)
	dialogWidth := len(msg) + 6
	if dialogWidth < 48 {
		dialogWidth = 48
	}
	const dialogHeight = 6

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	title := " Already Running "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	drawString(t.screen, x0+2, y0+2, msg, dialogStyle)
	drawString(t.screen, x0+2, y0+3, "Press Enter on it to connect.", dialogStyle)

	hint := "Press any key to continue"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleAlreadyRunningDialogKey(ev *tcell.EventKey) (Selection, bool) {
	t.mode = modeMenu
	return Selection{}, false
}

func (t *tui) drawEditDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	t.drawSessionFormDialog(width, height, "Edit Session")
}

func (t *tui) handleEditDialogKey(ev *tcell.EventKey) (Selection, bool) {
	return t.handleSessionFormKey(ev, true)
}

func (t *tui) drawEditRestartPromptDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	prompt := fmt.Sprintf("Saved. Restart session %q now?", name)
	dialogWidth := sizeDialogForError(len(prompt)+6, 52, t.dialogErr, width)
	errLines := wrapErrorText(t.dialogErr, dialogWidth-4)
	dialogHeight := 6
	if len(errLines) > 0 {
		dialogHeight = 5 + len(errLines) + 2
	}

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	title := " Restart Session "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	drawString(t.screen, x0+2, y0+2, prompt, dialogStyle)
	drawString(t.screen, x0+2, y0+3, "Y = restart now   N = keep config, don't restart", dialogStyle)

	for i, line := range errLines {
		drawString(t.screen, x0+2, y0+5+i, line, dialogErrStyle)
	}

	hint := "(Y)es  (N)o"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleEditRestartPromptKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		t.mode = modeMenu
		t.dialogErr = ""
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'y', 'Y':
			if t.cursor < 0 || t.cursor >= len(t.sessions) {
				t.mode = modeMenu
				return Selection{}, false
			}
			s := t.sessions[t.cursor]
			if t.restartFunc != nil {
				if err := t.restartFunc(s.UUID); err != nil {
					t.dialogErr = err.Error()
					return Selection{}, false
				}
			}
			t.mode = modeMenu
			t.dialogErr = ""
		case 'n', 'N':
			t.mode = modeMenu
			t.dialogErr = ""
		}
	}
	return Selection{}, false
}

func (t *tui) drawSettingsDialog(width, height int) {
	const dialogWidth = 64
	const dialogHeight = 9

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	title := " Settings "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	drawString(t.screen, x0+2, y0+2, "No settings are configurable at this time.", dialogStyle)
	drawString(t.screen, x0+2, y0+3, "Future options will appear here.", dialogStyle)

	hint := "Tab=Next  Shift+Tab=Prev  Enter=Save  Esc=Cancel"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleSettingsDialogKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		t.mode = modeMenu
		t.dialogErr = ""
	case tcell.KeyEnter:
		t.mode = modeMenu
		t.dialogErr = ""
	case tcell.KeyTab, tcell.KeyBacktab:
		// No fields to cycle yet; navigation is a no-op until settings are added.
	}
	return Selection{}, false
}

func (t *tui) drawOverrideDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	prompt := fmt.Sprintf("Override lock for session %q?", name)
	dialogWidth := sizeDialogForError(len(prompt)+6, 44, t.dialogErr, width)
	errLines := wrapErrorText(t.dialogErr, dialogWidth-4)
	dialogHeight := 5
	if len(errLines) > 0 {
		dialogHeight = 4 + len(errLines) + 2
	}

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	// Draw dialog background
	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	// Title
	title := " Override Lock "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	// Prompt
	drawString(t.screen, x0+2, y0+2, prompt, dialogStyle)

	// Error message (wrapped)
	for i, line := range errLines {
		drawString(t.screen, x0+2, y0+4+i, line, dialogErrStyle)
	}

	// Hint
	hint := "(Y)es  (N)o"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleOverrideDialogKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		t.mode = modeMenu
		t.dialogErr = ""
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'y', 'Y':
			if t.cursor >= 0 && t.cursor < len(t.sessions) && t.overrideLockFunc != nil {
				s := t.sessions[t.cursor]
				if err := t.overrideLockFunc(s.UUID); err != nil {
					t.dialogErr = err.Error()
					return Selection{}, false
				}
				t.mode = modeMenu
				t.dialogErr = ""
			} else {
				t.mode = modeMenu
			}
		case 'n', 'N':
			t.mode = modeMenu
			t.dialogErr = ""
		}
	}
	return Selection{}, false
}

func (t *tui) drawKillDialog(width, height int) {
	if t.cursor < 0 || t.cursor >= len(t.sessions) {
		return
	}
	s := t.sessions[t.cursor]

	name := truncate(s.Name, 30)
	prompt := fmt.Sprintf("Kill session %q?", name)
	dialogWidth := sizeDialogForError(len(prompt)+6, 40, t.dialogErr, width)
	errLines := wrapErrorText(t.dialogErr, dialogWidth-4)
	dialogHeight := 5
	if len(errLines) > 0 {
		dialogHeight = 4 + len(errLines) + 2
	}

	x0 := (width - dialogWidth) / 2
	y0 := (height - dialogHeight) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}

	// Draw dialog background
	for row := y0; row < y0+dialogHeight && row < height; row++ {
		for col := x0; col < x0+dialogWidth && col < width; col++ {
			t.screen.SetContent(col, row, ' ', nil, dialogStyle)
		}
	}

	// Title
	title := " Kill Session "
	drawString(t.screen, x0+(dialogWidth-len(title))/2, y0, title, dialogStyle.Bold(true))

	// Prompt
	drawString(t.screen, x0+2, y0+2, prompt, dialogStyle)

	// Error message (wrapped)
	for i, line := range errLines {
		drawString(t.screen, x0+2, y0+4+i, line, dialogErrStyle)
	}

	// Hint
	hint := "(Y)es  (N)o"
	drawString(t.screen, x0+(dialogWidth-len(hint))/2, y0+dialogHeight-1, hint, dialogStyle)
}

func (t *tui) handleKillDialogKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		t.mode = modeMenu
		t.dialogErr = ""
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'y', 'Y':
			if t.cursor >= 0 && t.cursor < len(t.sessions) {
				s := t.sessions[t.cursor]
				if !t.isRunning(s.UUID) {
					t.mode = modeNotRunningDialog
					t.dialogErr = ""
					return Selection{}, false
				}
				if t.killFunc != nil {
					if err := t.killFunc(s.UUID); err != nil {
						t.dialogErr = err.Error()
						return Selection{}, false
					}
				}
				t.mode = modeKillInitiated
				t.dialogErr = ""
			} else {
				t.mode = modeMenu
			}
		case 'n', 'N':
			t.mode = modeMenu
			t.dialogErr = ""
		}
	}
	return Selection{}, false
}

func (t *tui) handleDeleteDialogKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		t.mode = modeMenu
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'y', 'Y':
			if t.cursor >= 0 && t.cursor < len(t.sessions) {
				s := t.sessions[t.cursor]
				return Selection{Action: ActionDeleteSession, SessionID: s.UUID, Name: s.Name}, true
			}
			t.mode = modeMenu
		case 'n', 'N':
			t.mode = modeMenu
		}
	}
	return Selection{}, false
}

func (t *tui) handleKey(ev *tcell.EventKey) (Selection, bool) {
	switch t.mode {
	case modeCreateDialog:
		return t.handleCreateDialogKey(ev)
	case modeCloneDialog:
		return t.handleCloneDialogKey(ev)
	case modeDeleteConfirm:
		return t.handleDeleteDialogKey(ev)
	case modeLockedDialog:
		return t.handleLockedDialogKey(ev)
	case modeOverrideConfirm:
		return t.handleOverrideDialogKey(ev)
	case modeKillConfirm:
		return t.handleKillDialogKey(ev)
	case modeNotRunningDialog:
		return t.handleNotRunningDialogKey(ev)
	case modeKillInitiated:
		return t.handleKillInitiatedDialogKey(ev)
	case modeBackgroundConfirm:
		return t.handleBackgroundDialogKey(ev)
	case modeBackgroundInitiated:
		return t.handleBackgroundInitiatedDialogKey(ev)
	case modeNotConnectedDialog:
		return t.handleNotConnectedDialogKey(ev)
	case modeSettingsDialog:
		return t.handleSettingsDialogKey(ev)
	case modeRestartConfirm:
		return t.handleRestartDialogKey(ev)
	case modeRestartInitiated:
		return t.handleRestartInitiatedDialogKey(ev)
	case modeAlreadyRunningDialog:
		return t.handleAlreadyRunningDialogKey(ev)
	case modeEditDialog:
		return t.handleEditDialogKey(ev)
	case modeEditRestartPrompt:
		return t.handleEditRestartPromptKey(ev)
	case modeSelectAgent:
		return t.handleSelectAgentKey(ev)
	case modeNoAgents:
		return t.handleNoAgentsKey(ev)
	default:
		return t.handleMenuKey(ev)
	}
}

func (t *tui) handleMenuKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyUp:
		if t.cursor > 0 {
			t.cursor--
		}
	case tcell.KeyDown:
		if t.cursor < len(t.sessions)-1 {
			t.cursor++
		}
	case tcell.KeyEnter:
		if len(t.sessions) > 0 && t.cursor >= 0 && t.cursor < len(t.sessions) {
			s := t.sessions[t.cursor]
			if t.isLocked(s.UUID) {
				t.mode = modeLockedDialog
				return Selection{}, false
			}
			// Remote attach against a stopped container would dump the
			// agent's JSON error onto stdout. Intercept here and show
			// the "not running" dialog instead — operator presses (R)
			// afterward to restart.
			if s.IsRemote() && !t.isRunning(s.UUID) {
				t.mode = modeNotRunningDialog
				return Selection{}, false
			}
			return Selection{Action: s.UUID, SessionID: s.UUID}, true
		}
	case tcell.KeyEscape:
		return Selection{Action: ActionQuit}, true
	case tcell.KeyRune:
		switch ev.Rune() {
		case 'n', 'N':
			// Route to the agent picker first; the create form only
			// opens once we know which host will run the session.
			switch len(t.agents) {
			case 0:
				t.mode = modeNoAgents
			case 1:
				t.chosenAgent = t.agents[0]
				t.startCreateDialog()
			default:
				t.mode = modeSelectAgent
				t.selectedAgent = 0
			}
		case 'c', 'C':
			if len(t.sessions) > 0 && t.cursor >= 0 && t.cursor < len(t.sessions) {
				t.mode = modeCloneDialog
				t.inputBuf = nil
				t.dialogErr = ""
			}
		case 'd', 'D':
			if len(t.sessions) > 0 && t.cursor >= 0 && t.cursor < len(t.sessions) {
				t.mode = modeDeleteConfirm
			}
		case 'o', 'O':
			if len(t.sessions) > 0 && t.cursor >= 0 && t.cursor < len(t.sessions) {
				s := t.sessions[t.cursor]
				if t.isLocked(s.UUID) {
					t.mode = modeOverrideConfirm
					t.dialogErr = ""
				}
			}
		case 'e', 'E':
			if len(t.sessions) > 0 && t.cursor >= 0 && t.cursor < len(t.sessions) {
				s := t.sessions[t.cursor]
				t.mode = modeEditDialog
				t.inputBuf = []rune(s.Name)
				if s.Port > 0 {
					t.inputBufPort = []rune(strconv.Itoa(s.Port))
				} else {
					t.inputBufPort = nil
				}
				t.inputProtocol = s.EffectiveProtocol()
				t.inputBufDNS = []rune(s.DNSName)
				t.activeField = 0
				t.dialogErr = ""
			}
		case 'k', 'K':
			if len(t.sessions) > 0 && t.cursor >= 0 && t.cursor < len(t.sessions) {
				t.mode = modeKillConfirm
				t.dialogErr = ""
			}
		case 'b', 'B':
			if len(t.sessions) > 0 && t.cursor >= 0 && t.cursor < len(t.sessions) {
				t.mode = modeBackgroundConfirm
				t.dialogErr = ""
			}
		case 's', 'S':
			t.mode = modeSettingsDialog
			t.dialogErr = ""
		case 'r', 'R':
			if len(t.sessions) > 0 && t.cursor >= 0 && t.cursor < len(t.sessions) {
				s := t.sessions[t.cursor]
				if t.isRunning(s.UUID) {
					t.mode = modeAlreadyRunningDialog
					t.dialogErr = ""
					return Selection{}, false
				}
				t.mode = modeRestartConfirm
				t.dialogErr = ""
			}
		case 'q', 'Q':
			return Selection{Action: ActionQuit}, true
		default:
			if ev.Rune() >= '1' && ev.Rune() <= '9' {
				idx := int(ev.Rune() - '1')
				if idx < len(t.sessions) {
					s := t.sessions[idx]
					if t.isLocked(s.UUID) {
						t.cursor = idx
						t.mode = modeLockedDialog
						return Selection{}, false
					}
					return Selection{Action: s.UUID, SessionID: s.UUID}, true
				}
			}
		}
	}
	return Selection{}, false
}

func (t *tui) handleCreateDialogKey(ev *tcell.EventKey) (Selection, bool) {
	return t.handleSessionFormKey(ev, false)
}

// handleSessionFormKey drives both the create and edit dialogs. Fields are
// indexed: Name=0, Protocol=1, Port=2, DNSName=3. On Enter, the form is
// validated and either a Selection (for Create) or a call to editSaveFunc
// (for Edit) is emitted.
func (t *tui) handleSessionFormKey(ev *tcell.EventKey, isEdit bool) (Selection, bool) {
	const numFields = 4
	resetForm := func() {
		t.inputBuf = nil
		t.inputBufPort = nil
		t.inputBufDNS = nil
		t.inputProtocol = ""
		t.activeField = 0
		t.dialogErr = ""
	}
	switch ev.Key() {
	case tcell.KeyEscape:
		t.mode = modeMenu
		resetForm()
		return Selection{}, false
	case tcell.KeyTab:
		t.activeField = (t.activeField + 1) % numFields
		t.dialogErr = ""
		return Selection{}, false
	case tcell.KeyBacktab:
		t.activeField = (t.activeField + numFields - 1) % numFields
		t.dialogErr = ""
		return Selection{}, false
	case tcell.KeyEnter:
		name := strings.TrimSpace(string(t.inputBuf))
		if err := session.ValidateName(name); err != nil {
			t.dialogErr = err.Error()
			t.activeField = 0
			return Selection{}, false
		}
		proto, err := session.ValidateProtocol(t.inputProtocol)
		if err != nil {
			t.dialogErr = err.Error()
			t.activeField = 1
			return Selection{}, false
		}
		port, err := parsePortInput(string(t.inputBufPort))
		if err != nil {
			t.dialogErr = err.Error()
			t.activeField = 2
			return Selection{}, false
		}
		dnsName, err := session.ValidateDNSName(string(t.inputBufDNS))
		if err != nil {
			t.dialogErr = err.Error()
			t.activeField = 3
			return Selection{}, false
		}
		if isEdit {
			if t.cursor < 0 || t.cursor >= len(t.sessions) {
				t.mode = modeMenu
				return Selection{}, false
			}
			s := t.sessions[t.cursor]
			if t.editSaveFunc != nil {
				if err := t.editSaveFunc(s.UUID, name, proto, dnsName, port); err != nil {
					t.dialogErr = err.Error()
					return Selection{}, false
				}
			}
			t.mode = modeEditRestartPrompt
			t.dialogErr = ""
			return Selection{}, false
		}
		return Selection{Action: ActionNewSession, Name: name, Port: port, Protocol: proto, DNSName: dnsName, AgentID: t.chosenAgent}, true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		switch t.activeField {
		case 0:
			if len(t.inputBuf) > 0 {
				t.inputBuf = t.inputBuf[:len(t.inputBuf)-1]
				t.dialogErr = ""
			}
		case 2:
			if len(t.inputBufPort) > 0 {
				t.inputBufPort = t.inputBufPort[:len(t.inputBufPort)-1]
				t.dialogErr = ""
			}
		case 3:
			if len(t.inputBufDNS) > 0 {
				t.inputBufDNS = t.inputBufDNS[:len(t.inputBufDNS)-1]
				t.dialogErr = ""
			}
			// Protocol field (case 1) has no backspace — it's a toggle.
		}
		return Selection{}, false
	case tcell.KeyRune:
		r := ev.Rune()
		switch t.activeField {
		case 0:
			if len(t.inputBuf) < 64 {
				t.inputBuf = append(t.inputBuf, r)
				t.dialogErr = ""
			}
		case 1:
			// Protocol toggle: t/T = tcp, u/U = udp, Space cycles.
			switch r {
			case 't', 'T':
				t.inputProtocol = session.ProtocolTCP
				t.dialogErr = ""
			case 'u', 'U':
				t.inputProtocol = session.ProtocolUDP
				t.dialogErr = ""
			case ' ':
				if t.inputProtocol == session.ProtocolUDP {
					t.inputProtocol = session.ProtocolTCP
				} else {
					t.inputProtocol = session.ProtocolUDP
				}
				t.dialogErr = ""
			}
		case 2:
			if r >= '0' && r <= '9' && len(t.inputBufPort) < 5 {
				t.inputBufPort = append(t.inputBufPort, r)
				t.dialogErr = ""
			}
		case 3:
			// DNS name: letters/digits/hyphens/periods, lowercase on store.
			if len(t.inputBufDNS) >= 253 {
				return Selection{}, false
			}
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
				t.inputBufDNS = append(t.inputBufDNS, r)
				t.dialogErr = ""
			} else if r >= 'A' && r <= 'Z' {
				t.inputBufDNS = append(t.inputBufDNS, r+('a'-'A'))
				t.dialogErr = ""
			}
		}
	}
	return Selection{}, false
}

func (t *tui) handleCloneDialogKey(ev *tcell.EventKey) (Selection, bool) {
	switch ev.Key() {
	case tcell.KeyEscape:
		t.mode = modeMenu
		t.inputBuf = nil
		t.dialogErr = ""
	case tcell.KeyEnter:
		if t.cursor < 0 || t.cursor >= len(t.sessions) {
			t.mode = modeMenu
			return Selection{}, false
		}
		name := strings.TrimSpace(string(t.inputBuf))
		if err := session.ValidateName(name); err != nil {
			t.dialogErr = err.Error()
			return Selection{}, false
		}
		s := t.sessions[t.cursor]
		return Selection{Action: ActionCloneSession, SessionID: s.UUID, Name: name}, true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(t.inputBuf) > 0 {
			t.inputBuf = t.inputBuf[:len(t.inputBuf)-1]
			t.dialogErr = ""
		}
	case tcell.KeyRune:
		if len(t.inputBuf) < 64 {
			t.inputBuf = append(t.inputBuf, ev.Rune())
			t.dialogErr = ""
		}
	}
	return Selection{}, false
}

func drawString(screen tcell.Screen, x, y int, s string, style tcell.Style) {
	for _, ch := range s {
		screen.SetContent(x, y, ch, nil, style)
		x++
	}
}

func fillRow(screen tcell.Screen, row, width int, style tcell.Style) {
	for x := 0; x < width; x++ {
		screen.SetContent(x, row, ' ', nil, style)
	}
}

func clipToWidth(s string, width int) string {
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width])
}

// errorDialogMinWidth is the minimum dialog width to use when an error must
// be displayed — wide enough that wrapped lines read cleanly instead of
// collapsing to one word per row.
const errorDialogMinWidth = 72

// sizeDialogForError returns the dialog width to use given the dialog's
// natural width, its minimum width, the current error string, and the screen
// width. When an error is present, the dialog is widened to at least
// errorDialogMinWidth; the result is capped so the dialog still fits on screen.
func sizeDialogForError(natural, minWidth int, errText string, screenWidth int) int {
	width := natural
	if width < minWidth {
		width = minWidth
	}
	if errText != "" && width < errorDialogMinWidth {
		width = errorDialogMinWidth
	}
	if width > screenWidth-2 {
		width = screenWidth - 2
	}
	if width < minWidth {
		width = minWidth
	}
	return width
}

// errorTextMaxChars caps how much of a dialog error message is displayed.
// Anything beyond this is replaced with a trailing ellipsis so the dialog
// cannot grow unbounded on enormous error strings.
const errorTextMaxChars = 500

// wrapErrorText word-wraps an error message into lines of at most width
// characters, preserving word boundaries where possible and hard-wrapping
// single words that exceed width. Inputs longer than errorTextMaxChars are
// truncated with "..." before wrapping. Returns nil for empty input or
// non-positive width.
func wrapErrorText(s string, width int) []string {
	if s == "" || width <= 0 {
		return nil
	}
	if len([]rune(s)) > errorTextMaxChars {
		r := []rune(s)
		s = string(r[:errorTextMaxChars-3]) + "..."
	}
	var lines []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			lines = append(lines, string(current))
			current = current[:0]
		}
	}
	for _, word := range strings.Fields(s) {
		wr := []rune(word)
		// Hard-wrap words that are longer than the line width.
		for len(wr) > width {
			flush()
			lines = append(lines, string(wr[:width]))
			wr = wr[width:]
		}
		switch {
		case len(current) == 0:
			current = append(current, wr...)
		case len(current)+1+len(wr) <= width:
			current = append(current, ' ')
			current = append(current, wr...)
		default:
			flush()
			current = append(current, wr...)
		}
	}
	flush()
	return lines
}
