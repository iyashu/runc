package ebpf

import (
	"fmt"
	"runtime"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func nilCloser() error {
	return nil
}

func findAttachedCgroupDeviceFilters(dirFd int) ([]*ebpf.Program, error) {
	type bpfAttrQuery struct {
		TargetFd    uint32
		AttachType  uint32
		QueryType   uint32
		AttachFlags uint32
		ProgIds     uint64 // __aligned_u64
		ProgCnt     uint32
	}

	// Currently you can only have 64 eBPF programs attached to a cgroup.
	size := 64
	retries := 0
	for retries < 10 {
		progIds := make([]uint32, size)
		query := bpfAttrQuery{
			TargetFd:   uint32(dirFd),
			AttachType: uint32(unix.BPF_CGROUP_DEVICE),
			ProgIds:    uint64(uintptr(unsafe.Pointer(&progIds[0]))),
			ProgCnt:    uint32(len(progIds)),
		}

		// Fetch the list of program ids.
		_, _, errno := unix.Syscall(unix.SYS_BPF,
			uintptr(unix.BPF_PROG_QUERY),
			uintptr(unsafe.Pointer(&query)),
			unsafe.Sizeof(query))
		size = int(query.ProgCnt)
		runtime.KeepAlive(query)
		if errno != 0 {
			// On ENOSPC we get the correct number of programs.
			if errno == unix.ENOSPC {
				retries++
				continue
			}
			return nil, fmt.Errorf("bpf_prog_query(BPF_CGROUP_DEVICE) failed: %w", errno)
		}

		// Convert the ids to program handles.
		progIds = progIds[:size]
		programs := make([]*ebpf.Program, len(progIds))
		for idx, progId := range progIds {
			program, err := ebpf.NewProgramFromID(ebpf.ProgramID(progId))
			if err != nil {
				return nil, fmt.Errorf("cannot fetch program from id: %w", err)
			}
			programs[idx] = program
		}
		runtime.KeepAlive(progIds)
		return programs, nil
	}

	return nil, errors.New("could not get complete list of CGROUP_DEVICE programs")
}

// LoadAttachCgroupDeviceFilter installs eBPF device filter program to /sys/fs/cgroup/<foo> directory.
//
// Requires the system to be running in cgroup2 unified-mode with kernel >= 4.15 .
//
// https://github.com/torvalds/linux/commit/ebc614f687369f9df99828572b1d85a7c2de3d92
func LoadAttachCgroupDeviceFilter(insts asm.Instructions, license string, dirFd int) (func() error, error) {
	// Increase `ulimit -l` limit to avoid BPF_PROG_LOAD error (#2167).
	// This limit is not inherited into the container.
	memlockLimit := &unix.Rlimit{
		Cur: unix.RLIM_INFINITY,
		Max: unix.RLIM_INFINITY,
	}
	_ = unix.Setrlimit(unix.RLIMIT_MEMLOCK, memlockLimit)
	// Get the list of existing programs.
	oldProgs, err := findAttachedCgroupDeviceFilters(dirFd)
	if err != nil {
		return nilCloser, err
	}
	spec := &ebpf.ProgramSpec{
		Type:         ebpf.CGroupDevice,
		Instructions: insts,
		License:      license,
	}
	prog, err := ebpf.NewProgram(spec)
	if err != nil {
		return nilCloser, err
	}
	// If there is only one old program, we can just replace it directly.
	var replaceProg *ebpf.Program
	if len(oldProgs) == 1 {
		replaceProg = oldProgs[0]
	}
	err = link.RawAttachProgram(link.RawAttachProgramOptions{
		Target:  dirFd,
		Program: prog,
		Replace: replaceProg,
		Attach:  ebpf.AttachCGroupDevice,
		Flags:   unix.BPF_F_ALLOW_MULTI,
	})
	if err != nil {
		return nilCloser, fmt.Errorf("failed to call BPF_PROG_ATTACH (BPF_CGROUP_DEVICE, BPF_F_ALLOW_MULTI): %w", err)
	}
	closer := func() error {
		err = link.RawDetachProgram(link.RawDetachProgramOptions{
			Target:  dirFd,
			Program: prog,
			Attach:  ebpf.AttachCGroupDevice,
		})
		if err != nil {
			return fmt.Errorf("failed to call BPF_PROG_DETACH (BPF_CGROUP_DEVICE): %w", err)
		}
		// TODO: Should we attach the old filters back in this case? Otherwise
		//       we fail-open on a security feature, which is a bit scary.
		return nil
	}
	// If there was more than one old program, give a warning (since this
	// really shouldn't happen with runc-managed cgroups) and then detach all
	// the old programs.
	if len(oldProgs) > 1 {
		logrus.Warnf("found more than one filter (%d) attached to a cgroup -- removing extra filters!", len(oldProgs))
		for _, oldProg := range oldProgs {
			err = link.RawDetachProgram(link.RawDetachProgramOptions{
				Target:  dirFd,
				Program: oldProg,
				Attach:  ebpf.AttachCGroupDevice,
			})
			if err != nil {
				return closer, fmt.Errorf("failed to call BPF_PROG_DETACH (BPF_CGROUP_DEVICE) on old filter program: %w", err)
			}
		}
	}
	return closer, nil
}
