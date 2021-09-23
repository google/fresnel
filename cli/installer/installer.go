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

// Package installer provides a uniform, cross-platform implementation
// for handling OS installer provisioning for supported target platforms.
package installer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/google/fresnel/cli/console"
	"github.com/google/fresnel/models"
	"github.com/dustin/go-humanize"
	"github.com/google/logger"
	"github.com/google/winops/iso"
	"github.com/google/winops/storage"

	fetcher "github.com/google/splice/cli/appclient"
)

const oneGB = uint64(1073741824)

var (
	// Dependency injections for testing.
	currentUser     = user.Current
	connect         = fetcherConnect
	connectWithCert = tlsConnect
	downloadFile    = download
	getManifest     = readManifest
	mount           = mountISO
	selectPart      = selectPartition
	writeISOFunc    = writeISO

	// Wrapped errors for testing.
	errCache       = errors.New("missing cache")
	errConfig      = errors.New("invalid config")
	errConnect     = errors.New("connect error")
	errDownload    = errors.New("download error")
	errDevice      = errors.New("device error")
	errElevation   = errors.New("elevation is required for this operation")
	errEmpty       = errors.New("iso is empty")
	errEmptyUser   = errors.New("could not determine username")
	errFile        = errors.New("file error")
	errFinalize    = errors.New("finalize error")
	errFormat      = errors.New("format error")
	errInput       = errors.New("input error")
	errIO          = errors.New("io error")
	errManifest    = errors.New("manifest error")
	errMount       = errors.New("mount error")
	errNotEmpty    = errors.New("device not empty")
	errPartition   = errors.New("partitioning error")
	errPath        = errors.New("path error")
	errPerm        = errors.New("permissions error")
	errPost        = errors.New("http post error")
	errPrepare     = errors.New("preparation error")
	errProvision   = errors.New("provisioning error")
	errResponse    = errors.New("requested boot image is not in allowlist")
	errStatus      = errors.New("invalid status code")
	errSeed        = errors.New("invalid seed response")
	errUnmarshal   = errors.New("unmarshalling error")
	errUnsupported = errors.New("unsupported")
	errUser        = errors.New("user detection error")
	errWipe        = errors.New("device wipe error")

	// ErrLabel is made public to that callers can warn on mismatches.
	ErrLabel = errors.New(`label error`)

	// Regex for file matching.
	regExFileExt  = regexp.MustCompile(`\.[A-Za-z.]+`)
	regExFileName = regexp.MustCompile(`[\w,\s-]+\.[A-Za-z.]+$`)

	// minSFUPartSize represents the minimum partition size for SFU workflow.
	minSFUPartSize = 12 * oneGB
)

// httpDoer represents an http client that can retrieve files with the Do
// method.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Configuration represents config.Configuration.
type Configuration interface {
	DistroLabel() string
	Image() string
	ImageFile() string
	Elevated() bool
	FFU() bool
	FFUManifest() string
	FFUPath() string
	PowerOff() bool
	SeedDest() string
	SeedFile() string
	SeedServer() string
	UpdateOnly() bool
}

// Device represents storage.Device.
type Device interface {
	Dismount() error
	Eject() error
	FriendlyName() string
	Identifier() string
	Partition(string) error
	DetectPartitions(bool) error
	SelectPartition(uint64, storage.FileSystem) (*storage.Partition, error)
	Size() uint64
	Wipe() error
}

// partition represents storage.Partition.
type partition interface {
	Contents() ([]string, error)
	Erase() error
	Format(string) error
	Identifier() string
	Label() string
	Mount(string) error
	MountPoint() string
}

// isoHandler represents iso.Handler.
type isoHandler interface {
	Contents() []string
	Copy(string) error
	Dismount() error
	ImagePath() string
	MountPath() string
	Size() uint64
}

// Installer represents an operating system installer.
type Installer struct {
	cache  string        // The path where temporary files are cached.
	config Configuration // The configuration for this installer.
}

// SFUManifest struct for SFU manifest json.
type SFUManifest struct {
	Filename string
}

// New generates a new Installer from a configuration, with all the
// information needed to provision the installer on an available device.
func New(config Configuration) (*Installer, error) {
	if config == nil {
		return nil, errConfig
	}

	// Connect serves only to give an early warning if the SSO token is expired.
	// It is only called if the config specifies that a seed is required.
	if config.SeedServer() != "" {
		if _, err := connect(config.Image(), ""); err != nil {
			return nil, fmt.Errorf("fetcher.Connect(%q) returned %v: %w", config.Image(), err, errConnect)
		}
	}

	// Create a folder for temporary files. We do not need to worry about
	// cleaning up this folder as this is explicitly handled as part of
	// Finalize.
	temp, err := ioutil.TempDir("", "installer_")
	if err != nil {
		return nil, fmt.Errorf("ioutil.TempDir() returned: %v", err)
	}

	return &Installer{
		cache:  temp,
		config: config,
	}, nil
}

// fetcherConnect wraps fetcher.Connect and returns an httpDoer.
func fetcherConnect(path, user string) (httpDoer, error) {
	return fetcher.Connect(path, user)
}

// tlsConnect wraps fetcher.TLSClient and returns an httpDoer.
func tlsConnect() (httpDoer, error) {
	return fetcher.TLSClient(nil, nil)
}

// username obtains the username of the user requesting the installer. If the
// binary is running under sudo, the user who ran sudo is returned instead.
func username() (string, error) {
	u, err := currentUser()
	if err != nil {
		return "", fmt.Errorf("user.Current returned %v: %w", err, errUser)
	}
	username := u.Username
	if username == "root" {
		username = os.Getenv("SUDO_USER")
	}
	if username == "" {
		return "", errEmptyUser
	}
	return username, nil
}

// Retrieve locates and obtains the installer image, placing it in the
// temporary directory. Where additional metadata should be obtained
// or checked (such as a signature or a seed) prior to returning.
func (i *Installer) Retrieve() (err error) {
	// Confirm that the Installer has what we need.
	if i.config.Image() == "" {
		return fmt.Errorf("%w: missing image path", errConfig)
	}
	if i.cache == "" {
		return errCache
	}

	// Obtain an io.Writer for the installer image file. We will use this later
	// to provide status messages during the download process. Cleanup of this
	// temporary directory is handled as part of Finalize.
	path := filepath.Join(i.cache, i.config.ImageFile())
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("ioutil.TempFile(%q, %q) returned %v: %w", i.cache, i.config.ImageFile(), err, errFile)
	}
	// Close the file on return.
	defer func() {
		if err2 := f.Close(); err2 != nil {
			if err != nil {
				err = fmt.Errorf("%v %v", err, err2)
				return
			}
			err = err2
		}
	}()

	// Connect to the download server and retrieve the file.
	client, err := connectWithCert()
	if err != nil {
		return fmt.Errorf("fetcher.TLSClient() returned %v: %w", err, errConnect)
	}
	return downloadFile(client, i.config.Image(), f)
}

// download obtains the installer using the provided client and writes it
// to the provided io.Writer. It is aliased by downloadFile for testing
// purposes.
func download(client httpDoer, path string, w io.Writer) error {
	// Input sanity checks.
	if client == nil {
		return fmt.Errorf("empty http client: %w", errConnect)
	}
	if path == "" {
		return fmt.Errorf("image path was empty: %w", errInput)
	}
	if w == nil {
		return fmt.Errorf("no file to write to: %w", errFile)
	}

	// Obtain the file including status updates.
	req, err := http.NewRequest("GET", path, nil)
	if err != nil {
		return fmt.Errorf(`http.NewRequest("GET", %q, nil) returned %v`, path, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("get for %q returned %v: %w", path, err, errDownload)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w for %q with response %d", errStatus, path, resp.StatusCode)
	}

	// Provide updates during the download.
	fileName := regExFileName.FindString(path)
	op := "Download of " + fileName
	r := console.ProgressReader(resp.Body, op, resp.ContentLength)
	if _, err := io.Copy(w, r); err != nil {
		return fmt.Errorf("failed to write body of %q, %v: %w", path, err, errIO)
	}
	return nil
}

// Prepare takes a device and prepares it for provisioning. It supports
// device preparation based on the source image file format. Currently,
// it supports preparation for the ISO and IMG (Raw) formats.
func (i *Installer) Prepare(d Device) error {
	// Sanity check inputs.
	if i.config == nil {
		return errConfig
	}
	if i.config.ImageFile() == "" {
		return fmt.Errorf("missing image: %w", errInput)
	}
	ext := regExFileExt.FindString(i.config.ImageFile())
	if ext == "" {
		return fmt.Errorf("could not find extension for %q: %w", i.config.ImageFile(), errFile)
	}
	f, err := os.Stat(filepath.Join(i.cache, i.config.ImageFile()))
	if err != nil {
		return fmt.Errorf("%v: %w", err, errPath)
	}
	// Compensate for very small image files that can cause the wrong partition
	// to be selected.
	size := uint64(f.Size())
	if size < oneGB {
		size = oneGB
	}
	// Prepare the devices for provisioning.
	switch {
	case ext == ".iso" && i.config.UpdateOnly():
		return i.prepareForISOWithoutElevation(d, size)
	case ext == ".iso":
		return i.prepareForISOWithElevation(d, size)
	case ext == ".img":
		return i.prepareForRaw(d)
	}
	return fmt.Errorf("%q is not a supported image type: %w", ext, errProvision)
}

// prepareForISOWithElevation prepares a device to be provisioned with an
// ISO-based image. It wipes, re-partitions and re-formats the device in order
// to be prepared for file copy operations. Elevated permissions are required
// in order to prepare a device in this manner.
func (i *Installer) prepareForISOWithElevation(d Device, size uint64) error {
	logger.V(2).Infof("Preparing %q for ISO with elevation.", d.Identifier())
	if !i.config.Elevated() {
		return errElevation
	}
	// Preparing a device for an ISO follows these steps:
	// Wipe -> Re-Partition -> Format
	logger.V(2).Infof("Wiping %q.", d.Identifier())
	if err := d.Wipe(); err != nil {
		return fmt.Errorf("%w: Wipe() returned %v", errWipe, err)
	}
	logger.V(2).Infof("Partitioning %q.", d.Identifier())
	if err := d.Partition(i.config.DistroLabel()); err != nil {
		return fmt.Errorf("Partition returned %v: %w", err, errPartition)
	}
	// Formatting is not needed on Darwin.
	if runtime.GOOS == "darwin" {
		return nil
	}
	logger.V(2).Infof("Looking for a partition larger than %v on %q.", humanize.Bytes(size), d.FriendlyName())
	part, err := selectPart(d, size, "")
	if err != nil {
		return fmt.Errorf("SelectPartition(%d) returned %v: %w", size, err, errPrepare)
	}
	logger.V(2).Infof("Formatting partition on %q and setting a label of %q.", d.FriendlyName(), i.config.DistroLabel())
	if err := part.Format(i.config.DistroLabel()); err != nil {
		return fmt.Errorf("Format returned %v: %w", err, errFormat)
	}
	return nil
}

// prepareForISOWithoutElevation prepares a device to be provisioned with an
// ISO-based image. It attempts to erase the contents of the installer
// partition and checks for an appropriate label. A label mismatch suggests
// that the device may or may not result in a fully bootable image, and a
// warning is provided to state that the operation is considered "best effort"
// when there is a label mismatch. Elevated permissions are not required for
// this operation.
func (i *Installer) prepareForISOWithoutElevation(d Device, size uint64) error {
	logger.V(2).Infof("Preparing %q for ISO without elevation.", d.Identifier())
	// Preparing the device for an ISO follows these steps:
	// Erase default partition -> Check label (warn if necessary)
	part, err := selectPart(d, size, storage.FAT32)
	if err != nil {
		return fmt.Errorf("SelectPartition(%d, %q) returned %v: %w", size, storage.FAT32, err, errPartition)
	}
	base := ""
	if runtime.GOOS != "windows" {
		base = i.cache
	}
	logger.V(2).Infof("Mounting %q for erasing.", part.Identifier())
	if err := part.Mount(base); err != nil {
		return fmt.Errorf("Mount() for %q returned %v: %w", part.Identifier(), err, errMount)
	}
	logger.V(2).Infof("Preparing to erase contents of %q (device: %q, partition %q).", part.Label(), d.Identifier(), part.Identifier())
	if err := part.Erase(); err != nil {
		return fmt.Errorf("%w: partition.Erase() returned %v", errWipe, err)
	}
	if !strings.Contains(part.Label(), i.config.DistroLabel()) {
		console.Printf("\nWarning: Selected partition %q does not have a label that contains %q. Updating devices that were not previously provisioned by this tool is a best effort service. The device may not function as expected.\n", part.Identifier(), i.config.DistroLabel())
		logger.Warningf("Selected partition %q does not have a label that contains %q. Updating devices that were not previously provisioned by this tool is a best effort service. The device may not function as expected.", part.Label(), i.config.DistroLabel())
	}
	return nil
}

// readManifest ingests the downloaded FFU manifest and returns
// an array object.
func readManifest(path string) ([]SFUManifest, error) {
	// sfus represents the json struct for the SFU manifest.
	var sfus []SFUManifest
	manifest, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w failed to read file: %v", errFile, err)
	}
	console.Printf("Opened sfu manifest %s", path)
	if err := json.Unmarshal(manifest, &sfus); err != nil {
		return nil, fmt.Errorf("%w: %v", errUnmarshal, err)
	}
	return sfus, nil
}

// DownloadSFU downloads the SFU file and places it in the cache.
func (i *Installer) DownloadSFU() error {
	if i.cache == "" {
		return fmt.Errorf("missing cache location: %w", errCache)
	}
	sfus, err := getManifest(filepath.Join(i.cache, i.config.FFUManifest()))
	if err != nil {
		return fmt.Errorf("readManifest() %w: %v", errManifest, err)
	}
	// Connect to the download server and retrieve the file.
	client, err := connectWithCert()
	if err != nil {
		return fmt.Errorf("fetcher.TLSClient() returned %w: %v", errConnect, err)
	}
	for _, sfu := range sfus {
		path := filepath.Join(i.cache, sfu.Filename)
		f, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("ioutil.TempFile(%q, %q) returned %w: %v", i.cache, i.config.FFUManifest(), errFile, err)
		}
		defer f.Close()

		if err := downloadFile(client, fmt.Sprintf(`%s/%s`, i.config.FFUPath(), sfu.Filename), f); err != nil {
			return fmt.Errorf("DownloadSFU() returned %w: %v", errDownload, err)
		}

	}
	return nil
}

// PlaceSFU copies SFU files onto provisioned media from the local cache.
func (i *Installer) PlaceSFU(d Device) error {
	// Find a compatible partition to write the FFU to.
	logger.V(2).Infof("Searching for FFU %q for a %q partition larger than %v.", d.FriendlyName(), humanize.Bytes(minSFUPartSize), storage.FAT32)
	p, err := selectPart(d, minSFUPartSize, storage.FAT32)
	if err != nil {
		return fmt.Errorf("SelectPartition(%q, %q, %q) returned %w: %v", d.FriendlyName(), humanize.Bytes(minSFUPartSize), storage.FAT32, errPartition, err)
	}
	sfus, err := getManifest(filepath.Join(i.cache, i.config.FFUManifest()))
	if err != nil {
		return fmt.Errorf("getManifest() returned: %w: %v", errManifest, err)
	}
	for _, sfu := range sfus {
		sfu := sfu
		func() error{
			path := filepath.Join(i.cache, sfu.Filename)
			newPath := filepath.Join(p.MountPoint(), sfu.Filename)
			// Add colon for windows paths if its a drive root.
			if runtime.GOOS == "windows" && len(p.MountPoint()) < 2 {
				newPath = filepath.Join(fmt.Sprintf("%s:", p.MountPoint()), sfu.Filename)
			}
			source, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("%w: couldn't open file(%s) from cache: %v", errPath, path, err)
			}
			defer source.Close()
			destination, err := os.Create(newPath)
			if err != nil {
				return fmt.Errorf("%w: couldn't create target file(%s): %v", errFile, path, err)
			}
			defer destination.Close()
			cBytes, err := io.Copy(destination, source)
			if err != nil {
				return fmt.Errorf("failed to copy file to %s: %v", newPath, err)
			}
			console.Printf("Copied %d bytes", cBytes)
		return nil
		}()
	}
	return nil
}

// selectPartition wraps device.SelectPartition and returns its output wrapped
// in the partition interface.
func selectPartition(d Device, size uint64, fs storage.FileSystem) (partition, error) {
	return d.SelectPartition(size, fs)
}

// prepareForRaw prepares a device to be provisioned with an raw-based image.
// Raw only requires the device to be dismounted so that the operating system
// can write the directly to it. Though preparation does not require elevation,
// direct writes to disk always do.
func (i *Installer) prepareForRaw(d Device) error {
	return d.Dismount()
}

// Provision takes a device and provisions it with the installer. It provisions
// based on the source image file format. Each supported format enforces its
// own requirements for the device. Provision only checks that all needed
// configuration is present and that the image file has already been downloaded
// to cache.
func (i *Installer) Provision(d Device) error {
	// Sanity check inputs and configuration. Device checks are left to the
	// specific format based provisioning call itself.
	if i.config == nil {
		return errConfig
	}
	if i.cache == "" {
		return errCache
	}
	if i.config.ImageFile() == "" {
		return fmt.Errorf("missing image: %w", errInput)
	}
	ext := regExFileExt.FindString(i.config.ImageFile())
	if ext == "" {
		return fmt.Errorf("could not find extension for %q: %w", i.config.ImageFile(), errFile)
	}
	// Check that the image is already in cache.
	logger.V(2).Infof("Checking %q for existence of %q.", i.cache, i.config.ImageFile())
	path := filepath.Join(i.cache, i.config.ImageFile())
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("os.Stat(%q) returned %v: %w", path, err, errPath)
	}

	// Provision the device.
	switch ext {
	case ".img":
		return fmt.Errorf("img is not a supported image type: %w", errUnsupported)
	case ".iso":
		return i.provisionISO(d)
	}
	return fmt.Errorf("%q is an unknown image type: %w", ext, errProvision)
}

// provisionISO provisions a device with an ISO based image. It does this by
// preparing the image and mounting it, and then hands off writing to the
// device. If a seedServer is configured, it is used to add a seed to the
// device.
func (i *Installer) provisionISO(d Device) (err error) {
	// Construct the path to the ISO.
	path := filepath.Join(i.cache, i.config.ImageFile())
	// Obtain an iso.Handler by mounting the ISO.
	logger.V(2).Infof("Mounting ISO at %q.", path)
	handler, err := mount(path)
	if err != nil {
		return fmt.Errorf("mount(%q) returned %v: %w", path, err, errMount)
	}
	// Close the handler on return, capturing the error if there is one.
	defer func() {
		logger.V(2).Infof("Dismounting ISO at %q.", handler.MountPath())
		if err2 := handler.Dismount(); err2 != nil {
			if err != nil {
				err = fmt.Errorf("Dismount() for %q returned %v: %w", handler.MountPath(), err, err2)
				return
			}
			err = err2
		}
	}()
	// Set a minimum partition size so that very small ISO's don't cause us to
	// select an EFI partition unexpectedly.
	minSize := handler.Size()
	if handler.Size() < oneGB {
		minSize = oneGB
	}
	// Find a compatible partition to write to and mount if necessary.
	logger.V(2).Infof("Searching %q for a %q partition larger than %v.", d.FriendlyName(), humanize.Bytes(minSize), storage.FAT32)
	p, err := selectPart(d, minSize, storage.FAT32)
	if err != nil {
		return fmt.Errorf("SelectPartition(%q, %q, %q) returned %v: %w", d.FriendlyName(), humanize.Bytes(minSize), storage.FAT32, err, errPartition)
	}
	// Specify the cache folder as the base mount directory for non-Windows.
	base := ""
	if runtime.GOOS != "windows" {
		base = i.cache
	}
	logger.V(2).Infof("Mounting %q for writing.", p.Identifier())
	if err := p.Mount(base); err != nil {
		return fmt.Errorf("Mount() for %q returned %v: %w", p.Identifier(), err, errMount)
	}
	// Write the ISO.
	logger.V(2).Infof("Writing ISO at %q to %q.", handler.ImagePath(), d.FriendlyName())
	if err := writeISOFunc(handler, p); err != nil {
		return fmt.Errorf("writeISO() returned %v: %w", err, errProvision)
	}

	// If no seed is required, return early, otherwise, retrieve and write
	// the seed.
	if i.config.SeedServer() == "" {
		return nil
	}
	if err := i.writeSeed(handler, p); err != nil {
		return fmt.Errorf("writeSeed() returned %v", err)
	}
	return nil
}

// mountISO wraps the concrete iso.Mount return value in an equivalent interface.
func mountISO(path string) (isoHandler, error) {
	return iso.Mount(path)
}

// writeISO takes an isoHandler and copies its contents to a partition. The
// ISO is expected to be mounted and available. The contents are copied to
// the device's default partition unless a destination partition has been
// specified. The destination partition must be empty.
func writeISO(iso isoHandler, part partition) error {
	// Check inputs.
	if part == nil {
		return fmt.Errorf("partition was empty: %w", errPartition)
	}
	// Validate that the partition is ready for writing. If the drive is not
	// mounted, attempt to mount it.
	if part.MountPoint() == "" {
		return fmt.Errorf("partition is not available: %w", errMount)
	}
	contents, err := part.Contents()
	if err != nil {
		return fmt.Errorf("Contents(%q) returned %v", part.MountPoint(), err)
	}
	// Some operating systems list the device or indexes.
	if len(contents) > 2 {
		logger.V(3).Infof("contents of '%s(%s)'\n%v", part.Identifier(), part.Label(), contents)
		return fmt.Errorf("destination partition not empty: %w", errNotEmpty)
	}
	// Validate that the ISO is ready to be copied.
	if iso.MountPath() == "" {
		return fmt.Errorf("iso not mounted: %w", errInput)
	}
	if len(iso.Contents()) < 1 {
		return errEmpty
	}
	return iso.Copy(part.MountPoint())
}

// writeSeed obtains a seed and writes it to a mounted partition.
func (i *Installer) writeSeed(h isoHandler, p partition) error {
	// Input checks.
	if p.MountPoint() == "" {
		return fmt.Errorf("partition %q is not mounted: %w", p.Label(), errInput)
	}
	// We need to construct the path to the file to be hashed from configuration.
	// Then we request a seed using that hash.
	f := filepath.Join(h.MountPath(), i.config.SeedFile())
	hash, err := fileHash(f)
	if err != nil {
		return fmt.Errorf("fileHash(%q) returned %w", err, errFile)
	}
	logger.V(2).Infof("Hashed %q: %q.", f, hex.EncodeToString(hash))
	// Connect to the seed server and request the seed.
	u, err := username()
	if err != nil {
		return fmt.Errorf("username() returned %v: %w", err, errUser)
	}
	logger.V(2).Infof("Connecting to seed endpoint as user %q: %q.", u, i.config.SeedServer())
	client, err := connect(i.config.SeedServer(), u)
	if err != nil {
		return fmt.Errorf("fetcher.Connect(%q) returned %v: %w", i.config.SeedServer(), err, errConnect)
	}
	logger.V(2).Infof("Requesting seed from %q.", i.config.SeedServer())
	sr, err := seedRequest(client, string(hash), i.config)
	if err != nil {
		return fmt.Errorf("seedRequest returned %v: %w", err, errDownload)
	}
	seedFile := models.SeedFile{
		Seed:      sr.Seed,
		Signature: sr.Signature,
	}
	// See that the seed contents are human readable.
	content, err := json.MarshalIndent(seedFile, "", "")
	if err != nil {
		return fmt.Errorf("json.MarshalIndent(%v) returned: %v", seedFile, err)
	}
	logger.V(3).Infof("Retrieved seed: %s", content)
	// Determine where the seed should be written to and write it. Accommodate
	// for Windows not understanding drive letters vs relative paths.
	root := p.MountPoint()
	if runtime.GOOS == "windows" && !strings.Contains(root, `:`) {
		root = root + `:`
	}
	path := filepath.Join(root, i.config.SeedDest())
	logger.V(2).Infof("Creating seed directory: %q.", path)
	// Permissions = owner:read/write/execute, group:read/execute"
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("os.MkdirAll(%q, 0755) returned %v: %w", path, err, errPerm)
	}
	s := filepath.Join(path, `/seed.json`)
	logger.V(2).Infof("Writing seed: %q.", s)
	// Permissions = owner:read/write, group:read"
	if err := ioutil.WriteFile(s, content, 0644); err != nil {
		return fmt.Errorf("ioutil.WriteFile(%q) returned %v: %w", s, err, errIO)
	}
	return nil
}

// fileHash returns a the SHA-256 hash of the file at the provided path.
func fileHash(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("path was empty: %w", errInput)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("os.Open(%q) returned %v: %w", path, err, errPath)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("hashing %q returned %v: %w", f.Name(), path, errIO)
	}
	hash := h.Sum(nil)
	return hash, nil
}

// seedRequest obtains a signed seed for the installer and returns it for use.
func seedRequest(client httpDoer, hash string, config Configuration) (*models.SeedResponse, error) {
	if hash == "" {
		return nil, fmt.Errorf("missing hash: %w", errInput)
	}
	// Build the request.
	sr := &models.SeedRequest{
		Hash: []byte(hash),
	}
	reqBody, err := json.Marshal(sr)
	if err != nil {
		return nil, fmt.Errorf("could not marshal seed request(%+v): %v", sr, err)
	}
	req, err := http.NewRequest("POST", config.SeedServer(), bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("error composing post request %v: %w", err, errConnect)
	}
	req.Header.Set("Content-Type", "application/json")

	// Post the request and obtain a response.
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errPost, err)
	}
	defer resp.Body.Close()
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %v", err)
	}
	// If the server responded that the hash is not in the allowlist, return.
	if strings.Contains(fmt.Sprintf("%s", respBody), "not in allowlist") {
		return nil, fmt.Errorf("%w: %q", errResponse, hash)
	}

	r := &models.SeedResponse{}
	if err := json.Unmarshal(respBody, r); err != nil {
		return nil, fmt.Errorf("json.Unmarhsal(%s) returned %v: %w", respBody, err, errFormat)
	}
	if r.ErrorCode != models.StatusSuccess {
		return nil, fmt.Errorf("%w: %v %d", errSeed, r.Status, r.ErrorCode)
	}
	return r, nil
}

// Finalize performs post-provisioning tasks for a device. It is meant to
// be called after all provisioning tasks are completed. For example, if a set
// of devices are being provisioned, it can be called at the end of the process
// so that artifacts like downloaded images can be obtained just once and
// re-used during Preparation and Provisioning steps. If the cache exists
// it is automatically cleaned up. Optionally, the device can also be
// dismounted and/or powered off during the Finalize step.
func (i *Installer) Finalize(devices []Device, dismount bool) error {
	for _, device := range devices {
		if dismount {
			logger.V(2).Infof("Refreshing partition information for %q prior to dismount.", device.Identifier())
			if err := device.DetectPartitions(false); err != nil {
				return fmt.Errorf("DetectPartitions() for %q returned %v: %w", device.Identifier(), err, errFinalize)
			}
			console.Printf("Dismounting device %q.", device.Identifier())
			logger.V(2).Infof("Dismounting device %q.", device.Identifier())
			if err := device.Dismount(); err != nil {
				return fmt.Errorf("Dismount(%s) returned %v: %w", device.Identifier(), err, errDevice)
			}
		}
		if i.config.PowerOff() {
			console.Printf("Ejecting device %q.", device.Identifier())
			logger.V(2).Infof("Ejecting device %q.", device.Identifier())
			if err := device.Eject(); err != nil {
				return fmt.Errorf("Eject(%s) returned %v: %w", device.Identifier(), err, errIO)
			}
		}
	}
	// Clean up the cache if it still exists. os.RemoveAll returns nil if the
	// path doesn't exist, which is convenient for us here.
	logger.V(2).Infof("Cleaning up installer cache %q.", i.cache)
	if err := os.RemoveAll(i.cache); err != nil {
		return fmt.Errorf("os.RemoveAll(%s) returned %v: %w", i.cache, err, errPath)
	}
	return nil
}

// Cache returns the location of the cache folder for a given installer.
func (i *Installer) Cache() string {
	return i.cache
}
