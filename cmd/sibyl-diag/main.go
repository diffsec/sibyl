// Command sibyl-diag records inference metrics, watches inference, captures
// incidents, and builds and uploads redacted diagnostic bundles. It runs as
// root on the host. The systemd units start the daemons; an administrator
// runs the other commands with sudo. The design is in
// docs/design/diagnostics.md.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/diffsec/sibyl/internal/diag"
)

const usage = `usage: sibyl-diag <command> [flags]

Commands:
  status                 watchdog state, upload consent and safe mode
  perf [-since 24h] [-model ID]
                         requests and tokens per second per model
  hardware               hardware and software inventory
  incidents              recorded incidents
  capture [-note TEXT]   record an incident now
  bundle [-incident NAME | -latest] [-note TEXT] [-list]
                         make a redacted bundle with a diagnostic ID
  submit [-yes] [ID]     upload a bundle; needs upload consent
  consent [off|manual|automatic] [-endpoint URL]
                         show or set upload consent
  safe-mode [on|off]     show, set or clear safe mode

Started by systemd: metrics, watchdog, boot-check, clean-shutdown, upload-pending
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	d := &diag.Diag{Root: "/", Run: diag.Exec, Now: time.Now, Log: log, Group: -1}
	if err := d.LoadConfig(); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, d, os.Args[1], os.Args[2:]); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, d *diag.Diag, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "metrics":
		if err := outGroup(d); err != nil {
			return err
		}
		return d.RunMetrics(ctx)
	case "watchdog":
		return d.RunWatchdog(ctx)
	case "boot-check":
		// Finish before the unit's TimeoutStartSec, so that systemd runs
		// ExecStop at the next clean shutdown.
		bctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		res, err := d.BootCheck(bctx)
		fmt.Println(res)
		return err
	case "clean-shutdown":
		return d.CleanShutdown()
	case "upload-pending":
		sent, err := d.UploadPending(ctx)
		for _, id := range sent {
			fmt.Println("sent", id)
		}
		return err
	case "status":
		return printJSON(d.Status())
	case "hardware":
		return printJSON(d.Inventory(ctx))
	case "perf":
		since := fs.Duration("since", 24*time.Hour, "time window")
		model := fs.String("model", "", "one model ID")
		asJSON := fs.Bool("json", false, "print JSON")
		_ = fs.Parse(args)
		reqs, err := d.Requests(time.Now().Add(-*since), *model)
		if err != nil {
			return err
		}
		perf := diag.Perf(reqs)
		if *asJSON {
			return printJSON(perf)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "MODEL\tREQUESTS\tERRORS\tGEN TOK/S P50\tGEN TOK/S P5\tPROMPT TOK/S P50")
		for _, p := range perf {
			_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\n", p.Model, p.Requests, p.Errors,
				diag.FormatTPS(p.GenTPSP50), diag.FormatTPS(p.GenTPSP5), diag.FormatTPS(p.PromptTPSP50))
		}
		return tw.Flush()
	case "incidents":
		for _, name := range d.Incidents() {
			fmt.Println(name)
		}
		return nil
	case "capture":
		note := fs.String("note", "", "what you saw")
		_ = fs.Parse(args)
		dir, err := d.Capture(ctx, "manual", noteDetails(*note), false)
		fmt.Println(dir)
		return err
	case "bundle":
		return bundle(ctx, d, fs, args)
	case "submit":
		return submit(ctx, d, fs, args)
	case "consent":
		endpoint := fs.String("endpoint", "", "ingest service URL (https)")
		mode, err := modeAndFlags(fs, args)
		if err != nil {
			return err
		}
		if mode == "" {
			c := d.LoadConsent()
			fmt.Printf("mode=%s endpoint=%s\n", c.Mode, orNone(d.Endpoint()))
			return nil
		}
		c, err := d.SaveConsent(mode, *endpoint)
		if err != nil {
			return err
		}
		fmt.Printf("Upload consent is now %s.\n", c.Mode)
		return nil
	case "safe-mode":
		_ = fs.Parse(args)
		switch fs.Arg(0) {
		case "":
			on, why := d.SafeMode()
			fmt.Println(map[bool]string{true: "on: " + why, false: "off"}[on])
		case "on":
			if err := d.SetSafeMode(true, "set by an administrator"); err != nil {
				return err
			}
			fmt.Println("Safe mode is on. llama-swap does not start at the next boot, and the watchdog does not act.")
		case "off":
			if err := d.SetSafeMode(false, ""); err != nil {
				return err
			}
			fmt.Println("Safe mode is off. Start inference with: sudo systemctl --user -M sibyl@ start llama-swap.service")
		default:
			return fmt.Errorf("safe-mode takes on or off, not %q", fs.Arg(0))
		}
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func bundle(ctx context.Context, d *diag.Diag, fs *flag.FlagSet, args []string) error {
	incident := fs.String("incident", "", "incident name or prefix")
	latest := fs.Bool("latest", false, "use the newest incident")
	note := fs.String("note", "", "what you saw, in your own words")
	list := fs.Bool("list", false, "list the files in the bundle")
	_ = fs.Parse(args)
	names := d.Incidents()
	var dir string
	switch {
	case *latest:
		if len(names) == 0 {
			return fmt.Errorf("no incidents are recorded")
		}
		dir = filepath.Join(d.IncidentDir(), names[len(names)-1])
	case *incident != "":
		for _, n := range names {
			if strings.HasPrefix(n, *incident) {
				dir = filepath.Join(d.IncidentDir(), n)
			}
		}
		if dir == "" {
			return fmt.Errorf("no incident matches %q", *incident)
		}
	default:
		// No incident named: take a fresh snapshot, so the bundle has logs.
		var err error
		if dir, err = d.Capture(ctx, "manual", noteDetails(*note), false); err != nil {
			return err
		}
	}
	b, err := d.BuildBundle(dir, *note)
	if err != nil {
		return err
	}
	fmt.Printf("Diagnostic ID: %s\nBundle: %s (redacted)\n", b.ID, b.Path)
	if *list {
		for _, f := range b.Files {
			fmt.Printf("  %10d  %s\n", f.Bytes, f.Path)
		}
	}
	return nil
}

func submit(ctx context.Context, d *diag.Diag, fs *flag.FlagSet, args []string) error {
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	_ = fs.Parse(args)
	path := d.LatestBundle()
	if id := fs.Arg(0); id != "" {
		if !diag.ValidID(id) {
			return fmt.Errorf("%q is not a diagnostic ID", id)
		}
		path = filepath.Join(d.BundleDir(), strings.ToUpper(id)+".tar.gz")
	}
	info, err := os.Stat(path)
	if path == "" || err != nil {
		return fmt.Errorf("no bundle to send; make one with `sibyl-diag bundle`")
	}
	if !*yes {
		fmt.Printf("This sends %s (%d bytes, redacted) to %s.\nList its files with: tar tzf %s\nSend it? [y/N] ",
			filepath.Base(path), info.Size(), orNone(d.Endpoint()), path)
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(answer)) != "y" {
			return nil
		}
	}
	id, err := d.Upload(ctx, path)
	if err != nil {
		return err
	}
	fmt.Println("Sent. Diagnostic ID:", id)
	return nil
}

// outGroup makes status.json and perf.json readable by the System API.
func outGroup(d *diag.Diag) error {
	g, err := user.LookupGroup("sibyl")
	if err != nil {
		return nil // not on a Sibyl host, as in development
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	d.Group, d.Label = gid, "system_u:object_r:container_file_t:s0"
	return nil
}

// modeAndFlags parses "MODE -flag value" as well as "-flag value MODE". The
// flag package stops at the first argument that is not a flag, so without a
// second parse, "consent manual -endpoint URL" drops the endpoint.
func modeAndFlags(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	mode := fs.Arg(0)
	if fs.NArg() > 1 {
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return "", err
		}
		if fs.NArg() > 0 {
			return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
	}
	return mode, nil
}

func noteDetails(note string) map[string]any {
	if note == "" {
		return nil
	}
	return map[string]any{"note": note}
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
