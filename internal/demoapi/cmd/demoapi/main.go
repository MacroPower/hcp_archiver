// Command demoapi serves the fictional HCP Terraform organization the
// documentation recordings archive.
//
// It is a thin wrapper over
// [go.jacobcolvin.com/hcp_archiver/internal/demoapi]: it binds the listener
// (which is what fixes the artifact URLs the served documents advertise),
// prints the base address for the archiver to point at, and shuts down
// gracefully on a signal. The Taskfile starts it around a recording and stops
// it through the pid file it writes.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"go.jacobcolvin.com/hcp_archiver/internal/demoapi"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "address to listen on")
	seed := flag.String("seed", demoapi.DefaultSeed, "seed every generated identifier derives from")
	runs := flag.Int("runs", demoapi.DefaultRuns, "runs in each workspace's history")
	states := flag.Int("states", demoapi.DefaultStates, "state versions in each workspace's history")
	chaos := flag.Bool("chaos", true, "inject latency, one rate-limited response, and two retryable failures")
	pidfile := flag.String("pidfile", "", "file to write the server's process id into")

	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := run(ctx, *addr, *pidfile, demoapi.New(
		demoapi.WithSeed(*seed),
		demoapi.WithRuns(*runs),
		demoapi.WithStates(*states),
		demoapi.WithChaos(*chaos),
	))

	stop()

	if err != nil {
		log.Fatalf("demoapi: %v", err)
	}
}

// run binds the listener, announces the address, and serves until ctx is done.
//
// The bind happens before anything else so an already-bound port fails loudly
// here rather than leaving a stale server quietly in charge of the recording.
func run(ctx context.Context, addr, pidfile string, srv *demoapi.Server) error {
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	if pidfile != "" {
		err = os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
		if err != nil {
			return fmt.Errorf("write pid file %s: %w", pidfile, err)
		}

		defer os.Remove(pidfile)
	}

	_, err = fmt.Fprintf(os.Stdout, "http://%s\n", ln.Addr().String())
	if err != nil {
		return fmt.Errorf("announce address: %w", err)
	}

	return srv.Serve(ctx, ln)
}
