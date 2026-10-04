//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
)

func runCommandWithJob(cmd *exec.Cmd) (err error) {
	job, err := newKillOnCloseJob()
	if err != nil {
		return fmt.Errorf("create Node process job: %w", err)
	}
	defer func() {
		if closeErr := job.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close Node process job: %w", closeErr))
		}
	}()
	return cmd.Run()
}
