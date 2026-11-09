package timeline

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// childAttr puts kubectl in its own process group, so a signal to the
// follower's group (collect-timeline.sh stop) does not reach it directly; it
// is ended through the follower's context, or killed if the follower dies.
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// childExited reports whether our child pid has already exited, without
// reaping it (WNOWAIT), so exec.Cmd.Wait still collects its status. A child
// that is already reaped (ECHILD) has exited too.
func childExited(pid int) bool {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		switch err {
		case nil:
			return info.Signo == int32(unix.SIGCHLD) // zero: still running
		case unix.EINTR:
			continue
		case unix.ECHILD:
			return true
		default:
			return false
		}
	}
}
