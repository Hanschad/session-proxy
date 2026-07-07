package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/hanschad/session-proxy/internal/config"
	"github.com/hanschad/session-proxy/internal/upstream"
)

func main() {
	var (
		configPath     = flag.String("config", "", "Path to session-proxy config file")
		upstreamName   = flag.String("upstream", "", "Upstream name to validate")
		waitBeforeDrop = flag.Duration("drop-after", 10*time.Second, "Force-close transport after this duration")
		observeFor     = flag.Duration("observe-for", 20*time.Second, "Observe stream for this long after drop")
	)
	flag.Parse()

	if *configPath == "" || *upstreamName == "" {
		fmt.Fprintln(os.Stderr, "usage: resume-experiment --config config.yaml --upstream <name>")
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	pool := upstream.NewPool(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := pool.Connect(ctx); err != nil {
		log.Fatalf("connect upstreams: %v", err)
	}
	defer pool.Close()

	sshClient, adapter, err := pool.ExperimentSSHSession(*upstreamName)
	if err != nil {
		log.Fatalf("experiment session: %v", err)
	}

	session, err := sshClient.NewSession()
	if err != nil {
		log.Fatalf("new ssh session: %v", err)
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		log.Fatalf("stdout pipe: %v", err)
	}
	if err := session.Start(`sh -c 'i=0; while [ $i -lt 120 ]; do i=$((i+1)); echo tick-$i; sleep 1; done'`); err != nil {
		log.Fatalf("start command: %v", err)
	}

	ticks := make(chan string, 64)
	errCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			ticks <- scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			errCh <- err
			return
		}
		errCh <- io.EOF
	}()

	start := time.Now()
	var (
		lastTick      string
		ticksAfterDrop int
		dropDone      bool
	)

	fmt.Printf("Resume experiment started upstream=%q drop_after=%s observe_for=%s\n",
		*upstreamName, *waitBeforeDrop, *observeFor)

	for {
		select {
		case tick := <-ticks:
			lastTick = tick
			if dropDone {
				ticksAfterDrop++
			}
			fmt.Printf("[%s] %s\n", time.Since(start).Truncate(time.Millisecond), tick)
		case err := <-errCh:
			fmt.Printf("RESULT: FAIL (stream ended: %v last_tick=%q ticks_after_drop=%d)\n", err, lastTick, ticksAfterDrop)
			os.Exit(1)
		case <-time.After(200 * time.Millisecond):
			if !dropDone && time.Since(start) >= *waitBeforeDrop {
				fmt.Printf("[%s] forcing WebSocket transport close...\n", time.Since(start).Truncate(time.Millisecond))
				adapter.ForceCloseTransport()
				dropDone = true
				dropDoneAt := time.Now()
				go func() {
					time.Sleep(*observeFor)
					if ticksAfterDrop > 0 {
						fmt.Printf("RESULT: PASS (observed %d ticks after transport drop; last_tick=%q)\n", ticksAfterDrop, lastTick)
						os.Exit(0)
					}
					fmt.Printf("RESULT: FAIL (no ticks within %s after transport drop; last_tick=%q reconnecting=%v close_reason=%v)\n",
						*observeFor, lastTick, adapter.Reconnecting(), adapter.CloseReason())
					os.Exit(1)
				}()
				_ = dropDoneAt
			}
		}

		if dropDone && time.Since(start) > *waitBeforeDrop+*observeFor+2*time.Second {
			break
		}
	}

	if ticksAfterDrop > 0 && strings.HasPrefix(lastTick, "tick-") {
		fmt.Printf("RESULT: PASS (last_tick=%q ticks_after_drop=%d)\n", lastTick, ticksAfterDrop)
		return
	}
	fmt.Printf("RESULT: FAIL (last_tick=%q ticks_after_drop=%d)\n", lastTick, ticksAfterDrop)
	os.Exit(1)
}
