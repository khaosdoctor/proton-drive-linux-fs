// Command proton-drive-fs mounts Proton Drive as a local FUSE filesystem.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, "usage: proton-drive-fs <login|mount|unmount|logout|version> [args]")
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}

	switch args[0] {
	case "login":
		return runLogin()
	case "mount":
		return runMount(args[1:])
	case "unmount":
		return runUnmount(args[1:])
	case "logout":
		return runLogout()
	case "version":
		return runVersion()
	default:
		usage()
		return 2
	}
}

func runLogin() int {
	fmt.Println("not implemented")
	return 2
}

func runMount(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: proton-drive-fs mount <mountpoint>")
		return 2
	}
	fmt.Println("not implemented")
	return 2
}

func runUnmount(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: proton-drive-fs unmount <mountpoint>")
		return 2
	}
	fmt.Println("not implemented")
	return 2
}

func runLogout() int {
	fmt.Println("not implemented")
	return 2
}

func runVersion() int {
	fmt.Println(version)
	return 0
}
