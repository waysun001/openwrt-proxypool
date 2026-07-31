package main

import (
	"fmt"
	"os"

	"proxypoold/internal/buildinfo"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.Version)
		return
	}

	fmt.Fprintln(os.Stderr, "proxypoolctl: foundation shadow only")
	os.Exit(1)
}
