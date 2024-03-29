// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// main is the entry point for the image writer. It implements image writing
// functionality for installers to compatible devices through subcommands.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	// Register subcommands.
	_ "github.com/google/fresnel/cli/commands/list"
	_ "github.com/google/fresnel/cli/commands/write"
	"github.com/google/deck/backends/logger"
	"github.com/google/deck"

	"flag"
	"github.com/google/subcommands"
)

var (
	binaryName = filepath.Base(strings.ReplaceAll(os.Args[0], `.exe`, ``))
	logFile    *os.File
)

func setupLogging() error {
	// Initialize logging with the bare binary name as the source.
	lp := filepath.Join(os.TempDir(), fmt.Sprintf(`%s.log`, binaryName))
	var err error
	logFile, err = os.OpenFile(lp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0660)
	if err != nil {
		return fmt.Errorf("Failed to open log file: %v", err)
	}
	deck.Add(logger.Init(logFile, 0))

	return nil
}

func main() {

	// Explicitly set the log output flag from the log package so that we can see
	// info messages by default in the console and in the logs. Logging is
	// initialized in each sub-command.
	flag.Set("alsologtostderr", "true")
	flag.Set("vmodule", "third_party/golang/fresnel*=1")

	if err := setupLogging(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer logFile.Close()
	defer deck.Close()

	subcommands.Register(subcommands.HelpCommand(), "")
	subcommands.Register(subcommands.FlagsCommand(), "")
	subcommands.Register(subcommands.CommandsCommand(), "")

	if flag.NArg() < 1 {
		deck.Error("ERROR: No command specified.")
	}

	// Cancel the context on sigterm and sigint.
	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		// Only handle the first sigterm or sigint and then cancel the context
		// and ignore these signals.
		sig := <-signalCh
		deck.Errorf("Received %s signal. Cancelling context ...\n", sig)
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
		cancelFn()
	}()

	os.Exit(int(subcommands.Execute(ctx)))
}
