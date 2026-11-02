// Command cronkite is a maintenance runner for Linux and Windows servers.
//
//	plan    show what a run would do, touching nothing
//	run     execute a run (--apply) or rehearse it
//	check   validate configuration and preconditions
//	report  read a previous run report
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/durationx"
	"github.com/jhuancz/cronkite/internal/engine"
	"github.com/jhuancz/cronkite/internal/exitcode"
	"github.com/jhuancz/cronkite/internal/report"
	"github.com/jhuancz/cronkite/internal/tasks"
	"github.com/jhuancz/cronkite/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitcode.Error
	}

	// Global flag parsing per subcommand keeps "-h" meaningful everywhere.
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "cronkite %s\n", version.String())
		fmt.Fprintf(stdout, "built %s for %s/%s\n", version.Date, runtime.GOOS, runtime.GOARCH)
		return exitcode.OK
	case "help", "-h", "--help":
		usage(stdout)
		return exitcode.OK
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "plan":
		return cmdRun(args[1:], stdout, stderr, false)
	case "run":
		return cmdRun(args[1:], stdout, stderr, true)
	case "report":
		return cmdReport(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "cronkite: unknown command %q\n\n", args[0])
		usage(stderr)
		return exitcode.Error
	}
}

// commonFlags are shared by plan and run.
type commonFlags struct {
	config string
	json   bool
	force  bool
	tasks  taskList
}

func newCommonFlags(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.config, "config", "", "configuration file (default: search standard locations)")
	fs.StringVar(&c.config, "c", "", "shorthand for -config")
	fs.BoolVar(&c.json, "json", false, "write the run report to stdout as JSON")
	fs.BoolVar(&c.force, "force", false, "bypass the safety ceilings (recorded in the report)")
	fs.Var(&c.tasks, "task", "run only the named task (repeatable)")
	return c
}

func cmdRun(args []string, stdout, stderr io.Writer, allowApply bool) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := newCommonFlags(fs)
	apply := fs.Bool("apply", false, "perform the changes instead of rehearsing them")
	timeout := fs.Duration("timeout", 0, "override run.timeout from the configuration")
	if err := fs.Parse(args); err != nil {
		return exitcode.Error
	}
	if *apply && !allowApply {
		fmt.Fprintln(stderr, "cronkite: plan does not accept --apply; use \"cronkite run --apply\"")
		return exitcode.Error
	}
	if !*apply && allowApply {
		fmt.Fprintln(stderr, "cronkite: run without --apply rehearses the run and changes nothing")
	}

	cfg, code := loadConfig(common.config, stderr)
	if cfg == nil {
		return code
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	limit := *timeout
	if limit == 0 {
		parsed, err := durationx.Parse(cfg.Run.Timeout)
		if err != nil {
			fmt.Fprintf(stderr, "cronkite: run.timeout: %v\n", err)
			return exitcode.Error
		}
		limit = parsed
	}
	if limit > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, limit)
		defer stop()
	}

	rep, err := engine.Run(ctx, cfg, engine.Options{
		ConfigPath: cfgPathForReport(common.config),
		Only:       common.tasks,
		Apply:      *apply,
		Force:      common.force,
	})
	if err != nil {
		fmt.Fprintf(stderr, "cronkite: %v\n", err)
		return exitcode.Error
	}

	if common.json {
		if err := rep.Write(stdout); err != nil {
			fmt.Fprintf(stderr, "cronkite: %v\n", err)
			return exitcode.Error
		}
	} else if err := report.Human(stdout, rep); err != nil {
		fmt.Fprintf(stderr, "cronkite: %v\n", err)
		return exitcode.Error
	}

	if !common.json {
		fmt.Fprintf(stdout, "\n  report: %s\n", filepath.Join(cfg.Report.Dir, "run-"+rep.RunID+".json"))
	}
	return rep.ExitCode
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := newCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitcode.Error
	}

	cfg, code := loadConfig(common.config, stderr)
	if cfg == nil {
		return code
	}

	problems := 0
	warnings := 0
	note := func(format string, argsValue ...any) {
		problems++
		fmt.Fprintf(stdout, "  FAIL  "+format+"\n", argsValue...)
	}
	warn := func(format string, argsValue ...any) {
		warnings++
		fmt.Fprintf(stdout, "  WARN  "+format+"\n", argsValue...)
	}
	ok := func(format string, argsValue ...any) {
		fmt.Fprintf(stdout, "  ok    "+format+"\n", argsValue...)
	}

	fmt.Fprintf(stdout, "cronkite %s  check\n\n", version.String())
	ok("%s (%d tasks: %s)", cfgPathForReport(common.config), len(cfg.Tasks), strings.Join(cfg.TaskNames(), ", "))

	for _, dir := range []string{cfg.Report.Dir, filepath.Dir(cfg.Run.LockFile)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			note("state directory %s is not writable: %v", dir, err)
			continue
		}
		ok("writable: %s", dir)
	}

	for _, task := range cfg.Tasks {
		if !task.IsEnabled() {
			ok("task %s: disabled", task.Name)
			continue
		}
		for _, unset := range task.UnresolvedRoots {
			warn("task %s: root %q depends on a variable that is not set here; it will be skipped", task.Name, unset)
		}

		usable := 0
		for _, root := range task.Roots {
			info, err := os.Stat(root)
			switch {
			case errors.Is(err, os.ErrNotExist):
				warn("task %s: root %s does not exist; it will be skipped", task.Name, root)
			case err != nil:
				note("task %s: root %s: %v", task.Name, root, err)
			case !info.IsDir():
				note("task %s: root %s is not a directory", task.Name, root)
			default:
				ok("task %s: root %s", task.Name, root)
				usable++
			}
		}
		// One missing root is normal in a cross-platform file. Every root
		// missing means the task is dead weight and the operator should know.
		if usable == 0 && len(task.Roots) > 0 {
			note("task %s: no configured root exists, so this task can never do anything", task.Name)
		}

		// Build the task, which is what a run does before it touches anything. It
		// is the only way to catch what the file cannot show: an interpreter that
		// is not installed, a destination that cannot be opened. Discovering that
		// at 03:30 is the failure this command exists to prevent.
		runner, buildErr := tasks.Build(task)
		if buildErr != nil {
			// The constructor already names the task; prefixing it again reads as a
			// stutter in the one message an operator has to act on.
			note("%v", buildErr)
			continue
		}
		_ = runner.Close()
		ok("task %s: can be built", task.Name)
	}

	if problems > 0 {
		fmt.Fprintf(stdout, "\n%d problem(s) and %d warning(s); fix the problems before scheduling this run\n", problems, warnings)
		return exitcode.Error
	}
	if warnings > 0 {
		fmt.Fprintf(stdout, "\nconfiguration is valid, with %d warning(s). skipped roots are recorded in every run report. available task types: %s\n",
			warnings, strings.Join(tasks.Types, ", "))
		return exitcode.OK
	}
	fmt.Fprintf(stdout, "\nconfiguration is valid; available task types: %s\n", strings.Join(tasks.Types, ", "))
	return exitcode.OK
}

func cmdReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	common := newCommonFlags(fs)
	path := fs.String("path", "", "report file to read (default: the latest run)")
	if err := fs.Parse(args); err != nil {
		return exitcode.Error
	}

	target := *path
	if target == "" {
		cfg, code := loadConfig(common.config, stderr)
		if cfg == nil {
			return code
		}
		target = filepath.Join(cfg.Report.Dir, "latest.json")
	}

	rep, err := report.ReadFile(target)
	if err != nil {
		fmt.Fprintf(stderr, "cronkite: %v\n", err)
		return exitcode.Error
	}

	if common.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "cronkite: %v\n", err)
			return exitcode.Error
		}
		return exitcode.OK
	}
	if err := report.Human(stdout, rep); err != nil {
		fmt.Fprintf(stderr, "cronkite: %v\n", err)
		return exitcode.Error
	}
	return exitcode.OK
}

// loadConfig resolves the configuration path, prints a readable diagnostic, and
// returns nil with an exit code when the configuration is unusable.
func loadConfig(explicit string, stderr io.Writer) (*config.Config, int) {
	path, err := resolveConfig(explicit)
	if err != nil {
		fmt.Fprintf(stderr, "cronkite: %v\n", err)
		return nil, exitcode.Error
	}
	cfg, err := config.Load(path)
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			fmt.Fprintf(stderr, "cronkite: %v\n", err)
			return nil, exitcode.Error
		}
		fmt.Fprintf(stderr, "cronkite: %v\n", err)
		return nil, exitcode.Error
	}
	return cfg, exitcode.OK
}

// resolveConfig applies the documented search order so a scheduled run needs no
// flags and a development run needs no root-owned file.
func resolveConfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	var candidates []string
	candidates = append(candidates, "cronkite.conf")
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "cronkite", "cronkite.conf"))
	}
	if home, err := os.UserHomeDir(); err == nil && runtime.GOOS != "windows" {
		candidates = append(candidates, filepath.Join(home, ".config", "cronkite", "cronkite.conf"))
	}
	if runtime.GOOS == "windows" {
		if dir := os.Getenv("PROGRAMDATA"); dir != "" {
			candidates = append(candidates, filepath.Join(dir, "cronkite", "cronkite.conf"))
		}
	} else {
		candidates = append(candidates, "/etc/cronkite/cronkite.conf")
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no configuration file found (looked in: %s); pass -config or copy cronkite.conf.example", strings.Join(candidates, ", "))
}

func cfgPathForReport(explicit string) string {
	if explicit != "" {
		return explicit
	}
	path, err := resolveConfig("")
	if err != nil {
		return "(unknown)"
	}
	return path
}

// taskList collects repeated -task flags.
type taskList []string

func (t *taskList) String() string { return strings.Join(*t, ",") }

func (t *taskList) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*t = append(*t, part)
		}
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `cronkite %s — auditable maintenance runs for Linux and Windows servers

usage:
  cronkite <command> [flags]

commands:
  plan      show what a run would do; changes nothing
  run       execute the configured tasks (add --apply to change anything)
  check     validate the configuration and its preconditions
  report    read a previous run report
  version   print build information

common flags:
  -config, -c <path>   configuration file (default: search standard locations)
  -task <name>         restrict the run to a task; repeatable or comma separated
  -json                print the run report as JSON
  -force               bypass the safety ceilings; recorded in the report

run flags:
  -apply               perform the changes (without it, run rehearses)
  -timeout <duration>  override run.timeout from the configuration

exit codes:
  0 ok   1 error   2 partial   3 locked   4 refused (see docs/exit-codes.md)

examples:
  cronkite check -c /etc/cronkite/cronkite.conf
  cronkite plan
  cronkite run --apply -task logs
  cronkite report -json
`, version.Version)
}
