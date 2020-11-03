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

// Package list defines the list subcommand to display the devices available
// that qualify for provisioning with an installer.
package list

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"flag"
	"github.com/google/fresnel/cli/console"
	"github.com/google/logger"
	"github.com/google/subcommands"
	"github.com/google/winops/storage"
)

var (
	// The name of this binary, set in init.
	binaryName = ""
	// Dependency injections for testing.
	search = storage.Search
)

func init() {
	binaryName = filepath.Base(strings.ReplaceAll(os.Args[0], `.exe`, ``))
	subcommands.Register(&listCmd{}, "")
}

// listCmd represents the list subcommand.
type listCmd struct {
	// listFixed determines whether we want to consider fixed drives when listing
	// available devices. It is defaulted to false by flag.
	listFixed bool

	// minSize is the minimum size device to search for in GB. For convenience,
	// this value is defaulted to to a reasonable minimum size by flag.
	minSize int

	// maxSize is the largest size device to search for in GB. For convenience,
	// this value is set to 'no limit (0)' by default by flag.
	maxSize int

	// json silences any unnecessary text output and returns the device list in JSON.
	// This value is defaulted to false by flag.
	json bool
}

var oneGB = 1073741824

// Ensure listCommand implements the subcommands.Command interface.
var _ subcommands.Command = (*listCmd)(nil)

// Name returns the name of the subcommand.
func (*listCmd) Name() string {
	return "list"
}

// Synopsis returns a short string (less than one line) describing the subcommand.
func (*listCmd) Synopsis() string {
	return "list available devices suitable for provisioning with an installer"
}

// Usage returns a long string explaining the subcommand and its usage.
func (*listCmd) Usage() string {
	return fmt.Sprintf(`list [flags...]

List available devices suitable for provisioning with an installer.

Flags:
  --show_fixed    - Includes fixed disks when searching for suitable devices.
  --minimum [int] - The minimum size in GB to consider when searching.
  --maximum [int] - The maximum size in GB to consider when searching.

Example #1: Perform a standard search with defaults (removable media only > 2GB)
  '%s list'

Example #2: Limit search to larger devices.
  '%s list --minimum=8'

Example #3: Search fixed devices and removable devices.
  '%s list --show_fixed'

Example output:

DEVICE |  MODEL  | SIZE  | INSTALLER PRESENT
-------+---------+-------+--------------------
 disk1 | Unknown | 16 GB | Not Present
 disk3 | Cruzer  | 64 GB | Present

Defaults:
`, binaryName, binaryName, binaryName)
}

// SetFlags adds the flags for this command to the specified set.
func (c *listCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&c.listFixed, "show_fixed", false, "Also display fixed drives.")
	f.IntVar(&c.minSize, "minimum", 2, "The minimum size [in GB] of drives to search for.")
	f.IntVar(&c.maxSize, "maximum", 0, "The maximum size [in GB] drives to search for.")
	f.BoolVar(&c.json, "json", false, "Display the device list in JSON with no additional output")
}

// Execute runs the command and returns an ExitStatus.
func (c *listCmd) Execute(_ context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	// Scan for the available drives. Warn that this may take a while.
	if c.json {
		// Turning on verbose will silence console output
		console.Verbose = true
	}

	console.Print("Searching for devices. This take up to one minute...\n")
	logger.V(1).Info("Searching for devices.")
	devices, err := search("", uint64(c.minSize*oneGB), uint64(c.maxSize*oneGB), !c.listFixed)
	if err != nil {
		logger.Errorf("storage.Search(%d, %d, %t) returned %v", c.minSize, c.maxSize, !c.listFixed, err)
		return subcommands.ExitFailure
	}
	// Wrap devices in an []console.TargetDevice.
	available := []console.TargetDevice{}
	for _, d := range devices {
		available = append(available, d)
	}

	console.PrintDevices(available, os.Stdout, c.json)

	// Provide contextual help for next steps.
	console.Printf(`
Use the 'windows' subcommand to provision an installer on one, several, or all flash drives.
Example #1 (Windows): Use '%s windows 1 2' to write a windows installer image to disk 1 and 2.
Example #2 (Linux):   Use '%s windows --all' to write a windows installer image to all suitable drives.

For additional examples, see the help for the windows subcommand, '%s help windows'.`, binaryName, binaryName, binaryName)

	return subcommands.ExitSuccess
}
