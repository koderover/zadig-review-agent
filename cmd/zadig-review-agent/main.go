package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zadig-review-agent: %v\n", err)
		if errors.Is(err, context.Canceled) {
			os.Exit(agent.ExitCanceled)
		}
		if code == 0 {
			code = agent.ExitIncomplete
		}
	}
	os.Exit(code)
}
