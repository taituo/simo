// Command simo generates logs, metrics and events from a seed file.
//
//	simo validate seed.yaml
//	simo preview  seed.yaml [--for 1h] [--from 'day2 09:00']
//	simo run      seed.yaml [--for 1h] [--format text|jsonl|syslog] [--out DIR | --db DIR]
//	simo logs     DIR [--device NAME] [--level WARN] [--grep TEXT]
//	simo stats    WORLD [--by event,level] [--step 1h]
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/taituo/simo/internal/compile"
	"github.com/taituo/simo/internal/ir"
	"github.com/taituo/simo/internal/report"
	"github.com/taituo/simo/internal/spec"
)

// version is the CLI version; set with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

const usageText = `simo - math-driven log, metric and event simulator

Usage:
  simo validate SEED                  check a seed and lint it
  simo preview  SEED [flags]          simulate a short window and summarise it
  simo run      SEED [flags]          generate lines to stdout, files (--out) or a world (--db)
  simo logs     DIR  [flags]          render and filter records from --out files or a world

World commands (a directory built with run --db):
  simo stats       WORLD [flags]      event counts by event, level, device, site or class
  simo metrics     WORLD --device D   metric series for one device
  simo devices     WORLD              list devices
  simo materialize WORLD [flags]      store raw events for whole days
  simo truth       WORLD              ground truth: scheduled faults (never show to agents under test)

MCP server:
  simo serve [--role observer,author,admin] [--worlds DIR] [--world NAME|DIR] [--http ADDR]
             stdio by default; --http serves streamable HTTP at /mcp

  simo version

Time flags (preview, run, logs, stats, metrics, materialize):
  --from T    start of the window: "day2 09:15", "09:15" or an offset like 26h
  --to T      end of the window
  --for D     length of the window, e.g. 1h (preview defaults to 1h)

Run 'simo COMMAND -h' for a command's flags. Design: docs/design.md
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	var err error
	switch args[0] {
	case "validate":
		err = cmdValidate(args[1:], stdout, stderr)
	case "preview":
		err = cmdPreview(args[1:], stdout, stderr)
	case "run":
		err = cmdRun(args[1:], stdout, stderr)
	case "logs":
		err = cmdLogs(args[1:], stdout, stderr)
	case "stats":
		err = cmdStats(args[1:], stdout, stderr)
	case "metrics":
		err = cmdMetrics(args[1:], stdout, stderr)
	case "devices":
		err = cmdDevices(args[1:], stdout, stderr)
	case "materialize":
		err = cmdMaterialize(args[1:], stdout, stderr)
	case "truth":
		err = cmdTruth(args[1:], stdout, stderr)
	case "serve":
		err = cmdServe(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintf(stdout, "simo %s (engine %s)\n", version, ir.EngineVersion)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
	default:
		fmt.Fprintf(stderr, "simo: unknown command %q\n\n%s", args[0], usageText)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintln(stderr, "simo:", err)
		return 1
	}
}

var errUsage = errors.New("usage")

// parseArgs parses flags that may appear before or after positional
// arguments, and returns the positional ones.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func oneArg(fs *flag.FlagSet, args []string, what string) (string, error) {
	pos, err := parseArgs(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 {
		fmt.Fprintf(fs.Output(), "simo %s: want exactly one %s\n", fs.Name(), what)
		fs.Usage()
		return "", errUsage
	}
	return pos[0], nil
}

// loadWorld reads, validates and compiles a seed, printing warnings.
func loadWorld(path string, stderr io.Writer) (*ir.World, []byte, error) {
	s, raw, err := spec.Load(path)
	if err != nil {
		return nil, nil, err
	}
	for _, is := range spec.Validate(s) {
		if is.Severity == spec.Warning {
			fmt.Fprintln(stderr, is)
		}
	}
	w, err := compile.Compile(s, raw)
	if err != nil {
		return nil, nil, err
	}
	return w, raw, nil
}

// window holds the time flags.
type window struct {
	from, to, dur string
}

func (wd *window) register(fs *flag.FlagSet, defaultFor string) {
	fs.StringVar(&wd.from, "from", "", `window start: "day2 09:15", "09:15", an offset like 26h, or an RFC 3339 time`)
	fs.StringVar(&wd.to, "to", "", "window end (same forms as --from)")
	fs.StringVar(&wd.dur, "for", defaultFor, "window length, e.g. 1h")
}

// ticks resolves the window to [from, to) ticks of w.
func (wd *window) ticks(w *ir.World) (int64, int64, error) {
	from, to, err := report.Window(w, wd.from, wd.to, wd.dur)
	if err != nil {
		return 0, 0, fmt.Errorf("--%w", err)
	}
	return from, to, nil
}

func fmtOffset(d time.Duration) string { return report.Offset(d) }
func fmtSpan(d time.Duration) string   { return report.Span(d) }
func plural(n int, word string) string { return report.Plural(n, word) }
func commas(n int64) string            { return report.Commas(n) }

// loadQuiet compiles an already parsed and validated seed.
func loadQuiet(s *spec.Seed, raw []byte) (*ir.World, error) {
	return compile.Compile(s, raw)
}
