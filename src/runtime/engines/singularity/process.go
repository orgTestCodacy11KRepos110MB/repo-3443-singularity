// Copyright (c) 2018, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package singularity

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"syscall"
	"unsafe"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/singularityware/singularity/src/pkg/sylog"
)

// StartProcess starts the process
func (engine *EngineOperations) StartProcess(masterConn net.Conn) error {
	isInstance := engine.EngineConfig.GetInstance()
	bootInstance := (isInstance && engine.EngineConfig.GetBootInstance())
	shimProcess := false

	if err := os.Chdir(engine.CommonConfig.OciConfig.Process.Cwd); err != nil {
		os.Chdir("/")
	}

	args := engine.CommonConfig.OciConfig.Process.Args
	env := engine.CommonConfig.OciConfig.Process.Env

	if engine.CommonConfig.OciConfig.Linux != nil {
		namespaces := engine.CommonConfig.OciConfig.Linux.Namespaces
		for _, ns := range namespaces {
			if ns.Type == specs.PIDNamespace {
				if !engine.EngineConfig.GetNoInit() {
					shimProcess = true
				}
				break
			}
		}
	}

	if (!isInstance && !shimProcess) || bootInstance || engine.EngineConfig.GetInstanceJoin() {
		err := syscall.Exec(args[0], args, env)
		return err
	}

	// Spawn and wait container process, signal handler
	cmd := exec.Command(args[0], args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = env

	var status syscall.WaitStatus
	errChan := make(chan error, 1)
	signals := make(chan os.Signal, 1)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec %s failed: %s", args[0], err)
	}

	go func() {
		errChan <- cmd.Wait()
	}()

	masterConn.Close()

	// Modify argv argument and program name shown in /proc/self/comm
	name := "sinit"

	argv0str := (*reflect.StringHeader)(unsafe.Pointer(&os.Args[0]))
	argv0 := (*[1 << 30]byte)(unsafe.Pointer(argv0str.Data))[:argv0str.Len]
	progname := make([]byte, argv0str.Len)

	if len(name) > argv0str.Len {
		return fmt.Errorf("program name too short")
	}

	copy(progname, name)
	copy(argv0, progname)

	ptr := unsafe.Pointer(&progname[0])
	if _, _, err := syscall.Syscall(syscall.SYS_PRCTL, syscall.PR_SET_NAME, uintptr(ptr), 0); err != 0 {
		return syscall.Errno(err)
	}

	// Manage all signals
	signal.Notify(signals)

	for {
		select {
		case s := <-signals:
			sylog.Debugf("Received signal %s", s.String())
			switch s {
			case syscall.SIGCHLD:
				for {
					wpid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
					if wpid <= 0 || err != nil {
						break
					}
				}
			case syscall.SIGCONT:
			default:
				if isInstance {
					syscall.Kill(-1, s.(syscall.Signal))
				} else {
					// kill ourself with SIGKILL whatever signal was received
					syscall.Kill(syscall.Gettid(), syscall.SIGKILL)
				}
			}
		case err := <-errChan:
			if e, ok := err.(*exec.ExitError); ok {
				if status, ok := e.Sys().(syscall.WaitStatus); ok {
					if status.Signaled() {
						syscall.Kill(syscall.Gettid(), syscall.SIGKILL)
					}
					os.Exit(status.ExitStatus())
				}
				return fmt.Errorf("command exit with error: %s", err)
			}
			if err != nil {
				os.Exit(1)
			}
			if !isInstance {
				os.Exit(0)
			}
		}
	}
}
