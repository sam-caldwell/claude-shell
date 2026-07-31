// Package session manages convocate sessions including creation, listing, deletion, and locking.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/asymmetric-effort/convocate/internal/config"
	"github.com/google/uuid"
)

// Metadata holds session metadata persisted as session.json.
type Metadata struct {
	UUID         string    `json:"uuid"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	LastAccessed time.Time `json:"last_accessed"`
	// Port is the TCP/UDP port published by the session's container.
	// A value of 0 means no port is published.
	Port int `json:"port,omitempty"`
	// Protocol is "tcp" or "udp" — the protocol used for the published port.
	// An empty value is treated as "tcp" for backward compatibility.
	Protocol string `json:"protocol,omitempty"`
	// DNSName is the optional hostname registered with the local dnsmasq
	// service. Empty means no DNS record is published.
	DNSName string `json:"dns_name,omitempty"`

	// AgentID and AgentHost are populated when this metadata describes a
	// session living on a remote convocate-agent rather than on the local
	// host. They are never persisted (json:"-") — the local Manager's
	// session.json files never carry them, and remote metadata has them
	// stamped in by the shell-side aggregator after a CRUD list response.
	AgentID   string `json:"-"`
	AgentHost string `json:"-"`

	// Running is the live container state at the moment of the list
	// response: true = the session's docker container is running,
	// false = stopped. Set by the agent's list op; always false in
	// records produced by the local session.Manager alone (no docker
	// probe). Never persisted — it's a snapshot, not metadata.
	Running bool `json:"running,omitempty"`

	// Attached reports whether any operator currently has an open
	// convocate-agent-attach pty on this session. Set by the agent's
	// list op from an in-memory attach counter; drives the "C"
	// indicator in the TUI so operators can see when another user is
	// live on a session. Always false for sessions the local Manager
	// alone knows about (orphans + pre-v2).
	Attached bool `json:"attached,omitempty"`
}

// IsRemote reports whether this metadata describes a session owned by a
// remote agent (AgentID non-empty) rather than the local host.
func (m Metadata) IsRemote() bool { return m.AgentID != "" }

// EffectiveProtocol returns the session's protocol, defaulting to "tcp" when
// the field is empty (older sessions created before protocol support).
func (m Metadata) EffectiveProtocol() string {
	if m.Protocol == "" {
		return ProtocolTCP
	}
	return m.Protocol
}

// PortAuto is the sentinel value passed to CreateWithPort to request that an
// available port above 1000 be selected automatically.
const PortAuto = -1

// PortAutoMin is the lowest port number considered when auto-assigning.
const PortAutoMin = 1001

// Supported protocols for the published port.
const (
	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"
)

// ValidateProtocol normalizes a user-provided protocol string. An empty value
// is treated as "tcp"; otherwise the string is lowercased and must match one
// of the supported protocols.
func ValidateProtocol(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", ProtocolTCP:
		return ProtocolTCP, nil
	case ProtocolUDP:
		return ProtocolUDP, nil
	default:
		return "", fmt.Errorf("protocol must be %q or %q, got %q", ProtocolTCP, ProtocolUDP, s)
	}
}

// ValidateDNSName checks that s is a valid DNS hostname suitable for
// registration with the local dnsmasq. Empty is allowed (no DNS record).
// Otherwise:
//   - ASCII letters, digits, hyphens, and dots only
//   - Each label (between dots) is 1-63 chars, must not start or end with '-'
//   - Total length <= 253 chars
//
// The returned string is the lowercased canonical form.
func ValidateDNSName(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if len(s) > 253 {
		return "", fmt.Errorf("DNS name too long (max 253 chars)")
	}
	labels := strings.Split(s, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return "", fmt.Errorf("DNS label length must be 1-63 chars")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("DNS label cannot start or end with '-'")
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return "", fmt.Errorf("DNS name contains invalid character %q (only a-z, 0-9, -, . allowed)", r)
			}
		}
	}
	return s, nil
}

// CreateOptions holds all configurable parameters for creating a session.
type CreateOptions struct {
	Port     int
	Protocol string
	DNSName  string
}

// UpdateOptions holds the fields that can be edited after creation.
// Each field is interpreted verbatim; empty Protocol resolves to "tcp".
type UpdateOptions struct {
	Name     string
	Port     int
	Protocol string
	DNSName  string
}

// Manager handles session lifecycle operations.
type Manager struct {
	basePath string
	skelPath string
}

// NewManager creates a new session Manager.
func NewManager(basePath, skelPath string) *Manager {
	return &Manager{
		basePath: basePath,
		skelPath: skelPath,
	}
}

// Create creates a new session with the given name and returns its metadata.
// No network port is published.
func (m *Manager) Create(name string) (Metadata, error) {
	return m.CreateWithPortProtocol(name, 0, ProtocolTCP)
}

// CreateWithPort creates a new session with the given name and port using the
// default TCP protocol. Kept for existing callers that don't care about
// protocol selection.
func (m *Manager) CreateWithPort(name string, port int) (Metadata, error) {
	return m.CreateWithPortProtocol(name, port, ProtocolTCP)
}

// CreateWithPortProtocol creates a new session with the given name, port, and
// protocol (legacy wrapper — new code should use CreateWithOptions).
func (m *Manager) CreateWithPortProtocol(name string, port int, protocol string) (Metadata, error) {
	return m.CreateWithOptions(name, CreateOptions{Port: port, Protocol: protocol})
}

// CreateWithOptions creates a new session. Port semantics: 0 means no port is
// published; PortAuto (-1) requests auto-selection of the first available
// port at or above PortAutoMin; any positive value is used verbatim after
// checking no other session uses it with the same protocol.
func (m *Manager) CreateWithOptions(name string, opts CreateOptions) (Metadata, error) {
	id := uuid.New().String()
	return m.createWithUUIDOptions(id, name, opts)
}

// CreateWithUUID is the legacy test helper that creates a session with a
// specific UUID and defaults protocol to TCP.
func (m *Manager) CreateWithUUID(id, name string, port int) (Metadata, error) {
	return m.createWithUUIDOptions(id, name, CreateOptions{Port: port, Protocol: ProtocolTCP})
}

// CreateWithUUIDProtocol creates a new session with a specific UUID, port,
// and protocol (legacy wrapper).
func (m *Manager) CreateWithUUIDProtocol(id, name string, port int, protocol string) (Metadata, error) {
	return m.createWithUUIDOptions(id, name, CreateOptions{Port: port, Protocol: protocol})
}

// createWithUUIDOptions is the canonical implementation. It validates inputs,
// resolves collisions, writes the session directory, and persists metadata.
func (m *Manager) createWithUUIDOptions(id, name string, opts CreateOptions) (Metadata, error) {
	proto, err := ValidateProtocol(opts.Protocol)
	if err != nil {
		return Metadata{}, err
	}

	dnsName, err := ValidateDNSName(opts.DNSName)
	if err != nil {
		return Metadata{}, err
	}

	resolvedPort, err := m.resolvePort(opts.Port, proto)
	if err != nil {
		return Metadata{}, err
	}

	if dnsName != "" && m.isDNSNameUsedByOther(id, dnsName) {
		return Metadata{}, fmt.Errorf("DNS name %q is already assigned to another session", dnsName)
	}

	sessionDir := filepath.Join(m.basePath, id)

	if err := m.copySkel(sessionDir); err != nil {
		return Metadata{}, fmt.Errorf("failed to initialize session directory: %w", err)
	}

	if err := m.setupClaudeSymlinks(sessionDir); err != nil {
		return Metadata{}, fmt.Errorf("failed to setup claude symlinks: %w", err)
	}

	now := time.Now().UTC()
	meta := Metadata{
		UUID:         id,
		Name:         name,
		CreatedAt:    now,
		LastAccessed: now,
		Port:         resolvedPort,
		Protocol:     proto,
		DNSName:      dnsName,
	}

	if err := m.writeMetadata(sessionDir, meta); err != nil {
		_ = os.RemoveAll(sessionDir)
		return Metadata{}, fmt.Errorf("failed to write session metadata: %w", err)
	}

	return meta, nil
}

// isDNSNameUsedByOther reports whether the given DNS name is registered by
// any session other than excludeID.
func (m *Manager) isDNSNameUsedByOther(excludeID, dnsName string) bool {
	if dnsName == "" {
		return false
	}
	sessions, err := m.List()
	if err != nil {
		return false
	}
	for _, s := range sessions {
		if s.UUID == excludeID {
			continue
		}
		if strings.EqualFold(s.DNSName, dnsName) {
			return true
		}
	}
	return false
}

// resolvePort translates a caller-supplied port value into the port actually
// written to session metadata. Collisions are checked per-protocol, so
// tcp:53 and udp:53 are allowed to coexist in different sessions.
func (m *Manager) resolvePort(port int, protocol string) (int, error) {
	switch {
	case port == 0:
		return 0, nil
	case port == PortAuto:
		return m.FindAvailablePort(PortAutoMin)
	case port > 0 && port <= 65535:
		if m.isPortUsed(port, protocol) {
			return 0, fmt.Errorf("port %d/%s is already assigned to another session", port, protocol)
		}
		return port, nil
	default:
		return 0, fmt.Errorf("invalid port: %d", port)
	}
}

func (m *Manager) isPortUsed(port int, protocol string) bool {
	sessions, err := m.List()
	if err != nil {
		return false
	}
	for _, s := range sessions {
		if s.Port == port && s.EffectiveProtocol() == protocol {
			return true
		}
	}
	return false
}

// FindAvailablePort returns the lowest port at or above min that is not
// already assigned to any existing session.
func (m *Manager) FindAvailablePort(min int) (int, error) {
	sessions, err := m.List()
	if err != nil {
		return 0, fmt.Errorf("failed to list sessions: %w", err)
	}
	used := make(map[int]bool, len(sessions))
	for _, s := range sessions {
		if s.Port > 0 {
			used[s.Port] = true
		}
	}
	for p := min; p <= 65535; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no available port at or above %d", min)
}

// Clone creates a new session whose home directory is a copy of an existing session's.
func (m *Manager) Clone(sourceID, newName string) (Metadata, error) {
	srcDir := filepath.Join(m.basePath, sourceID)
	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		return Metadata{}, fmt.Errorf("session %q does not exist", sourceID)
	}

	newID := uuid.New().String()
	dstDir := filepath.Join(m.basePath, newID)

	if err := copyDir(srcDir, dstDir); err != nil {
		_ = os.RemoveAll(dstDir)
		return Metadata{}, fmt.Errorf("failed to copy session directory: %w", err)
	}

	now := time.Now().UTC()
	meta := Metadata{
		UUID:         newID,
		Name:         newName,
		CreatedAt:    now,
		LastAccessed: now,
	}

	if err := m.writeMetadata(dstDir, meta); err != nil {
		_ = os.RemoveAll(dstDir)
		return Metadata{}, fmt.Errorf("failed to write session metadata: %w", err)
	}

	return meta, nil
}

// List returns all existing sessions sorted by last accessed time (most recent first).
func (m *Manager) List() ([]Metadata, error) {
	entries, err := os.ReadDir(m.basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read sessions directory: %w", err)
	}

	var sessions []Metadata
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if _, err := uuid.Parse(name); err != nil {
			continue
		}
		meta, err := m.readMetadata(filepath.Join(m.basePath, name))
		if err != nil {
			continue
		}
		sessions = append(sessions, meta)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastAccessed.After(sessions[j].LastAccessed)
	})

	return sessions, nil
}

// Get retrieves metadata for a specific session.
func (m *Manager) Get(id string) (Metadata, error) {
	sessionDir := filepath.Join(m.basePath, id)
	if _, err := os.Stat(sessionDir); os.IsNotExist(err) {
		return Metadata{}, fmt.Errorf("session %q does not exist", id)
	}
	return m.readMetadata(sessionDir)
}

// Delete removes a session directory and its contents.
func (m *Manager) Delete(id string) error {
	sessionDir := filepath.Join(m.basePath, id)
	if _, err := os.Stat(sessionDir); os.IsNotExist(err) {
		return fmt.Errorf("session %q does not exist", id)
	}

	lockPath := filepath.Join(m.basePath, id+config.LockFileExtension)
	if _, err := os.Stat(lockPath); err == nil {
		return fmt.Errorf("session %q is currently locked (in use)", id)
	}

	return os.RemoveAll(sessionDir)
}

// Update applies edited fields to the given session and persists them to
// session.json. Legacy wrapper around UpdateWithOptions; new code should use
// UpdateWithOptions directly.
func (m *Manager) Update(id, newName string, newPort int, newProtocol string) (Metadata, error) {
	return m.UpdateWithOptions(id, UpdateOptions{
		Name:     newName,
		Port:     newPort,
		Protocol: newProtocol,
	})
}

// UpdateWithOptions applies the fields in opts to the session identified by
// id. UUID, CreatedAt, and LastAccessed are preserved. An empty Protocol is
// treated as "tcp". Collisions are checked per-protocol: tcp:53 and udp:53
// can coexist in different sessions. An empty DNSName clears any existing
// record.
func (m *Manager) UpdateWithOptions(id string, opts UpdateOptions) (Metadata, error) {
	sessionDir := filepath.Join(m.basePath, id)
	current, err := m.readMetadata(sessionDir)
	if err != nil {
		return Metadata{}, err
	}

	if err := ValidateName(opts.Name); err != nil {
		return Metadata{}, err
	}

	proto, err := ValidateProtocol(opts.Protocol)
	if err != nil {
		return Metadata{}, err
	}

	dnsName, err := ValidateDNSName(opts.DNSName)
	if err != nil {
		return Metadata{}, err
	}

	resolvedPort := current.Port
	// Re-resolve the port when either the number OR the protocol changed —
	// a collision may exist against the new (port, protocol) tuple even if
	// only the protocol differs.
	if opts.Port != current.Port || proto != current.EffectiveProtocol() {
		switch {
		case opts.Port == 0:
			resolvedPort = 0
		case opts.Port == PortAuto:
			rp, err := m.FindAvailablePort(PortAutoMin)
			if err != nil {
				return Metadata{}, err
			}
			resolvedPort = rp
		case opts.Port > 0 && opts.Port <= 65535:
			if m.isPortUsedByOther(id, opts.Port, proto) {
				return Metadata{}, fmt.Errorf("port %d/%s is already assigned to another session", opts.Port, proto)
			}
			resolvedPort = opts.Port
		default:
			return Metadata{}, fmt.Errorf("invalid port: %d", opts.Port)
		}
	}

	if dnsName != "" && !strings.EqualFold(dnsName, current.DNSName) && m.isDNSNameUsedByOther(id, dnsName) {
		return Metadata{}, fmt.Errorf("DNS name %q is already assigned to another session", dnsName)
	}

	current.Name = opts.Name
	current.Port = resolvedPort
	current.Protocol = proto
	current.DNSName = dnsName
	if err := m.writeMetadata(sessionDir, current); err != nil {
		return Metadata{}, err
	}
	return current, nil
}

// isPortUsedByOther reports whether the given (port, protocol) pair is held
// by any session other than the one identified by excludeID.
func (m *Manager) isPortUsedByOther(excludeID string, port int, protocol string) bool {
	sessions, err := m.List()
	if err != nil {
		return false
	}
	for _, s := range sessions {
		if s.UUID == excludeID {
			continue
		}
		if s.Port == port && s.EffectiveProtocol() == protocol {
			return true
		}
	}
	return false
}

// Touch updates the last accessed time for a session.
func (m *Manager) Touch(id string) error {
	sessionDir := filepath.Join(m.basePath, id)
	meta, err := m.readMetadata(sessionDir)
	if err != nil {
		return err
	}
	meta.LastAccessed = time.Now().UTC()
	return m.writeMetadata(sessionDir, meta)
}

// Lock acquires an exclusive lock for a session. Returns an unlock function.
func (m *Manager) Lock(id string) (func(), error) {
	lockPath := filepath.Join(m.basePath, id+config.LockFileExtension)

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("session %q is already locked (another instance may be running)", id)
		}
		return nil, fmt.Errorf("failed to acquire lock for session %q: %w", id, err)
	}

	pid := os.Getpid()
	_, _ = fmt.Fprintf(f, "%d", pid)
	_ = f.Close()

	unlock := func() {
		_ = os.Remove(lockPath)
	}

	return unlock, nil
}

// IsLocked checks if a session is currently locked.
func (m *Manager) IsLocked(id string) bool {
	lockPath := filepath.Join(m.basePath, id+config.LockFileExtension)
	info, err := os.Stat(lockPath)
	if err != nil {
		return false
	}
	// Check if the lock is stale (older than 24 hours)
	if time.Since(info.ModTime()) > 24*time.Hour {
		_ = os.Remove(lockPath)
		return false
	}
	// Check if the PID in the lock file is still running
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return false
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		_ = os.Remove(lockPath)
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(lockPath)
		return false
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		_ = os.Remove(lockPath)
		return false
	}
	return true
}

// OverrideLock removes a stale lock file if the owning process is no longer running.
// Returns an error if the session is actively locked by a running process.
func (m *Manager) OverrideLock(id string) error {
	lockPath := filepath.Join(m.basePath, id+config.LockFileExtension)
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return fmt.Errorf("session %q is not locked", id)
	}

	data, err := os.ReadFile(lockPath)
	if err != nil {
		// Can't read lock file — remove it
		return os.Remove(lockPath)
	}

	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		// Invalid PID — remove stale lock
		return os.Remove(lockPath)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return os.Remove(lockPath)
	}

	if err := process.Signal(syscall.Signal(0)); err == nil {
		return fmt.Errorf("session %q is actively in use by process %d", id, pid)
	}

	return os.Remove(lockPath)
}

// SessionDir returns the directory path for a session.
func (m *Manager) SessionDir(id string) string {
	return filepath.Join(m.basePath, id)
}

func (m *Manager) copySkel(dest string) error {
	if _, err := os.Stat(m.skelPath); os.IsNotExist(err) {
		return os.MkdirAll(dest, 0750)
	}

	return copyDir(m.skelPath, dest)
}

func (m *Manager) setupClaudeSymlinks(sessionDir string) error {
	claudeDir := filepath.Join(sessionDir, config.ClaudeConfigDir)
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		return err
	}

	sharedBase := filepath.Join("/home/convocate", config.ClaudeSharedDir)

	symlinks := []string{
		"settings.json",
		"settings.local.json",
		".credentials.json",
		"plugins",
	}

	for _, name := range symlinks {
		src := filepath.Join(sharedBase, name)
		dst := filepath.Join(claudeDir, name)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("failed to symlink %s: %w", name, err)
		}
	}

	return nil
}

func (m *Manager) writeMetadata(sessionDir string, meta Metadata) error {
	// Running and Attached are transient agent-stamped snapshots —
	// never persist either. This guard catches any caller that
	// accidentally round-trips a list-response Metadata back into the
	// Manager.
	meta.Running = false
	meta.Attached = false
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sessionDir, config.SessionMetadataFile), data, 0644)
}

func (m *Manager) readMetadata(sessionDir string) (Metadata, error) {
	data, err := os.ReadFile(filepath.Join(sessionDir, config.SessionMetadataFile))
	if err != nil {
		return Metadata{}, fmt.Errorf("failed to read session metadata: %w", err)
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return Metadata{}, fmt.Errorf("failed to parse session metadata: %w", err)
	}
	return meta, nil
}

// copyDir copies src to dst, preserving directory modes, regular files, and
// symlinks. Iterative — uses an explicit work-list rather than recursion,
// per the project-wide no-recursion rule (Go has no TCO; deep trees would
// otherwise risk stack-overflow panics).
func copyDir(src, dst string) error {
	type pair struct{ s, d string }

	rootInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, rootInfo.Mode()); err != nil {
		return err
	}

	stack := []pair{{src, dst}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		entries, err := os.ReadDir(cur.s)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			srcPath := filepath.Join(cur.s, entry.Name())
			dstPath := filepath.Join(cur.d, entry.Name())

			info, err := entry.Info()
			if err != nil {
				return err
			}

			if info.Mode()&os.ModeSymlink != 0 {
				link, err := os.Readlink(srcPath)
				if err != nil {
					return err
				}
				if err := os.Symlink(link, dstPath); err != nil {
					return err
				}
				continue
			}

			if entry.IsDir() {
				if err := os.MkdirAll(dstPath, info.Mode()); err != nil {
					return err
				}
				stack = append(stack, pair{srcPath, dstPath})
				continue
			}

			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, srcInfo.Mode())
}

// ValidateName checks if a session name is valid.
// Names may contain letters, digits, spaces, underscores, hyphens, and periods.
func ValidateName(name string) error {
	if len(strings.TrimSpace(name)) == 0 {
		return fmt.Errorf("session name cannot be empty")
	}
	if len(name) > 64 {
		return fmt.Errorf("session name cannot exceed 64 characters")
	}
	for _, c := range name {
		if !isValidNameRune(c) {
			return fmt.Errorf("session name contains invalid character: %q (only letters, digits, spaces, _, -, . allowed)", c)
		}
	}
	return nil
}

// isValidNameRune returns true if the rune is allowed in a session name.
func isValidNameRune(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == ' ' || c == '_' || c == '-' || c == '.'
}
