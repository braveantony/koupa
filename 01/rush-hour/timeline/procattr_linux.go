package timeline

import "syscall"

// childAttr puts kubectl in its own process group, so a signal to the
// follower's group (collect-timeline.sh stop) does not reach it directly; it
// is ended through the follower's context, or killed if the follower dies.
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
