package processgroup

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const terminationGracePeriod = 2 * time.Second
const processGroupPollInterval = 20 * time.Millisecond

func Configure(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func Wait(ctx context.Context, command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return errors.New("cannot wait for a process group before the command starts")
	}

	waitResult := make(chan error, 1)
	go func() {
		waitResult <- command.Wait()
	}()

	select {
	case waitErr := <-waitResult:
		return waitErr
	case <-ctx.Done():
	}

	processGroupID := command.Process.Pid
	_ = signal(processGroupID, syscall.SIGTERM)

	terminationTimer := time.NewTimer(terminationGracePeriod)
	defer terminationTimer.Stop()
	pollTimer := time.NewTicker(processGroupPollInterval)
	defer pollTimer.Stop()

	var waitErr error
	commandFinished := false
	for {
		if commandFinished && !exists(processGroupID) {
			return ctx.Err()
		}
		select {
		case waitErr = <-waitResult:
			commandFinished = true
			_ = waitErr
		case <-pollTimer.C:
		case <-terminationTimer.C:
			_ = signal(processGroupID, syscall.SIGKILL)
			if !commandFinished {
				<-waitResult
			}
			return ctx.Err()
		}
	}
}

func GroupExists(processGroupID int) bool {
	if processGroupID <= 0 {
		return false
	}
	return exists(processGroupID)
}

func WaitForGroupExit(ctx context.Context, processGroupID int) error {
	if processGroupID <= 0 {
		return nil
	}
	ticker := time.NewTicker(processGroupPollInterval)
	defer ticker.Stop()
	for {
		if !exists(processGroupID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TerminateGroup(processGroupID int) error {
	if processGroupID <= 0 {
		return nil
	}
	return signal(processGroupID, syscall.SIGTERM)
}

func Terminate(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return signal(command.Process.Pid, syscall.SIGTERM)
}

func signal(processGroupID int, signal syscall.Signal) error {
	err := syscall.Kill(-processGroupID, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func exists(processGroupID int) bool {
	return syscall.Kill(-processGroupID, 0) == nil
}
