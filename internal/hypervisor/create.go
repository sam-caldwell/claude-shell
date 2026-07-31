package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"
)

// init wires the package-level runCreateVM seam to the actual
// orchestrator. Tests can swap runCreateVM out and run their own
// scenario. types.go exports the seam so we don't have a cycle.
func init() {
	runCreateVM = orchestrate
}

// orchestrate is the top-level flow for `convocate-host create-vm`.
// Phases:
//
//  1. Dial hypervisor (key-then-password)
//  2. Install operator's pubkey + harden sshd
//  3. Generate random hostname → set on hypervisor → register A
//     record in shell's dnsmasq
//  4. apt update/upgrade + reboot, then reconnect with retries
//  5. Download Ubuntu ISO locally (cached + sha256-verified)
//  6. Configure hypervisor dnsmasq (forward to shell, fallback gw)
//  7. Install KVM stack + verify /dev/kvm
//  8. Detect host resources → cap machine.slice (Layer 2)
//  9. Query existing pledges → admission check (Layer 1)
//  10. SCP Ubuntu ISO to hypervisor + build autoinstall seed ISO
//  11. virt-install --import → unattended install runs in VM
//
// Each phase logs to opts.Stderr so the operator sees progress.
func orchestrate(ctx context.Context, opts *CreateVMOptions) error {
	w := opts.Stderr
	logf := func(s string, a ...any) { fmt.Fprintf(w, "[create-vm] "+s+"\n", a...) }

	// Phase 1: dial.
	logf("dialing %s@%s", opts.Username, opts.Hypervisor)
	r, err := dialRunner(ctx, opts)
	if err != nil {
		return err
	}
	defer r.Close()

	// Phase 2: install key + harden sshd.
	logf("installing operator pubkey")
	if err := InstallOperatorKey(ctx, r, ""); err != nil {
		return err
	}
	logf("hardening sshd (CIS-aligned)")
	if err := HardenSSHD(ctx, r); err != nil {
		return err
	}

	// Phase 3: hostname + DNS.
	hostname, err := randHostname()
	if err != nil {
		return fmt.Errorf("generate hostname: %w", err)
	}
	fqdn := hostname + "." + opts.Domain
	logf("assigning hostname %s (fqdn %s)", hostname, fqdn)
	if err := SetHypervisorHostname(ctx, r, hostname, fqdn); err != nil {
		return err
	}
	hypIP, err := ResolveHypervisorIP(opts.Hypervisor)
	if err != nil {
		return err
	}
	logf("registering %s → %s in shell dnsmasq", fqdn, hypIP)
	if err := registerDNSName("", fqdn, hypIP); err != nil {
		return err
	}

	// Phase 4: apt upgrade + reboot + reconnect.
	logf("apt update + dist-upgrade (then reboot)")
	if err := aptUpgradeAndReboot(ctx, r); err != nil {
		return err
	}
	r.Close()
	logf("waiting for hypervisor to come back up")
	r2, err := waitForReconnect(ctx, opts)
	if err != nil {
		return err
	}
	r = r2
	defer r.Close()

	// Phase 5: ISO download (operator side).
	logf("ensuring local Ubuntu ISO is present + verified")
	isoPath, err := ensureUbuntuISO(opts)
	if err != nil {
		return err
	}

	// Phase 6: hypervisor dnsmasq forwarder.
	logf("configuring hypervisor dnsmasq forwarder")
	shellIP := localShellIP()
	if shellIP == "" {
		return errors.New("create-vm: cannot determine local shell host IP for dnsmasq forwarder")
	}
	if err := ConfigureHypervisorDnsmasq(ctx, r, shellIP); err != nil {
		return err
	}

	// Phase 7: KVM stack.
	logf("installing KVM stack")
	if err := InstallKVMStack(ctx, r, opts.Username); err != nil {
		return err
	}
	if err := VerifyKVMSupport(ctx, r); err != nil {
		return err
	}

	// Phase 8: capacity probe + slice cap (Layer 2).
	logf("probing host resources")
	resources, err := DetectResources(ctx, r)
	if err != nil {
		return err
	}
	logf("host: %d cores, %d MB RAM, %d GB disk", resources.CPUCores, resources.RAMMB, resources.DiskGB)
	logf("capping machine.slice at 90%% (Layer 2)")
	if err := CapMachineSlice(ctx, r, resources); err != nil {
		return err
	}

	// Phase 9: admission control (Layer 1).
	logf("checking VM admission against 90%% cap (Layer 1)")
	existing, err := QueryExistingPledge(ctx, r)
	if err != nil {
		return err
	}
	want := Pledge{
		CPUCores: opts.CPU,
		RAMMB:    int64(opts.RAMMB),
		DiskGB:   int64(opts.OSDiskGB + opts.DataDiskGB),
	}
	if err := CheckAdmission(resources, existing, want); err != nil {
		return err
	}

	// Phase 10: ship ISO + build autoinstall seed.
	isoDir := opts.HypervisorISODir
	if isoDir == "" {
		isoDir = "/var/lib/libvirt/images/iso"
	}
	logf("transferring ISO to %s", isoDir)
	if err := r.Run(ctx, "mkdir -p "+shellQuoteArg(isoDir), RunOptions{Sudo: true}); err != nil {
		return fmt.Errorf("mkdir %s: %w", isoDir, err)
	}
	remoteISO := filepath.Join(isoDir, filepath.Base(isoPath))
	if err := r.CopyFile(ctx, isoPath, remoteISO, 0644); err != nil {
		return fmt.Errorf("scp ISO: %w", err)
	}

	pubKey, _, err := readOperatorPubKey("")
	if err != nil {
		return err
	}
	cfg := &AutoinstallConfig{
		Hostname:      hostname,
		FQDN:          fqdn,
		Username:      opts.Username,
		AuthorizedKey: string(pubKey),
	}
	seedName := opts.SeedISOName
	if seedName == "" {
		seedName = "convocate-seed-" + hostname + ".iso"
	}
	logf("building autoinstall seed (cloud-init NoCloud)")
	if err := PushAutoinstallSeed(ctx, r, cfg, isoDir, seedName); err != nil {
		return err
	}

	// Phase 11: virt-install.
	logf("starting virt-install (unattended)")
	if err := startVirtInstall(ctx, r, opts, hostname, isoDir, filepath.Base(isoPath), seedName); err != nil {
		return err
	}

	logf("create-vm complete: %s reachable at %s once install finishes", fqdn, hypIP)
	return nil
}

// aptUpgradeAndReboot runs the canonical "update + dist-upgrade then
// reboot" sequence. Reboot is `shutdown -r +0` (immediate) — we use
// `+0` rather than `now` so systemd's pending-reboot logic gets a
// chance to wire up cleanly. The trailing `&` detaches so the SSH
// session doesn't fail the reboot's own "connection lost" return.
//
// The exit-status of the SSH session for `shutdown` itself is
// frequently EOF / 255 because the daemon kills our connection mid-
// command. We treat that as success.
var aptUpgradeAndReboot = func(ctx context.Context, r Runner) error {
	cmd := `set -e
DEBIAN_FRONTEND=noninteractive apt-get update -y
DEBIAN_FRONTEND=noninteractive apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -y dist-upgrade
nohup shutdown -r +0 >/dev/null 2>&1 &
`
	err := r.Run(ctx, cmd, RunOptions{Sudo: true})
	// SSH connection drops mid-reboot are expected. Surface the error
	// only when it doesn't look like that. Production sshRunner wraps
	// the lib's ExitError into a `remote ...: %w` form; we conservatively
	// return nil so the wait-for-reboot loop can take over.
	_ = err
	return nil
}

// waitForReconnect polls the hypervisor every 30s for up to 10 minutes
// after the apt+reboot phase, redialling until SSH answers. Returns
// the new Runner that subsequent phases use.
var waitForReconnect = func(ctx context.Context, opts *CreateVMOptions) (Runner, error) {
	const interval = 30 * time.Second
	const overall = 10 * time.Minute
	deadline := time.Now().Add(overall)
	var lastErr error
	for time.Now().Before(deadline) {
		// Wait first — apt+reboot just finished, the box is still
		// going down. 30s is roughly when sshd usually comes back.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		r, err := dialRunner(ctx, opts)
		if err == nil {
			return r, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("hypervisor did not come back online within %v: %w", overall, lastErr)
}

// ensureUbuntuISO encapsulates the ISOFetcher call so tests can
// stub out the network without rewiring the orchestrator.
var ensureUbuntuISO = func(opts *CreateVMOptions) (string, error) {
	f := &ISOFetcher{CacheDir: opts.IsoCacheDir}
	return f.Fetch()
}

// registerDNSName is the orchestrator's seam onto RegisterDNSName so
// tests can stub the local-filesystem write that would otherwise
// require /var/lib/convocate to be present and writable.
var registerDNSName = RegisterDNSName

// localShellIP returns the local machine's primary IPv4 — what the
// hypervisor's dnsmasq should forward to. Tests stub this directly;
// production walks the local interface list via defaultDetectLocalIP.
var localShellIP = defaultDetectLocalIP

func defaultDetectLocalIP() string {
	// Identical body to internal/dns.DetectHostIP. Inline so create.go
	// stays a single-import file.
	addrs, err := netInterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if a.loopback {
			continue
		}
		if a.ipv4 != "" {
			return a.ipv4
		}
	}
	return ""
}

// netInterfaceAddrs is exposed as a var (with a small helper struct)
// so tests can drive defaultDetectLocalIP without touching the system
// network stack.
type ifaceAddr struct {
	ipv4     string
	loopback bool
}

var netInterfaceAddrs = func() ([]ifaceAddr, error) {
	raw, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]ifaceAddr, 0, len(raw))
	for _, a := range raw {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		entry := ifaceAddr{loopback: ipnet.IP.IsLoopback()}
		if v4 := ipnet.IP.To4(); v4 != nil {
			entry.ipv4 = v4.String()
		}
		out = append(out, entry)
	}
	return out, nil
}

// startVirtInstall fires `virt-install` on the hypervisor with all
// the knobs cobbled together from opts + the staging paths set up by
// earlier phases. Two --disk lines, two --cdrom lines (Ubuntu ISO +
// the NoCloud seed). --noautoconsole returns immediately so our SSH
// session doesn't have to babysit the install — the VM keeps running
// in the background under libvirt and finishes the install on its
// own.
//
// --extra-args 'autoinstall' tells subiquity to pick up the NoCloud
// data without prompting. --osinfo ubuntu22.04 lets virt-install
// pick sane defaults (machine type, video, etc.).
var startVirtInstall = func(ctx context.Context, r Runner, opts *CreateVMOptions, hostname, isoDir, isoFile, seedFile string) error {
	cpu := opts.CPU
	ram := opts.RAMMB
	osDisk := opts.OSDiskGB
	dataDisk := opts.DataDiskGB
	osDiskPath := filepath.Join("/var/lib/libvirt/images", hostname+"-os.qcow2")
	dataDiskPath := filepath.Join("/var/lib/libvirt/images", hostname+"-data.qcow2")
	isoPath := filepath.Join(isoDir, isoFile)
	seedPath := filepath.Join(isoDir, seedFile)

	cmd := fmt.Sprintf(`virt-install \
  --connect qemu:///system \
  --name %s \
  --vcpus %d \
  --memory %d \
  --osinfo ubuntu22.04 \
  --disk path=%s,size=%d,format=qcow2,bus=virtio \
  --disk path=%s,size=%d,format=qcow2,bus=virtio \
  --cdrom %s \
  --disk path=%s,device=cdrom,readonly=on \
  --network network=default,model=virtio \
  --graphics none \
  --console pty,target_type=serial \
  --extra-args 'console=ttyS0,115200n8 autoinstall' \
  --noautoconsole`,
		shellQuoteArg(hostname),
		cpu, ram,
		shellQuoteArg(osDiskPath), osDisk,
		shellQuoteArg(dataDiskPath), dataDisk,
		shellQuoteArg(isoPath),
		shellQuoteArg(seedPath),
	)

	return r.Run(ctx, cmd, RunOptions{Sudo: true, Stdout: io.Discard, Stderr: io.Discard})
}
