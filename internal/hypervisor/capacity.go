package hypervisor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Pledge is the aggregate of a set of VMs' allocated CPU + memory +
// disk. Used for Layer 1 admission control on create-vm: we sum the
// existing pledges from the hypervisor, add the new request, and
// refuse if the result would push the host past 90% of any axis.
type Pledge struct {
	CPUCores int
	RAMMB    int64
	DiskGB   int64
}

// QueryExistingPledge walks every VM defined on the hypervisor (not
// just running ones — stopped VMs still hold disk and would consume
// memory + CPU when booted) and tallies their allocations. CPU + RAM
// come from `virsh dominfo`; disk from libvirt's default storage pool
// volume list.
func QueryExistingPledge(ctx context.Context, r Runner) (Pledge, error) {
	var p Pledge

	names, err := listDomainNames(ctx, r)
	if err != nil {
		return p, fmt.Errorf("list domains: %w", err)
	}
	for _, name := range names {
		info, err := readDomainInfo(ctx, r, name)
		if err != nil {
			// One bad domain shouldn't refuse the whole admission
			// check — skip + continue. The aggregate will be a
			// slight under-count which biases toward generosity, but
			// the next create-vm will see the missing VM removed and
			// the count will catch up.
			continue
		}
		p.CPUCores += info.CPUCores
		p.RAMMB += info.RAMMB
	}
	disk, err := totalPoolCapacityGB(ctx, r)
	if err == nil {
		p.DiskGB = disk
	}
	return p, nil
}

// listDomainNames returns the names of every libvirt domain on the
// hypervisor. Empty list (no VMs yet) is fine — pledge stays zero.
func listDomainNames(ctx context.Context, r Runner) ([]string, error) {
	var sb stringBuilder
	if err := r.Run(ctx, `virsh list --all --name`, RunOptions{Stdout: &sb}); err != nil {
		return nil, err
	}
	out := []string{}
	for _, line := range strings.Split(strings.TrimSpace(sb.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// domainInfo is the subset of `virsh dominfo` we care about for
// admission decisions.
type domainInfo struct {
	CPUCores int
	RAMMB    int64
}

// readDomainInfo parses `virsh dominfo <name>` and pulls out CPU
// count + max memory. Lines look like:
//
//	CPU(s):         4
//	Max memory:     8388608 KiB
//
// Memory is reported in KiB by libvirt — the converter divides by
// 1024 to get the MB scale used by the rest of the package.
func readDomainInfo(ctx context.Context, r Runner, name string) (domainInfo, error) {
	var info domainInfo
	var sb stringBuilder
	cmd := fmt.Sprintf("virsh dominfo %s", shellQuoteArg(name))
	if err := r.Run(ctx, cmd, RunOptions{Stdout: &sb}); err != nil {
		return info, err
	}
	for _, line := range strings.Split(sb.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "CPU(s):"):
			if v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "CPU(s):"))); err == nil {
				info.CPUCores = v
			}
		case strings.HasPrefix(line, "Max memory:"):
			rest := strings.TrimSpace(strings.TrimPrefix(line, "Max memory:"))
			fields := strings.Fields(rest)
			if len(fields) >= 1 {
				if v, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					info.RAMMB = v / 1024 // KiB → MB
				}
			}
		}
	}
	return info, nil
}

// totalPoolCapacityGB sums the capacity of every volume in libvirt's
// default storage pool. virsh vol-list --details prints columns:
//
//	Name           Path                            Type    Capacity     Allocation
//	---------------------------------------------------------------------------
//	vm1.qcow2      /var/lib/libvirt/images/...     file    50.00 GiB    32.00 GiB
//
// We parse the "Capacity" column. Errors fall through to a 0 result —
// admission then lacks disk-axis info but CPU/RAM still gate.
func totalPoolCapacityGB(ctx context.Context, r Runner) (int64, error) {
	var sb stringBuilder
	if err := r.Run(ctx, `virsh vol-list --pool default --details 2>/dev/null`, RunOptions{Stdout: &sb}); err != nil {
		return 0, err
	}
	var total int64
	scanning := false
	for _, line := range strings.Split(sb.String(), "\n") {
		if !scanning {
			if strings.HasPrefix(strings.TrimSpace(line), "---") {
				scanning = true
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// Capacity = fields[3] + fields[4] (e.g. "50.00 GiB").
		cap := fields[3] + " " + fields[4]
		if gib, ok := parseGiB(cap); ok {
			total += gib
		}
	}
	return total, nil
}

// parseGiB converts strings like "50.00 GiB", "1.50 TiB", "512 MiB"
// to GiB (truncated to int64). Returns ok=false for unrecognized
// input — caller treats that as "no contribution to the disk total".
func parseGiB(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0, false
	}
	val, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, false
	}
	switch parts[1] {
	case "TiB":
		return int64(val * 1024), true
	case "GiB":
		return int64(val), true
	case "MiB":
		return int64(val) / 1024, true
	default:
		return 0, false
	}
}

// CheckAdmission applies the 90% rule to a proposed Pledge. host is
// what the hypervisor has total; existing is what's already pledged
// to other VMs; want is the new VM's request. Returns a clear,
// quantitative error when any axis would breach the cap.
func CheckAdmission(host HypervisorResources, existing, want Pledge) error {
	checkAxis := func(axis string, hostTotal int64, used int64, asked int64) error {
		cap := hostTotal * 90 / 100
		total := used + asked
		if total > cap {
			return fmt.Errorf("create-vm: %s admission failed — requested %d + already pledged %d = %d would exceed 90%% cap (%d of %d)",
				axis, asked, used, total, cap, hostTotal)
		}
		return nil
	}
	if err := checkAxis("CPU cores", int64(host.CPUCores), int64(existing.CPUCores), int64(want.CPUCores)); err != nil {
		return err
	}
	if err := checkAxis("RAM (MB)", host.RAMMB, existing.RAMMB, want.RAMMB); err != nil {
		return err
	}
	if host.DiskGB > 0 { // skip disk axis when DetectResources couldn't measure
		if err := checkAxis("disk (GB)", host.DiskGB, existing.DiskGB, want.DiskGB); err != nil {
			return err
		}
	}
	return nil
}
