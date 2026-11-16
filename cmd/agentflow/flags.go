package main

// One way to declare, parse and document a command's flags.
//
// Every command builds its flag set with newCommand, which ties the set to the
// command's synopsis so `agentflow help <command>` and `<command> -h` print
// help generated from the flags that actually exist: the text cannot drift
// from the code. parseArgs then does the three things each command used to do
// differently, or not at all: it names the replacement for a removed flag, it
// accepts flags after positional arguments, and it reports -h as success.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// cmdInfo is what a command's help says before its flags.
type cmdInfo struct {
	Name    string // "user export"
	Args    string // positional synopsis, e.g. "USER"
	Summary string // one line
	Detail  string // optional paragraph
}

// removedFlags are flags that no longer exist, with what to use instead. They
// apply to every command; a command with its own removals passes them to
// parseArgs, and those win.
var removedFlags = map[string]string{
	"configdir": "use -config DIR",
	"log-json":  "use -log-format json",
	"no-webui":  "use -no-console",
	"workers":   "set runtime.scheduler.workers in the config",
}

// newCommand builds a command's flag set. Errors and usage go to w.
func newCommand(info cmdInfo, w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(info.Name, flag.ContinueOnError)
	fs.SetOutput(w)
	fs.Usage = func() { printCommandHelp(fs.Output(), info, fs) }
	return fs
}

func printCommandHelp(w io.Writer, info cmdInfo, fs *flag.FlagSet) {
	synopsis := "agentflow " + info.Name
	hasFlags := false
	fs.VisitAll(func(*flag.Flag) { hasFlags = true })
	if hasFlags {
		synopsis += " [flags]"
	}
	if info.Args != "" {
		synopsis += " " + info.Args
	}
	fmt.Fprintf(w, "usage: %s\n\n%s\n", synopsis, info.Summary)
	if info.Detail != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimRight(info.Detail, "\n"))
	}
	if hasFlags {
		fmt.Fprintln(w, "\nflags:")
		fs.PrintDefaults()
	}
}

// addConfigFlag registers the -config flag every config-reading command shares.
func addConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "", "config file or directory (default: agentflow.yaml, then config.yaml)")
}

// parseArgs parses args into fs. ok is false when the command should return
// code straight away: 0 after -h, 2 after a usage error (already reported).
func parseArgs(fs *flag.FlagSet, args []string, removed map[string]string) (code int, ok bool) {
	for _, a := range args {
		if a == "--" {
			break
		}
		name, isFlag := flagName(a)
		if !isFlag || fs.Lookup(name) != nil {
			continue
		}
		hint, gone := removed[name]
		if !gone {
			hint, gone = removedFlags[name]
		}
		if gone {
			fmt.Fprintf(fs.Output(), "agentflow %s: -%s was removed; %s\n", fs.Name(), name, hint)
			return 2, false
		}
	}
	if err := fs.Parse(hoist(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	return 0, true
}

// flagName reports the flag an argument names: "-x", "--x" and "-x=v" all name
// x. A lone "-" is a positional argument.
func flagName(arg string) (string, bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", false
	}
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name, name != ""
}

// hoist moves flags ahead of positional arguments, so `cred set NAME -user U`
// parses like `cred set -user U NAME`: Go's flag package stops at the first
// positional, and people write the subject first. It asks fs which flags are
// boolean, so a boolean never swallows the argument after it. An argument after
// "--" is always positional.
func hoist(fs *flag.FlagSet, args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		name, isFlag := flagName(a)
		if !isFlag {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue // unknown: let Parse report it
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	if len(rest) == 0 {
		return flags
	}
	return append(append(flags, "--"), rest...)
}

// positionals checks the argument count. want is the exact count; it reports
// the problem with the command's synopsis and returns false when it is wrong.
func positionals(fs *flag.FlagSet, info cmdInfo, want int) bool {
	if fs.NArg() == want {
		return true
	}
	switch {
	case fs.NArg() > want:
		fmt.Fprintf(fs.Output(), "agentflow %s: unexpected argument %q\n", info.Name, fs.Arg(want))
	case info.Args != "":
		fmt.Fprintf(fs.Output(), "agentflow %s: missing %s\n", info.Name, info.Args)
	}
	fmt.Fprintf(fs.Output(), "run `agentflow help %s` for usage\n", info.Name)
	return false
}

// writeOutput writes data to path, or to stdout when path is empty. -out means
// the same thing on every command that has it.
func writeOutput(path string, data []byte, perm os.FileMode, stdout io.Writer) error {
	if path == "" {
		_, err := stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, perm)
}

// subcommand is one entry of a command group (config, loop, user, ...).
type subcommand struct {
	name, summary string
	run           func(args []string, stdout, stderr io.Writer) int
}

// runGroup dispatches a command group. With no subcommand it prints the group's
// usage as an error; with -h, --help or help it prints it as help.
func runGroup(group string, subs []subcommand, args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "usage: agentflow %s <command> [flags]\n\ncommands:\n", group)
		for _, s := range subs {
			fmt.Fprintf(w, "  %-16s %s\n", s.name, s.summary)
		}
		fmt.Fprintf(w, "\nrun `agentflow help %s <command>` for a command's flags\n", group)
	}
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		usage(stderr)
		return 0
	}
	// Flags may come before the subcommand (`cred -config X set NAME`): the
	// first argument that names a subcommand is the subcommand.
	if strings.HasPrefix(args[0], "-") {
		for i, a := range args {
			for _, s := range subs {
				if a == s.name {
					rest := append(append([]string{}, args[:i]...), args[i+1:]...)
					return s.run(rest, stdout, stderr)
				}
			}
		}
	}
	for _, s := range subs {
		if s.name == args[0] {
			return s.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "agentflow %s: unknown command %q\n\n", group, args[0])
	usage(stderr)
	return 2
}
