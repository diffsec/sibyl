// Command sibyl-hostd runs the privileged host actions that the System API
// queues: hostname, network, and operator accounts. It runs as root from
// sibyl-hostd.path, handles every queued request, and exits.
//
// With -state it only rewrites the state file.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/ihavespoons/sibyl/internal/hostd"
)

func main() {
	stateOnly := flag.Bool("state", false, "only write the state file")
	queue := flag.String("queue", "/var/lib/sibyl/run/host", "request queue, written by the System API")
	out := flag.String("out", "/var/lib/sibyl-host/out", "state and results, read by the System API")
	data := flag.String("data", "/var/lib/sibyl-host", "records that only root reads")
	label := flag.String("label", "system_u:object_r:container_file_t:s0", "SELinux context for the output files")
	group := flag.String("group", "sibyl", "group that can read the output files")
	etc := flag.String("etc", "/etc", "directory with crypttab and fstab")
	pools := flag.String("pools", "/var/lib/sibyl-pools", "directory where model pools are mounted")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	g, err := user.LookupGroup(*group)
	if err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
	a := &hostd.Agent{Queue: *queue, Out: *out, Data: *data, Run: run, Now: time.Now, Log: log, Label: *label, Group: gid,
		Etc: *etc, PoolsDir: *pools}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if *stateOnly {
		err = a.WriteState(ctx)
	} else {
		err = a.Process(ctx)
	}
	if err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

// run runs a command with a fixed PATH and the C locale, so the output that
// sibyl-hostd parses does not change with the environment.
func run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin", "LC_ALL=C"}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		var ee *exec.ExitError
		if errors.As(err, &ee) && msg != "" {
			err = errors.New(msg)
		}
		err = fmt.Errorf("%s: %w", name, err)
	}
	return stdout.String(), err
}
