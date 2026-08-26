package main

import (
	"fmt"
	"os"

	"github.com/omnilsp/omni/internal/conformance"
)

func main() {
	rep, err := conformance.FastReport(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(conformance.RenderText(rep))
}
