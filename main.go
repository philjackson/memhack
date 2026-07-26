// memhack is a scanmem-style interactive memory scanner for Linux. It attaches
// to a target process, scans its memory for values, progressively narrows the
// set of matches across scans, and can write new values back.
//
// Reading and writing another process's memory requires appropriate
// privileges (same-user with a permissive ptrace_scope, CAP_SYS_PTRACE, or
// root). Only use it on processes you own or are authorized to inspect.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phil/memhack/internal/memory"
	"github.com/phil/memhack/internal/scan"
)

func main() {
	pidFlag := flag.Int("pid", 0, "attach to this process id on startup")
	execFlag := flag.String("exec", "", "launch this program as a child and attach to it")
	typeFlag := flag.String("type", "i32", "initial data type (i8..u64, f32/f64, bytes, string)")
	watchFlag := flag.Duration("watch", defaultWatchInterval, "live-watch refresh interval in the TUI (e.g. 500ms, 2s)")
	freezeFlag := flag.Duration("freeze", defaultFreezeInterval, "how often frozen values are rewritten (e.g. 100ms)")
	alignFlag := flag.Int("align", 0, "scan alignment in bytes (0 = align to type width, fast; 1 = every byte, thorough)")
	replFlag := flag.Bool("repl", false, "use the line-based REPL instead of the TUI")
	flag.Parse()

	dt, err := scan.ParseDataType(*typeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if *replFlag {
		runREPL(dt, *pidFlag, *execFlag, flag.Args(), *alignFlag)
		return
	}
	if err := runTUI(dt, *pidFlag, *execFlag, flag.Args(), *watchFlag, *freezeFlag, *alignFlag); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// runREPL runs the line-based interface. ptrace binds the tracer to one OS
// thread, and the REPL is synchronous on this goroutine, so pin it here and
// every ptrace/mem call rides along.
func runREPL(dt scan.DataType, pid int, exec string, args []string, align int) {
	runtime.LockOSThread()

	app := &app{dtype: dt, align: align}
	switch {
	case exec != "":
		if err := app.launch(append([]string{exec}, args...)); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	case pid != 0:
		if err := app.attach(pid); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}

	fmt.Println("memhack — scanmem-style memory scanner. Type 'help' for commands.")
	app.repl()
}

// runTUI runs the Bubble Tea interface. ptrace work happens on the worker's
// own locked thread (see worker.loop), so the UI goroutine stays free.
func runTUI(dt scan.DataType, pid int, exec string, args []string, watch, freeze time.Duration, align int) error {
	ctrl := newController(dt, freeze, align)

	var startup tea.Cmd
	start := screenScanner
	switch {
	case exec != "":
		startup = ctrl.launch(append([]string{exec}, args...))
	case pid != 0:
		startup = ctrl.attach(pid)
	default:
		// No target given: open the process picker.
		start = screenPicker
	}

	prog := tea.NewProgram(newModel(ctrl, dt, startup, start, watch), tea.WithAltScreen())
	_, err := prog.Run()
	return err
}

type app struct {
	proc    *memory.Process
	scanner *scan.Scanner
	dtype   scan.DataType
	align   int
}

func (a *app) attach(pid int) error {
	if a.proc != nil {
		a.proc.Close()
	}
	proc, err := memory.Open(pid)
	if err != nil {
		return err
	}
	a.proc = proc
	a.scanner = scan.NewScanner(proc, a.dtype)
	a.scanner.Align = a.align
	fmt.Printf("attached to pid %d (type=%s)\n", pid, a.dtype)
	return nil
}

func (a *app) launch(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no program given")
	}
	if a.proc != nil {
		a.proc.Close()
	}
	proc, err := memory.Launch(argv[0], argv[1:]...)
	if err != nil {
		return err
	}
	a.proc = proc
	a.scanner = scan.NewScanner(proc, a.dtype)
	a.scanner.Align = a.align
	fmt.Printf("launched %s as pid %d (type=%s)\n", argv[0], proc.Pid, a.dtype)
	return nil
}

func (a *app) repl() {
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(a.prompt())
		if !sc.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if quit := a.dispatch(line); quit {
			return
		}
	}
}

func (a *app) prompt() string {
	if a.scanner == nil {
		return "memhack> "
	}
	return fmt.Sprintf("memhack[%d]> ", len(a.scanner.Matches))
}

// dispatch runs one command line. It returns true if the REPL should exit.
func (a *app) dispatch(line string) bool {
	fields := strings.Fields(line)
	cmd := fields[0]
	args := fields[1:]

	switch cmd {
	case "help", "?":
		printHelp()
	case "quit", "exit", "q":
		return true
	case "pid", "attach":
		a.cmdAttach(args)
	case "run":
		if len(args) == 0 {
			fmt.Println("usage: run <program> [args...]")
		} else if err := a.launch(args); err != nil {
			fmt.Println("error:", err)
		}
	case "type":
		a.cmdType(args)
	case "align":
		a.cmdAlign(args)
	case "regions", "maps":
		a.cmdRegions()
	case "list", "ls":
		a.cmdList(args)
	case "watch", "w":
		a.cmdWatch(args)
	case "count":
		a.cmdCount()
	case "reset", "clear":
		a.cmdReset()
	case "undo", "u":
		a.cmdUndo()
	case "set":
		a.cmdSet(args)
	case "setall":
		a.cmdSetAll(args)
	default:
		a.cmdScan(line)
	}
	return false
}

func (a *app) cmdAttach(args []string) {
	if len(args) != 1 {
		fmt.Println("usage: pid <pid>")
		return
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Println("invalid pid:", args[0])
		return
	}
	if err := a.attach(pid); err != nil {
		fmt.Println("error:", err)
	}
}

func (a *app) cmdType(args []string) {
	if len(args) != 1 {
		fmt.Printf("current type: %s\n", a.dtype)
		return
	}
	dt, err := scan.ParseDataType(args[0])
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	a.dtype = dt
	if a.scanner != nil {
		a.scanner.SetType(dt)
		a.scanner.Align = a.align
		fmt.Println("type set; matches reset")
	}
	fmt.Printf("type = %s\n", dt)
}

func (a *app) cmdAlign(args []string) {
	if len(args) != 1 {
		if a.scanner != nil {
			fmt.Printf("alignment: %d byte(s)\n", a.scanner.Alignment())
		} else {
			fmt.Printf("alignment setting: %d (0 = type width)\n", a.align)
		}
		return
	}
	n, err := parseAlign(args[0])
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	a.align = n
	if a.scanner != nil {
		a.scanner.Align = n
	}
	fmt.Printf("alignment set to %d (0 = type width; applies to the next scan)\n", n)
}

// parseAlign parses an alignment argument: "type"/"auto"/"0" = align to the
// type width, "byte"/"none"/"1" = every byte, or a positive integer.
func parseAlign(s string) (int, error) {
	switch s {
	case "type", "auto", "0":
		return 0, nil
	case "byte", "none", "1":
		return 1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid alignment %q (use a number, or 'type')", s)
	}
	return n, nil
}

func (a *app) cmdRegions() {
	if a.proc == nil {
		fmt.Println("not attached; use 'pid <pid>'")
		return
	}
	regions, err := memory.ReadMaps(a.proc.Pid)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	var total uint64
	n := 0
	for _, r := range regions {
		if !r.Scannable() {
			continue
		}
		fmt.Println(r)
		total += r.Size()
		n++
	}
	fmt.Printf("%d scannable regions, %s total\n", n, humanBytes(total))
}

func (a *app) cmdCount() {
	if a.scanner == nil {
		fmt.Println("not attached")
		return
	}
	fmt.Printf("%d matches\n", len(a.scanner.Matches))
}

func (a *app) cmdReset() {
	if a.scanner == nil {
		fmt.Println("not attached")
		return
	}
	a.scanner.Reset()
	fmt.Println("matches reset")
}

func (a *app) cmdUndo() {
	if a.scanner == nil {
		fmt.Println("not attached")
		return
	}
	if !a.scanner.Undo() {
		fmt.Println("nothing to undo")
		return
	}
	if a.scanner.Scanned() {
		fmt.Printf("undone; back to %d matches\n", len(a.scanner.Matches))
	} else {
		fmt.Println("undone; back to the pre-scan state")
	}
}

const listCap = 50

func (a *app) cmdList(args []string) {
	if a.scanner == nil {
		fmt.Println("not attached")
		return
	}
	limit := listCap
	if len(args) == 1 {
		if v, err := strconv.Atoi(args[0]); err == nil {
			limit = v
		}
	}
	a.scanner.Refresh()
	matches := a.scanner.Matches
	for i, m := range matches {
		if i >= limit {
			fmt.Printf("... %d more (raise the limit with 'list <n>')\n", len(matches)-limit)
			break
		}
		fmt.Println(a.formatMatch(i, m))
	}
	if len(matches) == 0 {
		fmt.Println("no matches")
	}
}

// formatMatch renders one match line: its index, address, and current value.
func (a *app) formatMatch(i int, m scan.Match) string {
	return fmt.Sprintf("[%d] %#012x = %s", i, m.Addr, a.dtype.Format(m.Last))
}

// cmdWatch continuously re-reads and displays the current matched values until
// the user presses Ctrl-C. On a terminal it redraws in place; otherwise it
// prints a fresh block each interval so piped output stays readable.
func (a *app) cmdWatch(args []string) {
	if a.scanner == nil {
		fmt.Println("not attached")
		return
	}
	if len(a.scanner.Matches) == 0 {
		fmt.Println("no matches to watch")
		return
	}

	interval := 500 * time.Millisecond
	if len(args) >= 1 {
		d, err := parseInterval(args[0])
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		interval = d
	}
	const minInterval = 50 * time.Millisecond
	if interval < minInterval {
		interval = minInterval
	}

	n := len(a.scanner.Matches)
	if n > listCap {
		n = listCap
		fmt.Printf("(watching the first %d of %d matches)\n", n, len(a.scanner.Matches))
	}
	tty := isTerminal(os.Stdout)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	fmt.Printf("watching every %s — press Ctrl-C to stop\n", interval)
	a.drawWatch(n, tty)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-sig:
			fmt.Println("stopped.")
			return
		case <-ticker.C:
			if !a.proc.Alive() {
				fmt.Println("target exited.")
				return
			}
			if tty {
				// Move back up over the previous block and overwrite it.
				fmt.Printf("\033[%dA", n)
			} else {
				fmt.Println("---")
			}
			a.drawWatch(n, tty)
		}
	}
}

// drawWatch refreshes the current values and prints the first n matches.
func (a *app) drawWatch(n int, tty bool) {
	a.scanner.Refresh()
	matches := a.scanner.Matches
	for i := 0; i < n && i < len(matches); i++ {
		line := a.formatMatch(i, matches[i])
		if tty {
			// Clear to end of line so a shorter new value leaves no residue.
			fmt.Print("\033[K")
		}
		fmt.Println(line)
	}
}

// parseInterval accepts either a Go duration ("250ms", "1s") or a bare number
// of milliseconds ("250").
func parseInterval(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if ms, err := strconv.Atoi(s); err == nil {
		return time.Duration(ms) * time.Millisecond, nil
	}
	return 0, fmt.Errorf("invalid interval %q (try 250ms, 1s, or a number of ms)", s)
}

// isTerminal reports whether f is a character device (an interactive terminal).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (a *app) cmdSet(args []string) {
	if a.scanner == nil {
		fmt.Println("not attached")
		return
	}
	if len(args) != 2 {
		fmt.Println("usage: set <index> <value>")
		return
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil || idx < 0 || idx >= len(a.scanner.Matches) {
		fmt.Println("invalid match index:", args[0])
		return
	}
	raw, err := a.dtype.Encode(args[1])
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	m := a.scanner.Matches[idx]
	if err := a.scanner.Write(m, raw); err != nil {
		fmt.Println("write failed:", err)
		return
	}
	fmt.Printf("wrote %s to %#012x\n", args[1], m.Addr)
}

func (a *app) cmdSetAll(args []string) {
	if a.scanner == nil {
		fmt.Println("not attached")
		return
	}
	if len(args) != 1 {
		fmt.Println("usage: setall <value>")
		return
	}
	raw, err := a.dtype.Encode(args[0])
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	ok, fail := 0, 0
	for _, m := range a.scanner.Matches {
		if err := a.scanner.Write(m, raw); err != nil {
			fail++
		} else {
			ok++
		}
	}
	fmt.Printf("wrote %s to %d matches (%d failed)\n", args[0], ok, fail)
}

// cmdScan parses a scan expression and runs it. See parseScan for the grammar.
func (a *app) cmdScan(line string) {
	if a.scanner == nil {
		fmt.Println("not attached; use 'pid <pid>' first")
		return
	}

	cond, err := parseScan(line, a.dtype)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	firstScan := !a.scanner.Scanned()
	before := len(a.scanner.Matches)
	if err := a.scanner.Scan(cond); err != nil {
		fmt.Println("error:", err)
		return
	}
	after := len(a.scanner.Matches)
	if firstScan {
		fmt.Printf("%d matches\n", after)
	} else {
		fmt.Printf("%d matches (was %d)\n", after, before)
	}
}

// parseScan converts a scan expression into a scan.Cond. Supported forms:
//
//	42            equal to 42            -5            equal to -5 (negative literal)
//	> < >= <= !=  compare to a value     42..99        value in the range [42, 99]
//	changed       differs from last      unchanged     same as last
//	inc / +       increased              dec / -       decreased
//	inc 5 / + 5   increased by exactly 5 dec 5 / - 5   decreased by exactly 5
//
// The relative-by-amount forms use a separated operator ("- 5", "dec 5");
// a glued "-5" is instead read as the negative literal −5.
//
// For Bytes/String types the whole line is a literal pattern instead (with
// "changed"/"unchanged" still recognised as relative scans).
func parseScan(line string, dt scan.DataType) (scan.Cond, error) {
	line = strings.TrimSpace(line)

	if dt.IsBytes() {
		return parseBytesScan(line, dt)
	}

	// Range: "lo..hi" (whitespace around ".." is allowed).
	if i := strings.Index(line, ".."); i >= 0 {
		lo, err := decodeLiteral(dt, strings.TrimSpace(line[:i]))
		if err != nil {
			return scan.Cond{}, fmt.Errorf("range lower bound: %w", err)
		}
		hi, err := decodeLiteral(dt, strings.TrimSpace(line[i+2:]))
		if err != nil {
			return scan.Cond{}, fmt.Errorf("range upper bound: %w", err)
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		return scan.Cond{Op: scan.InRange, Value: lo, Value2: hi}, nil
	}

	// Relative ops. A single token is a direction; the two-token inc/dec
	// forms ("inc 5", "+ 5", ...) mean a change by an exact amount.
	fields := strings.Fields(line)
	if n := len(fields); n == 1 || n == 2 {
		switch fields[0] {
		case "changed", "!=?":
			if n == 1 {
				return scan.Cond{Op: scan.Changed}, nil
			}
		case "unchanged", "==?":
			if n == 1 {
				return scan.Cond{Op: scan.Unchanged}, nil
			}
		case "inc", "increased", "+":
			if n == 1 {
				return scan.Cond{Op: scan.Increased}, nil
			}
			amt, err := decodeLiteral(dt, fields[1])
			if err != nil {
				return scan.Cond{}, err
			}
			return scan.Cond{Op: scan.IncreasedBy, Delta: amt}, nil
		case "dec", "decreased", "-":
			if n == 1 {
				return scan.Cond{Op: scan.Decreased}, nil
			}
			amt, err := decodeLiteral(dt, fields[1])
			if err != nil {
				return scan.Cond{}, err
			}
			return scan.Cond{Op: scan.DecreasedBy, Delta: amt}, nil
		}
	}

	// Comparison operators, else a bare literal (Equal).
	op := scan.Equal
	rest := line
	for _, p := range []struct {
		prefix string
		op     scan.Op
	}{
		{">=", scan.GreaterEqual},
		{"<=", scan.LessEqual},
		{"!=", scan.NotEqual},
		{">", scan.Greater},
		{"<", scan.Less},
		{"=", scan.Equal},
	} {
		if strings.HasPrefix(rest, p.prefix) {
			op = p.op
			rest = strings.TrimSpace(rest[len(p.prefix):])
			break
		}
	}

	v, err := decodeLiteral(dt, rest)
	if err != nil {
		return scan.Cond{}, err
	}
	return scan.Cond{Op: op, Value: v}, nil
}

// parseBytesScan parses a scan line for Bytes/String types: the line is a
// literal pattern, except for the "changed"/"unchanged" relative keywords.
func parseBytesScan(line string, dt scan.DataType) (scan.Cond, error) {
	switch line {
	case "changed":
		return scan.Cond{Op: scan.Changed}, nil
	case "unchanged":
		return scan.Cond{Op: scan.Unchanged}, nil
	}
	pat, err := dt.Encode(line)
	if err != nil {
		return scan.Cond{}, err
	}
	if len(pat) == 0 {
		return scan.Cond{}, fmt.Errorf("empty pattern")
	}
	return scan.Cond{Op: scan.Equal, Bytes: pat}, nil
}

// decodeLiteral validates a textual value against the current type and returns
// the float representation the scanner compares with.
func decodeLiteral(dt scan.DataType, s string) (float64, error) {
	raw, err := dt.Encode(s)
	if err != nil {
		return 0, err
	}
	v, _ := dt.Decode(raw)
	return v, nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func printHelp() {
	fmt.Print(`commands:
  pid <pid>          attach to a running process
  run <prog> [args]  launch a program as a child and attach (works under a
                     restrictive ptrace_scope, unlike attaching by pid)
  type [t]           show or set data type (i8..u64, f32/f64, bytes, string)
  align [n]          show/set scan alignment (0=type width, 1=every byte)
  regions            list scannable memory regions of the target

  <value>            scan: keep locations equal to <value> (e.g. 1337, -5)
  > < >= <= != <v>   scan: keep locations comparing that way to <v>
  <lo>..<hi>         scan: keep locations with value in the range [lo, hi]
  changed|unchanged  scan: keep locations that did/didn't change since last scan
  inc|dec            scan: keep locations that increased/decreased (+/-)
  inc <n>|dec <n>    scan: keep locations that changed by exactly <n> (+ n / - n)

  list [n]           list current matches with their current values (first 50)
  watch [interval]   live-update the matched values until Ctrl-C (e.g. watch 250ms)
  count              number of current matches
  set <i> <value>    write <value> to match #i
  setall <value>     write <value> to every match
  undo               revert the last scan, restoring the previous matches
  reset              discard all matches, start over

  help               this help
  quit               exit
`)
}
