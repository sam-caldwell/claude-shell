package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// kvmPackages lists everything apt installs to turn a vanilla Ubuntu
// host into a working KVM hypervisor for our flow:
//
//	qemu-kvm                  hypervisor binaries
//	libvirt-daemon-system     libvirtd + virsh
//	libvirt-clients           virt-install / virsh client tools
//	virtinst                  virt-install scripting wrapper
//	bridge-utils              bridge networking
//	cloud-image-utils         cloud-localds (NoCloud seed builder)
//	genisoimage               fallback seed ISO builder
const kvmPackages = `qemu-kvm libvirt-daemon-system libvirt-clients virtinst bridge-utils cloud-image-utils genisoimage`

// InstallKVMStack installs the apt packages needed to run KVM VMs,
// adds username to the libvirt + kvm groups, ensures the daemons are
// active, and brings libvirt's default NAT network up. Idempotent —
// re-running on an already-configured host is a no-op.
func InstallKVMStack(ctx context.Context, r Runner, username string) error {
	if username == "" {
		return errors.New("InstallKVMStack: username required")
	}

	cmd := fmt.Sprintf(`set -e
DEBIAN_FRONTEND=noninteractive apt-get install -y %s
systemctl enable --now libvirtd
usermod -aG libvirt %s
usermod -aG kvm %s
# default NAT network. virsh refuses if it's already running, so
# tolerate that and just ensure autostart is on.
virsh net-autostart default || true
virsh net-start default || true
`, kvmPackages, username, username)

	return r.Run(ctx, cmd, RunOptions{Sudo: true})
}

// VerifyKVMSupport runs a quick sanity check that /dev/kvm exists on
// the hypervisor. Surfaces a clear error early if the host is itself
// a VM with nested-virt disabled — saves the operator a confusing
// failure when virt-install can't fire QEMU.
func VerifyKVMSupport(ctx context.Context, r Runner) error {
	cmd := `test -c /dev/kvm && test -r /dev/kvm`
	if err := r.Run(ctx, cmd, RunOptions{}); err != nil {
		// Note: /dev/kvm permissions on stock Ubuntu are root:kvm 0660.
		// Operators not yet in the kvm group will fail the read test
		// but pass the existence test. Surface both possibilities in
		// the message.
		return fmt.Errorf("/dev/kvm not available — host either lacks virtualization extensions, has nested-virt disabled, or the connecting user is not yet in the kvm group: %w", err)
	}
	return nil
}

// HypervisorResources holds the host's CPU/RAM/disk totals captured
// after InstallKVMStack succeeds. Callers consume these alongside
// existing-VM allocations to make Layer 1 admission decisions.
type HypervisorResources struct {
	CPUCores int
	RAMMB    int64
	DiskGB   int64
}

// DetectResources reads CPU + memory + libvirt-storage-pool totals
// from the hypervisor. Disk is the size of the default libvirt pool
// (typically /var/lib/libvirt/images) since that's where the new VM
// will land.
func DetectResources(ctx context.Context, r Runner) (HypervisorResources, error) {
	var out HypervisorResources

	cores, err := readInt(ctx, r, `nproc`)
	if err != nil {
		return out, fmt.Errorf("read nproc: %w", err)
	}
	out.CPUCores = cores

	memKB, err := readInt(ctx, r, `awk '/^MemTotal:/{print $2; exit}' /proc/meminfo`)
	if err != nil {
		return out, fmt.Errorf("read MemTotal: %w", err)
	}
	out.RAMMB = int64(memKB) / 1024

	// Default libvirt pool: virsh pool-info default. Fall back to
	// /var/lib/libvirt/images via df if the pool isn't defined yet.
	diskBytes, err := readInt(ctx, r, `df -B1 --output=size /var/lib/libvirt/images 2>/dev/null | tail -n1`)
	if err != nil || diskBytes == 0 {
		// /var/lib/libvirt/images may not exist yet on a host that
		// just had KVM installed; df its parent /var instead.
		fallback, ferr := readInt(ctx, r, `df -B1 --output=size /var | tail -n1`)
		if ferr != nil {
			return out, fmt.Errorf("read disk size: %w", ferr)
		}
		diskBytes = fallback
	}
	out.DiskGB = int64(diskBytes) / (1 << 30)
	return out, nil
}

// readInt runs cmd remotely and parses the first integer in the
// trimmed stdout. Surfaces a clear "parse <output>" error if the
// command produced something unexpected.
func readInt(ctx context.Context, r Runner, cmd string) (int, error) {
	var sb stringBuilder
	if err := r.Run(ctx, cmd, RunOptions{Stdout: &sb}); err != nil {
		return 0, err
	}
	s := strings.TrimSpace(sb.String())
	// Pull off the leading run of digits (handles `df` output that
	// may include header rows when the caller forgot to tail).
	digits := ""
	for _, c := range s {
		if c >= '0' && c <= '9' {
			digits += string(c)
			continue
		}
		break
	}
	if digits == "" {
		return 0, fmt.Errorf("no integer in %q", s)
	}
	n := 0
	for _, c := range digits {
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// stringBuilder is a tiny io.Writer-compatible builder. Avoids
// dragging strings.Builder's internal complexity into a one-shot
// "capture stdout" helper.
type stringBuilder struct{ buf []byte }

func (s *stringBuilder) Write(p []byte) (int, error) { s.buf = append(s.buf, p...); return len(p), nil }
func (s *stringBuilder) String() string              { return string(s.buf) }
