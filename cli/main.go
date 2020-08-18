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
	"os"
	"os/signal"
	"syscall"

	// Register subcommands.
	_ "github.com/google/fresnel/cli/commands/list"
	_ "github.com/google/fresnel/cli/commands/write"

	// TODO: Implement and import wipe subcommand.

	"flag"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func main() {

	// Explicitly set the log output flag from the log package so that we can see
	// info messages by default in the console and in the logs. Logging is
	// initialized in each sub-command.
	flag.Set("alsologtostderr", "true")
	flag.Set("vmodule", "third_party/golang/fresnel*=1")

	subcommands.Register(subcommands.HelpCommand(), "")
	subcommands.Register(subcommands.FlagsCommand(), "")
	subcommands.Register(subcommands.CommandsCommand(), "")

	if flag.NArg() < 1 {
		logger.Error("ERROR: No command specified.")
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
		logger.Errorf("Received %s signal. Cancelling context ...\n", sig)
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
		cancelFn()
	}()

	os.Exit(int(subcommands.Execute(ctx)))
}
