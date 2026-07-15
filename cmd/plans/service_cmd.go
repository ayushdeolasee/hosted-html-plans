package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/ayushdeolasee/hosted-html-plans/service"
)

// cmdService dispatches `plans service install|uninstall|status` into the
// service package, which owns the launchd/systemd logic (see
// service/service.go and plan.html §5).
func cmdService(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: plans service install|uninstall|status [-user] [-print]")
		return 2
	}
	sub := args[0]

	fs := flag.NewFlagSet("service "+sub, flag.ExitOnError)
	userMode := fs.Bool("user", false, "Linux only: install/uninstall/query a user-level systemd unit")
	printOnly := fs.Bool("print", false, "dry run: print what would be installed without touching the system")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	opts := service.Options{
		UserMode: *userMode,
		DryRun:   *printOnly || os.Getenv("PLANS_SERVICE_DRYRUN") != "",
	}

	switch sub {
	case "install":
		out, err := service.Install(opts)
		if out != "" {
			fmt.Println(out)
		}
		return exitFor(err, "install")
	case "uninstall":
		out, err := service.Uninstall(opts)
		if out != "" {
			fmt.Println(out)
		}
		return exitFor(err, "uninstall")
	case "status":
		rep, err := service.Status(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "plans service status: %v\n", err)
			return 1
		}
		service.PrintStatus(os.Stdout, rep)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "plans service: unknown subcommand %q\n", sub)
		return 2
	}
}

// exitFor turns an install/uninstall error into an exit code. ErrNeedsRoot
// means the human-readable instructions were already printed to stdout —
// no need to also dump the error to stderr, just fail the exit code.
func exitFor(err error, verb string) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, service.ErrNeedsRoot) {
		return 1
	}
	fmt.Fprintf(os.Stderr, "plans service %s: %v\n", verb, err)
	return 1
}
