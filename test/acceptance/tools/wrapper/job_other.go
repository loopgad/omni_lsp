//go:build !windows

package main

import "os"

type noOpJob struct{}

func newKillOnCloseJob() (processJob, error) { return noOpJob{}, nil }

func (noOpJob) Assign(*os.Process) error { return nil }

func (noOpJob) Close() error { return nil }
