package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	litefsgo "github.com/superfly/litefs-go"
)

// RunCommand represents a command to run a program with the HALT lock.
type RunCommand struct {
	// The database to acquire a halt lock on.
	WithHaltLockOn string

	// Subcommand & args
	Cmd  string
	Args []string

	// If true, enables verbose logging.
	Verbose bool
}

// NewRunCommand returns a new instance of RunCommand.
func NewRunCommand() *RunCommand {
	return &RunCommand{}
}

// ParseFlags parses the command line flags & config file.
func (c *RunCommand) ParseFlags(ctx context.Context, args []string) (err error) {
	// Split the args list if there is a double dash arg included.
	args0, args1 := splitArgs(args)

	fs := flag.NewFlagSet("litefs-run", flag.ContinueOnError)
	fs.StringVar(&c.WithHaltLockOn, "with-halt-lock-on", "", "full database path to halt")
	fs.BoolVar(&c.Verbose, "v", false, "enable verbose logging")
	fs.Usage = func() {
		fmt.Println(`
The run command will execute a subcommand with certain guarantees provided by
LiteFS. Typically, this is executed with --with-halt-lock-on to acquire a HALT lock
so that write transactions can temporarily be executed on the local node.

Usage:

	litefs run [arguments] -- CMD [ARG...]

Arguments:
`[1:])
		fs.PrintDefaults()
		fmt.Println("")
	}
	if err := fs.Parse(args0); err != nil {
		return err
	} else if fs.NArg() == 0 && len(args1) == 0 {
		fs.Usage()
		return flag.ErrHelp
	} else if fs.NArg() > 0 {
		return fmt.Errorf("too many arguments, specify a '--' to specify an exec command")
	}

	if len(args1) == 0 {
		return fmt.Errorf("no subcommand specified")
	}
	c.Cmd, c.Args = args1[0], args1[1:]

	// Optionally disable logging.
	if !c.Verbose {
		log.SetOutput(io.Discard)
	}

	return nil
}

// Run executes the command.
func (c *RunCommand) Run(ctx context.Context) (err error) {
	// Acquire the halt lock on the given database, if specified.
	var f *os.File
	if c.WithHaltLockOn != "" {
		// Ensure database exists first.
		if _, err := os.Stat(c.WithHaltLockOn); os.IsNotExist(err) {
			return fmt.Errorf("database does not exist: %s", c.WithHaltLockOn)
		} else if err != nil {
			return err
		}

		// Attempt to lock the database.
		if f, err = os.OpenFile(c.WithHaltLockOn+"-lock", os.O_RDWR, 0666); os.IsNotExist(err) {
			return fmt.Errorf("lock file not available, are you sure %q is a LiteFS mount?", filepath.Dir(c.WithHaltLockOn))
		} else if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()

		t := time.Now()
		log.Printf("acquiring halt lock")
		if err := litefsgo.Halt(f); err != nil {
			return err
		}
		log.Printf("halt lock acquired in %s", time.Since(t))
	}

	// Execute subcommand.
	cmd := exec.CommandContext(ctx, c.Cmd, c.Args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if f != nil {
		cmd.ExtraFiles = []*os.File{f} // pass along, otherwise the file is flushed
	}
	if err := cmd.Run(); err != nil {
		return err
	}

	// Unhalt, if database specified.
	if f != nil {
		t := time.Now()
		log.Printf("releasing halt lock")
		if err := litefsgo.Unhalt(f); err != nil {
			return err
		}
		log.Printf("halt lock released in %s", time.Since(t))

		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}
