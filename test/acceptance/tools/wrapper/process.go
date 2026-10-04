package main

import "os"

type processJob interface {
	Assign(*os.Process) error
	Close() error
}
