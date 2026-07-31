package hypervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInstallKVMStack_RequiresUsername(t *testing.T) {
	if err := InstallKVMStack(context.Background(), &mockRunner{}, ""); err == nil {
		t.Error("expected username error")
	}
}

func TestInstallKVMStack_ScriptShape(t *testing.T) {
	m := &mockRunner{}
	if err := InstallKVMStack(context.Background(), m, "operator"); err != nil {
		t.Fatal(err)
	}
	if len(m.cmds) != 1 {
		t.Fatalf("expected 1 cmd, got %d", len(m.cmds))
	}
	if !m.cmds[0].Opts.Sudo {
		t.Error("KVM install must sudo")
	}
	body := m.cmds[0].Cmd
	for _, want := range []string{
		"qemu-kvm",
		"libvirt-daemon-system",
		"virtinst",
		"cloud-image-utils",
		"genisoimage",
		"systemctl enable --now libvirtd",
		"usermod -aG libvirt operator",
		"usermod -aG kvm operator",
		"virsh net-autostart default",
		"virsh net-start default",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("script missing %q\n%s", want, body)
		}
	}
}

func TestInstallKVMStack_PropagatesError(t *testing.T) {
	m := &mockRunner{failOn: map[string]error{"set -e": errors.New("apt fail")}}
	err := InstallKVMStack(context.Background(), m, "u")
	if err == nil || !strings.Contains(err.Error(), "apt fail") {
		t.Errorf("expected propagated error, got %v", err)
	}
}

func TestVerifyKVMSupport_Pass(t *testing.T) {
	m := &mockRunner{}
	if err := VerifyKVMSupport(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.cmds[0].Cmd, "/dev/kvm") {
		t.Errorf("expected /dev/kvm test: %s", m.cmds[0].Cmd)
	}
}

func TestVerifyKVMSupport_Fail(t *testing.T) {
	m := &mockRunner{failOn: map[string]error{"test -c /dev/kvm": errors.New("not a char device")}}
	err := VerifyKVMSupport(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "/dev/kvm not available") {
		t.Errorf("expected /dev/kvm error, got %v", err)
	}
}

func TestDetectResources_HappyPath(t *testing.T) {
	// The remote `tail -n1` strips df's header in production. Mock
	// emits the post-tail value directly so the test parser sees only
	// the digits.
	m := &mockRunner{
		cmdStdout: map[string]string{
			"nproc":                "8\n",
			"awk '/^MemTotal:/":    "16777216\n", // 16 GiB in KB
			"df -B1 --output=size": "107374182400\n",
		},
	}
	res, err := DetectResources(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if res.CPUCores != 8 {
		t.Errorf("CPUCores = %d, want 8", res.CPUCores)
	}
	if res.RAMMB != 16384 {
		t.Errorf("RAMMB = %d, want 16384", res.RAMMB)
	}
	if res.DiskGB != 100 {
		t.Errorf("DiskGB = %d, want 100", res.DiskGB)
	}
}

func TestDetectResources_FallbackDfPath(t *testing.T) {
	// libvirt path returns empty → fallback to /var via df. Mock
	// matches by prefix; the failOn entry forces the libvirt-path df
	// to error so we exercise the fallback branch deterministically.
	m := &mockRunner{
		cmdStdout: map[string]string{
			"nproc":                            "2\n",
			"awk '/^MemTotal:/":                "1048576\n",
			"df -B1 --output=size /var | tail": "53687091200\n",
		},
		failOn: map[string]error{
			"df -B1 --output=size /var/lib/libvirt/images": errors.New("missing"),
		},
	}
	res, err := DetectResources(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if res.CPUCores != 2 || res.RAMMB != 1024 {
		t.Errorf("unexpected: %+v", res)
	}
	if res.DiskGB != 50 {
		t.Errorf("DiskGB fallback = %d, want 50", res.DiskGB)
	}
}

func TestDetectResources_NprocFails(t *testing.T) {
	m := &mockRunner{failOn: map[string]error{"nproc": errors.New("boom")}}
	if _, err := DetectResources(context.Background(), m); err == nil ||
		!strings.Contains(err.Error(), "nproc") {
		t.Errorf("expected nproc error, got %v", err)
	}
}

func TestDetectResources_MemTotalFails(t *testing.T) {
	m := &mockRunner{
		cmdStdout: map[string]string{"nproc": "1\n"},
		failOn:    map[string]error{"awk '/^MemTotal:/": errors.New("no /proc")},
	}
	if _, err := DetectResources(context.Background(), m); err == nil ||
		!strings.Contains(err.Error(), "MemTotal") {
		t.Errorf("expected MemTotal error, got %v", err)
	}
}

func TestDetectResources_DiskTotalFailsBoth(t *testing.T) {
	m := &mockRunner{
		cmdStdout: map[string]string{
			"nproc": "1\n",
			"awk '/^MemTotal:/{print $2; exit}' /proc/meminfo": "1048576\n",
		},
		failOn: map[string]error{
			"df -B1 --output=size /var/lib/libvirt/images": errors.New("a"),
			"df -B1 --output=size /var":                    errors.New("b"),
		},
	}
	if _, err := DetectResources(context.Background(), m); err == nil ||
		!strings.Contains(err.Error(), "disk size") {
		t.Errorf("expected disk error, got %v", err)
	}
}

// readInt error paths
func TestReadInt_NoIntegerInOutput(t *testing.T) {
	m := &mockRunner{cmdStdout: map[string]string{"echo x": "no digits here\n"}}
	_, err := readInt(context.Background(), m, "echo x")
	if err == nil || !strings.Contains(err.Error(), "no integer") {
		t.Errorf("got %v", err)
	}
}

func TestReadInt_ParsesLeadingDigits(t *testing.T) {
	// df typically outputs "size\n<bytes>\n"; the helper should still
	// extract the digits even if there's noise after.
	m := &mockRunner{cmdStdout: map[string]string{"x": "12345 some-suffix\n"}}
	got, err := readInt(context.Background(), m, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got != 12345 {
		t.Errorf("got %d", got)
	}
}

// --- slice.go --------------------------------------------------------------

func TestCapMachineSlice_ScriptShape(t *testing.T) {
	m := &mockRunner{}
	res := HypervisorResources{CPUCores: 8, RAMMB: 16384, DiskGB: 100}
	if err := CapMachineSlice(context.Background(), m, res); err != nil {
		t.Fatal(err)
	}
	if len(m.cmds) != 1 {
		t.Fatalf("expected 1 cmd, got %d", len(m.cmds))
	}
	body := m.cmds[0].Cmd
	for _, want := range []string{
		"/etc/systemd/system/machine.slice.d/99-convocate-cap.conf",
		"CPUQuota=720%",         // 8 * 90
		"MemoryMax=15461882265", // 16384 * 1024 * 1024 * 90 / 100
		"systemctl daemon-reload",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("script missing %q\n%s", want, body)
		}
	}
	if !m.cmds[0].Opts.Sudo {
		t.Error("slice cap must run as sudo")
	}
}

func TestCapMachineSlice_PropagatesError(t *testing.T) {
	m := &mockRunner{failOn: map[string]error{"set -e": errors.New("read-only fs")}}
	err := CapMachineSlice(context.Background(), m, HypervisorResources{CPUCores: 4, RAMMB: 8192})
	if err == nil || !strings.Contains(err.Error(), "read-only fs") {
		t.Errorf("expected propagated error, got %v", err)
	}
}
