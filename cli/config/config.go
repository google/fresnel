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

// Package config parses flags and returns a configuration for imaging a usb
package config

import (
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// Dependency injections for testing.
	currentUser = user.Current

	// Wrapped errors for testing.
	errDistro    = errors.New(`distribution selection error`)
	errDevice    = errors.New(`device error`)
	errElevation = errors.New(`elevation detection error`)
	errInput     = errors.New("invalid or missing input")
	errSeed      = errors.New("seed error")
	errTrack     = errors.New("track error")

	// Regex Matching
	regExDevicePath = regexp.MustCompile(`^[a-zA-Z0-9/]`)
	regExDeviceID   = regexp.MustCompile(`^[a-zA-Z0-9]+$`)
	regExFQDN       = regexp.MustCompile(`^(([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]*[a-zA-Z0-9])\.){2,}([A-Za-z0-9/]|[A-Za-z0-9][A-Za-z0-9\-]*[A-Za-z0-9]){2,}$`)
	regExFileName   = regexp.MustCompile(`[\w,\s-]+\.[A-Za-z.]+`)
)

// OperatingSystem is used to indicate the OS of the media to be generated.
type OperatingSystem string

const (
	// windows declares that the OS will be Windows.
	windows OperatingSystem = "windows"
	// linux declares that the OS will be Linux.
	linux OperatingSystem = "linux"
)

// distribution defines a target operating system and the configuration
// required to obtain the resources required to install it.
type distribution struct {
	os          OperatingSystem
	name        string // Friendly name: e.g. Corp Windows.
	label       string // If set, is used to set partition labels.
	seedServer  string // If set, a seed is obtained from here.
	seedFile    string // This file is hashed when obtainng a seed.
	seedDest    string // The relative path where the seed should be written.
	imageServer string // The base image is obtained here.
	images      map[string]string
	ffus        map[string]string // Contains SFU manifests names.
}

// Configuration represents the state of all flags and selections provided
// by the user when the binary is invoked.
type Configuration struct {
	cleanup  bool
	devices  []string
	distro   *distribution
	dismount bool
	ffu      bool
	update   bool
	eject    bool
	elevated bool // If the user is running as root.
	track    string
	warning  bool
}

// New generates a new configuration from flags passed on the command line.
// It performs sanity checks on those parameters.
func New(cleanup, warning, eject, ffu, update bool, devices []string, os, track, seedServer string) (*Configuration, error) {
	// Create a partial config using known good values.
	conf := &Configuration{
		cleanup:  cleanup,
		warning:  warning,
		ffu:      ffu,
		eject:    eject,
		update:   update,
	}
	if len(devices) > 0 {
		if err := conf.addDeviceList(devices); err != nil {
			return nil, fmt.Errorf("addDeviceList(%q) returned %v", devices, err)
		}
	}
	// Sanity check the chosen distribution and add it to the config.
	if err := conf.addDistro(os); err != nil {
		return nil, fmt.Errorf("addDistro(%q) returned %v", os, err)
	}
	// Sanity check the image track and add it to the config.
	if err := conf.addTrack(track); err != nil {
		return nil, err
	}
	// Sanity check the seed server and override if instructed to do so by flag.
	if err := conf.addSeedServer(seedServer); err != nil {
		return nil, err
	}
	// Determine if the user is running with elevated permissions.
	elevated, err := isElevated()
	if err != nil {
		return nil, fmt.Errorf("%w: isElevated() returned %v", errElevation, err)
	}
	conf.elevated = elevated

	return conf, nil
}

func (c *Configuration) addDistro(choice string) error {
	distro, ok := distributions[choice]
	if !ok {
		var opts []string
		for o := range distributions {
			opts = append(opts, o)
		}
		return fmt.Errorf("%w: image %q is not in %v", errDistro, choice, opts)
	}
	// If a seed server is configured, it must be accompanied by a seedFile.
	if distro.seedServer != "" && distro.seedFile == "" {
		return fmt.Errorf("%w: seedServer(%q) specified without a seedFile(%q)", errInput, distro.seedServer, distro.seedFile)
	}
	// If a seedFile is configured, a destination for the seed must be specified.
	// A seed is always stored as 'seed.json' in the location specified by
	// seedDest.
	if distro.seedFile != "" && distro.seedDest == "" {
		return fmt.Errorf("%w: seedFile(%q) specified without a destination(%q)", errSeed, distro.seedFile, distro.seedDest)
	}

	// The chosen distro is known, set it and return successfully.
	c.distro = &distro
	return nil
}

// addDeviceList sanity checks the provided devices and adds them to the
// configuration or returns an error.
func (c *Configuration) addDeviceList(devices []string) error {
	if len(devices) < 1 {
		return fmt.Errorf("%w: no devices were specified", errInput)
	}
	// Check that the device IDs appear valid. Throw an error if a partition
	// or drive letter was specified.
	for _, d := range devices {
		if !regExDeviceID.Match([]byte(d)) {
			return fmt.Errorf("%w: device(%q) must be a device ID (sda[#]), number(1-9) or disk identifier(disk[#])", errDevice, d)
		}
	}
	// Set devices in config.
	c.devices = devices
	return nil
}

func (c *Configuration) addSeedServer(fqdn string) error {
	// If no fqdn was provided, the existing default stands and we simply return.
	if fqdn == "" {
		return nil
	}
	// Check that the fqdn is correctly formatted.
	if !regExFQDN.Match([]byte(fqdn)) {
		return fmt.Errorf("%w: %q is not a valid FQDN", errSeed, fqdn)
	}
	if !strings.HasPrefix(fqdn, "http") {
		fqdn = `https://` + fqdn
	}
	// Override the default seed server if one was provided by flag.
	if fqdn != "" {
		c.distro.seedServer = fqdn
	}
	return nil
}

func (c *Configuration) addTrack(track string) error {
	// Check that a default image is avaialble in the distro.
	if _, ok := c.distro.images["default"]; !ok {
		return fmt.Errorf("%w: a default image is not available", errInput)
	}
	// If no track was provided, the existing default is used.
	if track == "" {
		c.track = "default"
		return nil
	}
	// Sanity check the specified track against the available
	// options for the distro.
	if _, ok := c.distro.images[track]; !ok {
		var opts []string
		for o := range c.distro.images {
			opts = append(opts, o)
		}
		return fmt.Errorf("%w: invalid image track requested: %q is not in %v", errTrack, track, opts)
	}
	// Set the chosen, sanity checked image.
	c.track = track
	return nil
}

// Distro returns the name of the selected distribution, or blank
// if none has been selected.
func (c *Configuration) Distro() string {
	return c.distro.name
}

// DistroLabel returns the label that should be used for media provisioned with the
// selected distribution. Can be empty.
func (c *Configuration) DistroLabel() string {
	return c.distro.label
}

// Track returns the selected track of the installer image. This generally maps
// to one of default, unstable, testing, or stable.
func (c *Configuration) Track() string {
	return c.track
}

// Image returns the full path to the raw image for this configuration.
func (c *Configuration) Image() string {
	return fmt.Sprintf(`%s/%s`, c.distro.imageServer, c.distro.images[c.track])
}

// ImageFile returns the filename of the raw image for this configuration.
func (c *Configuration) ImageFile() string {
	// Return the filename only.
	return filepath.Base(c.distro.images[c.track])
}

// Cleanup returns whether or not the cleanup of temp files was requested by
// flag.
func (c *Configuration) Cleanup() bool {
	return c.cleanup
}

// Devices returns the devices to be provisioned.
func (c *Configuration) Devices() []string {
	return c.devices
}

// UpdateDevices updates the list of devices to be provisioned.
func (c *Configuration) UpdateDevices(newDevices []string) {
	c.devices = newDevices
}

// FFU returns whether or not to place the SFU files after provisioning.
func (c *Configuration) FFU() bool {
	return c.ffu
}

// FFUManifest returns the filename of the SFU manifest file for this configuration.
func (c *Configuration) FFUManifest() string {
	// Return the filename only.
	return filepath.Base(c.distro.ffus[c.track])
}

// FFUPath returns the path to the SFU manifest.
func (c *Configuration) FFUPath() string {
	return fmt.Sprintf(`%s/%s/%s`, c.distro.imageServer, c.distro.name, c.track)
}

// PowerOff returns whether or not devices should be powered off after write
// operations.
func (c *Configuration) PowerOff() bool {
	return c.eject
}

// UpdateOnly returns whether only an update is being requested.
func (c *Configuration) UpdateOnly() bool {
	return c.update
}

// Warning returns whether or not a warning should be presented prior to
// destructive operations.
func (c *Configuration) Warning() bool {
	return c.warning
}

// SeedServer returns the configured seed server for the chosen distribution.
func (c *Configuration) SeedServer() string {
	return c.distro.seedServer
}

// SeedFile returns the path to the file that is to be hashed when obtaining
// a seed.
func (c *Configuration) SeedFile() string {
	return c.distro.seedFile
}

// SeedDest returns the relative path where a seed should be written.
func (c *Configuration) SeedDest() string {
	return c.distro.seedDest
}

// Elevated identifies if the user is running the binary with elevated
// permissions.
func (c *Configuration) Elevated() bool {
	return c.elevated
}

// String implements the fmt.Stringer interface. This allows config to be passed to
// logging for a human-readable display of the selected configuration.
func (c *Configuration) String() string {
	return fmt.Sprintf(`  Configuration:
  -------------
  Cleanup     : %t
  Elevated    : %t
  Update      : %t
  Warning     : %t

  Distribution: %q
  Label       : %q
  Track       : %q
  Image       : %q
  ImageFile   : %q
  SeedServer  : %q
  SeedFile    : %q
  SeedDest    : %q

  Targets     : %v
  PowerOff    : %t`,
		c.Cleanup(),
		c.Elevated(),
		c.UpdateOnly(),
		c.Warning(),
		c.Distro(),
		c.DistroLabel(),
		c.Track(),
		c.Image(),
		c.ImageFile(),
		c.SeedServer(),
		c.SeedFile(),
		c.SeedDest(),
		c.Devices(),
		c.PowerOff())
}

// isElevated determins if the current user is running the binary with elevated
// permissions, such as 'sudo' (Linux) or 'run as administrator' (Windows).
func isElevated() (bool, error) {
	return IsElevatedCmd()
}
