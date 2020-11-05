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

// Package write implements the write subcommand for provisioning an installer.
package write

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"flag"
	"github.com/google/fresnel/cli/config"
	"github.com/google/fresnel/cli/console"
	"github.com/google/fresnel/cli/installer"
	"github.com/google/logger"
	"github.com/google/subcommands"
	"github.com/google/winops/storage"
)

const (
	oneGB   int = 1073741824 // Represents one GB of data.
	minSize int = 2          // The default minimum size for available storage.
)

var (
	binaryName string

	// Wrapped errors for testing.
	errConfig    = errors.New(`config error`)
	errDevice    = errors.New(`device error`)
	errInstaller = errors.New(`installer error`)
	errElevation = errors.New(`elevation error`)
	errFinalize  = errors.New(`finalize error`)
	errPrepare   = errors.New(`prepare error`)
	errProvision = errors.New(`provision error`)
	errRetrieve  = errors.New(`retrieve error`)
	errSearch    = errors.New(`search error`)

	// Dependency Injections for testing
	execute      = run
	search       = storageSearch
	newInstaller = installerNew
)

func init() {
	binaryName = filepath.Base(strings.ReplaceAll(os.Args[0], `.exe`, ``))

	// write registers several aliases to allow simper interactions at the
	// command line. An alternate name and a set of default values is provided
	// for each alias. For example, write can be registered under the name
	// 'linux' or 'windows' with a default value in the distro field to eliminate
	// the need to provide a value for any flags at the command line. A default
	// subcommand of write with no defaults is always provided.
	//
	// e.g. 'image-writer windows sdc' instead of 'image-writer write -distro=windows sdc'.
	subcommands.Register(&writeCmd{name: "write"}, "")
	subcommands.Register(&writeCmd{name: "windows", distro: "windows", track: "stable"}, "")
	subcommands.Register(&writeCmd{name: "windows-testing", distro: "windows", track: "testing"}, "")
	subcommands.Register(&writeCmd{name: "windows-unstable", distro: "windows", track: "unstable"}, "")
	subcommands.Register(&writeCmd{name: "update", distro: "windows", track: "stable", update: true}, "")
}

// writeCmd is the write subcommand to download and write the installer image
// to avaialble storage devices.
type writeCmd struct {
	// name is the name of the write command.
	name string

	// allDevices determines whether the image is written to all suitable
	// devices. If true, specified devices are not used.
	allDrives bool

	// cleanup determines whether temporary files generated during provisioning
	// are cleaned up after provisioning. Defaults to true.
	cleanup bool

	// dismount determines whether devices are dismounted after provisioning
	// to limit accidental writes afterwords. The default value is specified
	// when initializing the subcommand.
	dismount bool

	// distro specifies the OS distribution to be provisioned onto selected
	// devices. The available values are determined by the config package.
	// The write subcommand can be initialized with a default value present
	// in distro to eliminate the need to pass the distro flag when running
	// the command. A distro specifies what track values are available.
	distro string

	// track specifies the distribution track or variant of the distribution
	// to be provisioned.
	// Examples: 'stable', 'testing', 'unstable', 'test'.
	track string

	// seedServer permits overriding the default server used to obtain a seed
	// for distributions that require them. If the chosen distribution does not
	// require a seed, this setting is ignored. The default value is specified
	// in the configuration for the distribution.
	seedServer string

	// warning provides a confirmation prompt before devices are overwritten. It
	// defaults to true. Warnings are automatically skipped when all devices
	// already have an installer, as no data loss is possible.
	warning bool

	// eject powers off and ejects a device after writing the image. The default
	// value is specified when the subcommand is initialized.
	eject bool

	// update signals write to perform an erase and refresh instead of a full
	// wipe and write of contents. Update automatically detects the distribution
	// type. It is typically used by non-admin users.
	update bool

	// info causes console messages to be displayed with debugging information
	// included.
	info bool

	// v controls the level of log verbosity. It defaults to 1, and higher levels
	// increase the info logging that is provided.
	v int

	// verbose is a convenience control that turns log verbosity up to the
	// maximum. It is most often used for simplicity when troubleshooting.
	verbose bool

	// listFixed determines whether we want to consider fixed drives when
	// determining available devices. It is defaulted to false by flag.
	// If listFixed is specified, the all flag is disallowed.
	listFixed bool

	// minSize is the minimum size device to search for in GB. For convenience,
	// this value is defaulted to minSize.
	minSize int

	// maxSize is the largest size device to search for in GB. For convenience,
	// this value is set to 'no limit (0)' by default by flag.
	maxSize int
}

// Ensure writeCommand implements the subcommands.Command interface.
var _ subcommands.Command = (*writeCmd)(nil)

// Name returns the name of the subcommand.
func (c *writeCmd) Name() string {
	return c.name
}

// Synopsis returns a short string (less than one line) describing the subcommand.
func (c *writeCmd) Synopsis() string {
	d := c.distro
	if d == "" {
		d = "specific (see -distro flag)"
	}
	return fmt.Sprintf("Provision a %s %s installer to devices", d, c.track)
}

// Usage returns a long string explaining the subcommand and giving usage information.
func (c *writeCmd) Usage() string {
	return fmt.Sprintf(`%s [flags...] [device(s)...]

Download and provision an image (installer) to one or more storage devices.
This operation requires elevated permissions such as 'sudo' on Linux/Mac or
'run as administrator' on Windows.

Flags:
  --all      - Provision all suitable devices that are attached to this system.
  --a        - Alias for --all
  --cleanup  - Cleanup temporary files after provisioning completes.
  --dismount - Dismount devices after provisioning completes.
  --eject    - Eject/PowerOff devices after provisioniong completes.
  --warning  - Display a confirmation prompt before non-installers are overwritten.
  --distro   - The os distribution to be provisioned, typically 'windows' or 'linux'
  --track    - The track (variant) of the installer to provision.
	--update   - Attempts to perform a device refresh only (for non-admin users).
  --info     - Display console messages with debugging information included.
  --verbose   - Increase info log verbosity to maximum, used as an alias for '--v 5'.
  --v        - Controls the level of info log verbosity.

  --show_fixed    - Includes fixed disks when searching for suitable devices.
  --minimum [int] - The minimum size in GB to consider when searching.
  --maximum [int] - The maximum size in GB to consider when searching.

Use the 'list' command to list available devices or use the '--all' flag to
write to all suitable devices.

Example #1 (Linux): 'provision a windows installer on storage devices sdy and sdz'
  - '%s windows sdy sdz'

Example #2 (Windows): 'provision a windows installer on storage device 1'
  - '%s windows 1'

Example #3 (Windows) 'provision a windows installer on all storage devices'
  - '%s windows -all'

Example #4 (Mac) 'provision a windows test image on storage device disk1'
  - '%s write -distro=windows -track=test disk1'

Example #5 (Windows) 'update a windows installer on storage device disk1'
  - '%s update 1'

Defaults:
`, c.name, binaryName, binaryName, binaryName, binaryName, binaryName)
}

// SetFlags adds the flags for this command to the specified set.
func (c *writeCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&c.allDrives, "all", false, "write the installer to all suitable storage devices")
	f.BoolVar(&c.allDrives, "a", false, "write the installer to all suitable flash drives (shorthand)")
	f.BoolVar(&c.cleanup, "cleanup", true, "cleanup temporary files after provisioning is complete")
	f.BoolVar(&c.eject, "eject", c.eject, "eject/power-off devices after provisioning is complete")
	f.BoolVar(&c.warning, "confirm", true, "display a confirmation prompt before non-installer storage devices are overwritten")
	f.BoolVar(&c.update, "update", c.update, "attempts to perform a device refresh only for non-admin users")
	f.StringVar(&c.distro, "distro", c.distro, "the os distribution to be provisioned, typically 'windows' or 'linux'")
	f.StringVar(&c.track, "track", c.track, "track (variant) of the installer to provision")
	f.StringVar(&c.seedServer, "seed_server", "", "override the default server to use for obtaining seeds, only used for debugging")
	f.BoolVar(&c.info, "info", false, "display console messages with debugging information included")
	f.IntVar(&c.v, "v", 1, "controls the level of info log verbosity")
	f.BoolVar(&c.verbose, "verbose", false, "increase info log verbosity to maximum, alias for '-v 5'")
	// Search related flags.
	f.BoolVar(&c.listFixed, "show_fixed", false, "also consider fixed drives, cannot be combined with --all")
	f.IntVar(&c.minSize, "minimum", minSize, "minimum size [in GB] of drives to consider as available")
	f.IntVar(&c.maxSize, "maximum", 0, "maximum size [in GB] drives to consider as avaialble")

	// Special case flag handling.

	// The dismount flag is always defaulted to true on Linux as we don't want
	// to leave temp mount folders behind. On Windows and Darwin it is generally
	// expected that the device remain avaialble post-provioning.
	d := c.dismount
	if runtime.GOOS == "linux" {
		d = true
	}
	f.BoolVar(&c.dismount, "dismount", d, "dismount devices after provisioning is complete")
}

// imageInstaller represents installer.Installer.
type imageInstaller interface {
	Cache() string
	Finalize([]installer.Device) error
	Retrieve() error
	Prepare(installer.Device) error
	Provision(installer.Device) error
}

// Execute executes the command and returns an ExitStatus.
func (c *writeCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...interface{}) (exitStatus subcommands.ExitStatus) {
	// Enable turning verbosity up past log.V(1) for the cli with a single bool
	// flag to retain flag equivalence with similar tooling on Windows. To avoid
	// excessive verbosity, V is only increased for local libraries.
	if c.verbose {
		c.v = 5
	}
	if c.info || c.v > 1 {
		console.Verbose = true
	}

	// Initialize logging with the bare binary name as the source.
	lp := filepath.Join(os.TempDir(), fmt.Sprintf(`%s.log`, binaryName))
	lf, err := os.OpenFile(lp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0660)
	if err != nil {
		logger.Errorf("Failed to open log file: %v", err)
		return subcommands.ExitFailure
	}
	defer lf.Close()
	defer logger.Init(binaryName, console.Verbose, true, lf).Close()
	// Verbosity will need to be a flag in main
	logger.SetLevel(logger.Level(c.v))

	// Log startup for upstream consumption by dashboards.
	logger.V(1).Infof("%s is initializing.\n", binaryName)

	// Check if any devices were specified.
	if f.NArg() == 0 && !c.allDrives {
		logger.Errorf("No devices were specified.\n"+
			"Use the 'list' command to list available devices or use the '--all' flag to write to all suitable devices.\n"+
			"usage: %s %s\n", os.Args[0], c.Usage())
		return subcommands.ExitUsageError
	}

	// Setting both all and listFixed is not allowed to protect users from
	// unintentional wiping of their fixed (os) disks.
	if c.allDrives && c.listFixed {
		logger.Errorln("Only one of '--all' or '--show_fixed' is allowed.")
		return subcommands.ExitFailure
	}

	// We now know we have a valid list of devices to provision, and we can
	// begin provisioning.
	if err = execute(c, f); err != nil {
		logger.Error(err)
		logger.Errorf("%s completed with errors.", binaryName)
		return subcommands.ExitFailure
	}

	// Log completion for upstream consumption by dashboards.
	console.Printf("%s completed successfully.", binaryName)
	logger.V(1).Infof("%s completed successfully.", binaryName)
	return subcommands.ExitSuccess
}

func run(c *writeCmd, f *flag.FlagSet) (err error) {
	// Generate a writer configuration.
	conf, err := config.New(c.cleanup, c.warning, c.dismount, c.eject, c.update, f.Args(), c.distro, c.track, c.seedServer)
	if err != nil {
		return fmt.Errorf("config.New(cleanup: %t, warning: %t, dismount: %t, eject: %t, devices: %v, distro: %s, track: %s, seedServer: %s) returned %v: %w",
			c.cleanup, c.warning, c.dismount, c.eject, f.Args(), c.distro, c.track, c.seedServer, err, errConfig)
	}
	// Write requires elevated permissions, Update does not.
	if !c.update && !conf.Elevated() {
		return fmt.Errorf("elevated permissions are required to use the %q command, try again using 'sudo' (Linux/Mac) or 'run as administrator' (Windows): %w", c.name, errElevation)
	}

	// Pull a list of suitable devices.
	console.Printf("Searching for available devices... ")
	logger.V(1).Infof("Searching for available devices... ")
	available, err := search("", uint64(c.minSize*oneGB), uint64(c.maxSize*oneGB), !c.listFixed)
	if err != nil {
		return fmt.Errorf("search returned %v: %w", err, errSearch)
	}

	// If the --all flag was specified, update the target list.
	if c.allDrives {
		// Build a list of the device names.
		all := []string{}
		for _, d := range available {
			all = append(all, d.Identifier())
		}
		conf.UpdateDevices(all)
	}

	// Build a simple map of available devices for lookups.
	verified := make(map[string]installer.Device)
	for _, d := range available {
		verified[d.Identifier()] = d
	}
	// Check if the requested devices are available and build a list of targets.
	targets := []installer.Device{}
	for _, t := range conf.Devices() {
		d, ok := verified[t]
		if !ok {
			return fmt.Errorf("requested device %q is not suitable for provisioning, available devices %v: %w", t, verified, errDevice)
		}
		targets = append(targets, d)
	}

	logger.V(3).Infof("Configuration to be applied:\n%s", conf)
	// Adjust wording based on whether or not we're doing an update.
	writeType := "provisioned"
	if c.update {
		writeType = "updated"
	}
	console.Printf("The following devices will be %s with the latest %s [%s] installer:\n", writeType, conf.Distro(), conf.Track())
	logger.V(2).Infof("Devices %v will be %s with the latest %s [%s] installer.\n", writeType, conf.Devices(), conf.Distro(), conf.Track())

	// Wrap targets in the interface required for the prompt.
	devices := []console.TargetDevice{}
	for _, device := range targets {
		devices = append(devices, device)
	}
	// Display information about the device(s) and warn the user.
	console.PrintDevices(devices, os.Stdout, false)
	if conf.Warning() {
		if err := console.PromptUser(); err != nil {
			return fmt.Errorf("console.PromptUser() returned %v", err)
		}
	}

	// Initialize the installer.
	i, err := newInstaller(conf)
	if err != nil {
		return fmt.Errorf("installer.New() returned %v: %w", err, errInstaller)
	}

	// Defer dismounts, power-off, and cleanup. Finalize only performs these
	// actions if configuration states to do so. Cleanup is performed only after
	// the last device has been finalized.
	defer func(devices []installer.Device) {
		if err2 := i.Finalize(devices); err2 != nil {
			if err == nil {
				err = fmt.Errorf("Finalize() returned %v: %w", err2, errFinalize)
			} else {
				err = fmt.Errorf("%v\nFinalize() returned %v: %w", err, err2, errFinalize)
			}
		}
	}(targets)

	// Retrieve the image. This step occurs only once for n>0 devices.
	console.Printf("\nRetrieving image...\n    %s ->\n    %s", conf.Image(), i.Cache())
	logger.V(1).Infof("Retrieving image...\n    %s ->\n    %s\n\n", conf.Image(), i.Cache())
	if err := i.Retrieve(); err != nil {
		return fmt.Errorf("Retrieve() returned %v: %w", err, errRetrieve)
	}
	// Prepare and provision devices. This step occurs once per device.
	for _, device := range targets {
		console.Printf("\nPreparing device %q...", device.Identifier())
		logger.V(1).Infof("Preparing device %q...", device.Identifier())
		// Prepare the device.
		if err := i.Prepare(device); err != nil {
			return fmt.Errorf("Prepare(%q) returned %v: %w", device.FriendlyName(), err, errPrepare)
		}
		console.Printf("Provisioning device %q...", device.Identifier())
		logger.V(1).Infof("Provisioning device %q...", device.Identifier())
		// Provision the device.
		if err := i.Provision(device); err != nil {
			return fmt.Errorf("Provision(%q) returned %v: %w", device.FriendlyName(), err, errProvision)
		}
	}
	return nil
}

// storageSearch wraps storage.Search and returns an appropriate interface.
func storageSearch(deviceID string, minSize, maxSize uint64, removableOnly bool) ([]installer.Device, error) {
	devices, err := storage.Search(deviceID, minSize, maxSize, removableOnly)
	if err != nil {
		return nil, fmt.Errorf("storage.Search(%s, %d, %d, %t) returned %v", deviceID, minSize, maxSize, removableOnly, err)
	}
	// Wrap storage.Device in installer.Device
	results := []installer.Device{}
	for _, d := range devices {
		results = append(results, d)
	}
	return results, nil
}

// installerNew wraps installer.New and returns an appropriate interface.
func installerNew(config installer.Configuration) (imageInstaller, error) {
	return installer.New(config)
}
