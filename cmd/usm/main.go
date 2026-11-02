package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/teinfai/Universal-server-manager/internal/config"
	"github.com/teinfai/Universal-server-manager/internal/job"
)

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: usm run [--now] [--config <path>]")
	fmt.Fprintln(os.Stderr, "       usm status [--config <path>]")
	fmt.Fprintln(os.Stderr, "       usm resume [--config <path>]")
	fmt.Fprintln(os.Stderr, "       usm restore [--yes] [--config <path>]")
	fmt.Fprintln(os.Stderr, "       usm test [--config <path>]")
}

func execute(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	command := args[0]
	switch command {
	case "run", "status", "resume", "restore", "test":
	default:
		usage()
		return 2
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.Usage = usage
	path := flags.String("config", "/etc/usm/config.yaml", "config file")
	var now, yes bool
	if command == "run" {
		flags.BoolVar(&now, "now", false, "ignore date and time guards")
	}
	if command == "restore" {
		flags.BoolVar(&yes, "yes", false, "perform the restore")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage()
		return 2
	}
	c, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	j := job.New(c)
	defer j.Close()
	switch command {
	case "run":
		return j.Run(now)
	case "status":
		return j.Status()
	case "resume":
		return j.Resume()
	case "restore":
		return j.Restore(yes)
	case "test":
		return j.Test()
	}
	return 2
}

func main() { os.Exit(execute(os.Args[1:])) }
