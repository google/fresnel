// Copyright 2026 Google LLC
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

package write

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"flag"
	"github.com/google/fresnel/cli/config"
	"github.com/google/fresnel/cli/console"
	"github.com/google/fresnel/cli/installer"
	"github.com/google/deck"
	"github.com/dustin/go-humanize"
	"github.com/google/winops/storage"
)

const (
	oneMiB uint64 = 1 << 20

	// imageExt is the extension used for image files. The Device Recovery
	// Utility's "Use local image" file picker lists .bin files but not .img
	// files.
	imageExt = ".bin"

	// imagePartialExt marks an image file that is still being written. The file
	// gets its final name only after it is complete.
	imagePartialExt = ".partial"

	// imageLayoutOverhead covers the 4096-sector partition offset used by
	// storage.Partition, the backup GPT, and alignment slack.
	imageLayoutOverhead = 8 * oneMiB

	// imageMinHeadroom is the minimum free space left on the installer
	// partition. Partitions do not grow when the image is written to a larger
	// drive, so later refreshes with the update command need this headroom.
	imageMinHeadroom = 512 * oneMiB

	// loopControl is present when the kernel supports loop devices.
	loopControl = "/dev/loop-control"
)

var (
	// Wrapped errors for testing.
	errImageUsage  = errors.New("image file usage error")
	errImageFile   = errors.New("image file error")
	errAttach      = errors.New("loop device attach error")
	errDetach      = errors.New("loop device detach error")
	errInterrupted = errors.New("interrupted")

	// usbDriveClasses lists common USB drive sizes with a conservative usable
	// capacity in bytes. Drives are sold in decimal gigabytes and are often a
	// few percent smaller than their nominal size.
	usbDriveClasses = []struct {
		nominalGB int
		usable    uint64
	}{
		{4, 3_700_000_000},
		{8, 7_400_000_000},
		{16, 14_800_000_000},
		{32, 29_600_000_000},
		{64, 59_200_000_000},
		{128, 118_400_000_000},
	}

	// imageTools are the external commands used to build an image file.
	imageTools = []string{"sudo", "losetup", "lsblk", "parted", "wipefs", "mkfs.vfat", "fatlabel", "mount", "umount"}

	loopDevRE = regexp.MustCompile(`^/dev/(loop[0-9]+)$`)
	loopIDRE  = regexp.MustCompile(`^loop[0-9]+$`)
	// loopAssocRE matches a device in 'losetup --associated' output, such as
	// "/dev/loop7: [2049]:1234 (/home/user/out.bin.partial)".
	loopAssocRE = regexp.MustCompile(`^/dev/(loop[0-9]+):`)

	// Dependency injections for testing.
	executeImage           = runImage
	imageNow               = time.Now
	imageSleep             = time.Sleep
	imageGetenv            = os.Getenv
	imageGeteuid           = os.Geteuid
	imageUserHomeDir       = os.UserHomeDir
	imageLookupHome        = lookupHome
	imageLookPath          = findTool
	imageStat              = os.Stat
	imageRunCmd            = runCmd
	imageFreeBytes         = freeBytes
	imageFileInode         = fileInode
	imageNewDevice         = newStorageDevice
	imagePartitionID       = func(p *storage.Partition) string { return p.Identifier() }
	imagePartitionTimeout  = 15 * time.Second
	imageDetachTimeout     = 5 * time.Second
	imagePollInterval      = 250 * time.Millisecond
	imageToolSearchFolders = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"}
)

// imageFileFlag is the value of --image_file. It acts like a boolean flag when
// it is given without a value and like a string flag when it is given a value
// with '=', which makes the output path optional.
type imageFileFlag struct {
	enabled bool
	path    string
}

// String implements flag.Value.
func (v *imageFileFlag) String() string {
	if v == nil || !v.enabled {
		return ""
	}
	if v.path == "" {
		return "true"
	}
	return v.path
}

// Get implements flag.Getter.
func (v *imageFileFlag) Get() any {
	return v.String()
}

// IsBoolFlag allows --image_file to be given without a value.
func (v *imageFileFlag) IsBoolFlag() bool {
	return true
}

// Set implements flag.Value. Values that parse as booleans turn image mode on
// or off, and any other value is the output path. Paths that look like a
// boolean, such as "true", need a "./" prefix.
func (v *imageFileFlag) Set(s string) error {
	if s == "" {
		return errors.New("--image_file= requires a path; omit '=' to use the default path")
	}
	if b, err := strconv.ParseBool(s); err == nil {
		v.enabled, v.path = b, ""
		return nil
	}
	v.enabled, v.path = true, s
	return nil
}

// validateImageMode rejects flags that do not apply when writing to an image
// file. When --image_file was given without a value, a single positional
// argument is accepted as the output path and stored in c.image.path.
func validateImageMode(c *writeCmd, f *flag.FlagSet) error {
	if !imageModeSupported {
		return fmt.Errorf("%w: --image_file is only supported on Linux", errImageUsage)
	}
	switch {
	case c.update:
		return fmt.Errorf("%w: --image_file cannot be combined with update", errImageUsage)
	case c.allDrives:
		return fmt.Errorf("%w: --image_file cannot be combined with --all", errImageUsage)
	case c.listFixed:
		return fmt.Errorf("%w: --image_file cannot be combined with --show_fixed", errImageUsage)
	case c.eject:
		return fmt.Errorf("%w: --image_file cannot be combined with --eject", errImageUsage)
	case !c.dismount:
		return fmt.Errorf("%w: --image_file requires --dismount", errImageUsage)
	}
	args := f.Args()
	switch {
	case len(args) > 1:
		return fmt.Errorf("%w: expected at most one output path with --image_file, got %q; use --image_file=PATH and put all other flags before it", errImageUsage, args)
	case len(args) == 1 && c.image.path != "":
		return fmt.Errorf("%w: both --image_file=%q and %q were given; devices cannot be written in image mode", errImageUsage, c.image.path, args[0])
	case len(args) == 1:
		c.image.path = args[0]
	}
	return nil
}

// runImage provisions the installer into a raw disk image file instead of a
// storage device. The file is attached as a loop device and provisioned by the
// same installer steps that are used for USB drives.
func runImage(ctx context.Context, c *writeCmd, _ *flag.FlagSet) error {
	// The image is meant to be written to removable media, so the removable
	// media policy applies to image files as well.
	if err := funcUSBPermissions(); err != nil {
		if errors.Is(err, config.ErrWritePerms) {
			console.Print(err)
		}
		deck.Warning(err)
		return config.ErrUSBwriteAccess
	}
	if err := imagePreflight(); err != nil {
		return err
	}
	conf, err := config.New(c.cleanup, false, false, c.ffu, false, nil, c.distro, c.track, c.confTrack, c.seedServer)
	if err != nil {
		return fmt.Errorf("%w: config.New(cleanup: %t, ffu: %t, distro: %s, track: %s, seedServer: %s) returned %v",
			errConfig, c.cleanup, c.ffu, c.distro, c.track, c.seedServer, err)
	}
	// Only the ISO provisioning path builds a bootable layout from scratch.
	if ext := strings.ToLower(filepath.Ext(conf.ImageFile())); ext != ".iso" {
		return fmt.Errorf("%w: distribution %q uses a %q image; only ISO based distributions can be written to an image file", errImageFile, conf.Distro(), ext)
	}
	if !conf.Elevated() {
		return fmt.Errorf("%w: elevated permissions are required to use --image_file, try again using 'sudo'", errElevation)
	}
	final, err := resolveImagePath(c.image.path, conf.Distro(), conf.Track())
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Ext(final), imageExt) {
		console.Printf("Warning: %q does not end in %s. The Device Recovery Utility only lists %s files for local images.", final, imageExt, imageExt)
	}
	// Fail before downloading anything if the output already exists. publish
	// checks again so that an existing file is never replaced.
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("%w: %q already exists", errImageFile, final)
	}

	stop := notifyInterrupt(ctx)
	defer stop()
	j := &imageJob{final: final, partial: final + imagePartialExt}
	buildErr := j.build(ctx, conf)
	cleanErr := j.cleanup()
	if buildErr == nil && cleanErr == nil {
		if buildErr = j.publish(); buildErr == nil {
			j.printSuccess(conf.SeedServer() != "")
			return nil
		}
	}
	j.discard()
	if cleanErr != nil {
		console.Print(j.recoveryHint())
	}
	return errors.Join(buildErr, cleanErr)
}

// imagePreflight checks for loop device support and the required tools before
// anything is downloaded.
func imagePreflight() error {
	for _, t := range imageTools {
		if _, err := imageLookPath(t); err != nil {
			return fmt.Errorf("%w: required command %q was not found: %v", errImageFile, t, err)
		}
	}
	if _, err := imageStat(loopControl); err != nil {
		return fmt.Errorf("%w: loop devices are not available (%v); --image_file needs a Linux system or virtual machine with loop device support, unprivileged containers such as Crostini are not supported", errImageFile, err)
	}
	return nil
}

// findTool looks for a command in PATH and in the system folders that sudo
// typically searches but unprivileged users often do not have in PATH.
func findTool(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range imageToolSearchFolders {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%q not found in PATH or %v", name, imageToolSearchFolders)
}

// defaultImageName returns the file name that is used when --image_file has no
// path or names a folder.
func defaultImageName(distro, track string, t time.Time) string {
	return fmt.Sprintf("%s-%s-%s-%s%s", binaryName, distro, track, t.Format("20060102-150405"), imageExt)
}

// resolveImagePath returns the absolute path of the image file. An empty arg
// selects the invoking user's home folder, and a folder gets the default file
// name.
func resolveImagePath(arg, distro, track string) (string, error) {
	name := defaultImageName(distro, track, imageNow())
	if arg == "" {
		home, err := invokingHome()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, name), nil
	}
	// The shell does not expand '~' after '=', so expand it here.
	if arg == "~" || strings.HasPrefix(arg, "~/") {
		home, err := invokingHome()
		if err != nil {
			return "", err
		}
		arg = filepath.Join(home, strings.TrimPrefix(arg, "~"))
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", fmt.Errorf("%w: filepath.Abs(%q) returned %v", errImageFile, arg, err)
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		return filepath.Join(abs, name), nil
	}
	parent := filepath.Dir(abs)
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%w: output folder %q does not exist", errImageFile, parent)
	}
	return abs, nil
}

// invokingHome returns the home folder of the user who ran the command. When
// running under sudo, this is the home folder of the user who ran sudo rather
// than root's.
func invokingHome() (string, error) {
	if imageGeteuid() != 0 {
		home, err := imageUserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: could not determine the home folder (%v); pass --image_file=PATH", errImageFile, err)
		}
		return home, nil
	}
	uid := imageGetenv("SUDO_UID")
	if uid == "" || uid == "0" {
		return "", fmt.Errorf("%w: cannot determine the invoking user's home folder when running as root without sudo; pass --image_file=PATH", errImageFile)
	}
	home, err := imageLookupHome(uid)
	if err != nil {
		return "", fmt.Errorf("%w: could not determine the home folder for uid %s (%v); pass --image_file=PATH", errImageFile, uid, err)
	}
	return home, nil
}

// lookupHome returns the home folder for a uid.
func lookupHome(uid string) (string, error) {
	if u, err := user.LookupId(uid); err == nil && u.HomeDir != "" {
		return u.HomeDir, nil
	}
	// os/user may not consult NSS, so accounts from a directory service are
	// looked up with getent.
	out, err := exec.Command("getent", "passwd", uid).Output()
	if err != nil {
		return "", fmt.Errorf("getent passwd %s returned %v", uid, err)
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) < 7 || fields[5] == "" {
		return "", fmt.Errorf("unexpected getent output %q", out)
	}
	return fields[5], nil
}

// invokingOwner returns the uid and gid of the user who ran sudo, so that
// files created as root can be handed back to that user. ok is false when the
// owner does not need to change.
func invokingOwner() (uid, gid int, ok bool, err error) {
	if imageGeteuid() != 0 {
		return 0, 0, false, nil
	}
	su, sg := imageGetenv("SUDO_UID"), imageGetenv("SUDO_GID")
	if su == "" || su == "0" {
		return 0, 0, false, nil
	}
	if uid, err = strconv.Atoi(su); err != nil {
		return 0, 0, false, fmt.Errorf("invalid SUDO_UID %q: %v", su, err)
	}
	if gid, err = strconv.Atoi(sg); err != nil {
		return 0, 0, false, fmt.Errorf("invalid SUDO_GID %q: %v", sg, err)
	}
	return uid, gid, true, nil
}

// roundUpMiB rounds n up to a whole number of MiB.
func roundUpMiB(n uint64) uint64 {
	return (n + oneMiB - 1) / oneMiB * oneMiB
}

// computeImageSize returns the image size for a payload of the given size and
// the smallest common USB drive size that the image fits on, or 0 if it is
// larger than all of them.
func computeImageSize(payload uint64) (uint64, int) {
	// installer.Prepare and provisionISO require a partition of at least the
	// payload size and never less than 1GB.
	floor := payload
	if floor < uint64(oneGB) {
		floor = uint64(oneGB)
	}
	// FAT32 allocation tables use about 1/512 of the volume, and 1/256 leaves
	// room for cluster rounding.
	hardMin := roundUpMiB(floor + floor/256 + imageLayoutOverhead)
	headroom := payload / 10
	if headroom < imageMinHeadroom {
		headroom = imageMinHeadroom
	}
	want := roundUpMiB(hardMin + headroom)
	// Headroom is capped so that it never pushes the image onto a larger class
	// of USB drive than the payload needs.
	for _, d := range usbDriveClasses {
		if hardMin <= d.usable {
			if limit := d.usable / oneMiB * oneMiB; want > limit {
				want = limit
			}
			return want, d.nominalGB
		}
	}
	return want, 0
}

// runCmd runs a command and returns its standard output. Standard error is
// kept separate so that it cannot be mistaken for output, and it is included
// in the error.
func runCmd(name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %q returned %v: %s", name, args, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// attachLoop attaches path to a free loop device and returns its id, such as
// "loop7".
func attachLoop(path string) (string, error) {
	out, err := imageRunCmd("sudo", "losetup", "--find", "--show", "--partscan", path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errAttach, err)
	}
	m := loopDevRE.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", fmt.Errorf("%w: unexpected losetup output %q", errAttach, out)
	}
	return m[1], nil
}

// detachLoop detaches a loop device.
func detachLoop(id string) error {
	if !loopIDRE.MatchString(id) {
		return fmt.Errorf("%w: %q is not a loop device", errDetach, id)
	}
	if _, err := imageRunCmd("sudo", "losetup", "--detach", "/dev/"+id); err != nil {
		return fmt.Errorf("%w: %v", errDetach, err)
	}
	return nil
}

// flexUint decodes JSON numbers that older util-linux releases print as
// strings.
type flexUint uint64

// UnmarshalJSON implements json.Unmarshaler.
func (u *flexUint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*u = 0
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return err
	}
	*u = flexUint(n)
	return nil
}

// loopInfo models a device from 'losetup --list --json'.
type loopInfo struct {
	Name      string   `json:"name"`
	BackFile  string   `json:"back-file"`
	BackIno   flexUint `json:"back-ino"`
	Offset    flexUint `json:"offset"`
	SizeLimit flexUint `json:"sizelimit"`
}

// blockInfo models a device from 'lsblk --json'.
type blockInfo struct {
	KName      string   `json:"kname"`
	Type       string   `json:"type"`
	Size       flexUint `json:"size"`
	PKName     string   `json:"pkname"`
	MountPoint string   `json:"mountpoint"`
}

// listBlocks returns a loop device and its partitions.
func listBlocks(id string) ([]blockInfo, error) {
	out, err := imageRunCmd("lsblk", "--json", "--bytes", "--output", "KNAME,TYPE,SIZE,PKNAME,MOUNTPOINT", "/dev/"+id)
	if err != nil {
		return nil, err
	}
	result := struct {
		BlockDevices []blockInfo `json:"blockdevices"`
	}{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("json.Unmarshal(%q) returned %v", out, err)
	}
	return result.BlockDevices, nil
}

// verifyLoop confirms that loop device id is backed by the image file before
// anything destructive runs on it.
func verifyLoop(id string, ino uint64, path string, size uint64) error {
	out, err := imageRunCmd("losetup", "--list", "--json", "--output", "NAME,BACK-FILE,BACK-INO,OFFSET,SIZELIMIT", "/dev/"+id)
	if err != nil {
		return fmt.Errorf("%w: %v", errAttach, err)
	}
	result := struct {
		LoopDevices []loopInfo `json:"loopdevices"`
	}{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return fmt.Errorf("%w: json.Unmarshal(%q) returned %v", errAttach, out, err)
	}
	if len(result.LoopDevices) != 1 || result.LoopDevices[0].Name != "/dev/"+id {
		return fmt.Errorf("%w: unexpected losetup output for /dev/%s: %q", errAttach, id, out)
	}
	l := result.LoopDevices[0]
	// Only the file name is compared. losetup may report the backing file by
	// a different path (for example through a symlink or bind mount), and
	// truncates long paths, so the inode is the real identity check. The
	// " (deleted)" suffix that losetup adds for an unlinked file is stripped;
	// the inode still identifies the file, and writing to it is harmless.
	backFile := filepath.Base(strings.TrimSuffix(l.BackFile, " (deleted)"))
	switch {
	case uint64(l.BackIno) != ino:
		return fmt.Errorf("%w: /dev/%s is backed by inode %d, want %d", errAttach, id, l.BackIno, ino)
	case backFile != filepath.Base(path):
		return fmt.Errorf("%w: /dev/%s is backed by %q, want %q", errAttach, id, l.BackFile, path)
	case l.Offset != 0 || l.SizeLimit != 0:
		return fmt.Errorf("%w: /dev/%s has offset %d and size limit %d, want 0", errAttach, id, l.Offset, l.SizeLimit)
	}
	blocks, err := listBlocks(id)
	if err != nil {
		return fmt.Errorf("%w: %v", errAttach, err)
	}
	if len(blocks) != 1 {
		return fmt.Errorf("%w: /dev/%s unexpectedly has partitions: %+v", errAttach, id, blocks)
	}
	b := blocks[0]
	switch {
	case b.KName != id || b.Type != "loop":
		return fmt.Errorf("%w: lsblk reports %q of type %q, want %q of type loop", errAttach, b.KName, b.Type, id)
	case uint64(b.Size) != size:
		return fmt.Errorf("%w: /dev/%s is %d bytes, want %d", errAttach, id, b.Size, size)
	case b.MountPoint != "":
		return fmt.Errorf("%w: /dev/%s is mounted at %q", errAttach, id, b.MountPoint)
	}
	return nil
}

// isLoopChild reports whether part is a partition of loop device id.
func isLoopChild(id, part string) bool {
	rest, ok := strings.CutPrefix(part, id+"p")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// waitForLoopPartition waits for the kernel to report the first partition of
// loop device id and for its device node to exist.
func waitForLoopPartition(id string) error {
	want := id + "p1"
	deadline := imageNow().Add(imagePartitionTimeout)
	probed := false
	for {
		// Best effort: udevadm is missing on some systems.
		imageRunCmd("udevadm", "settle", "--timeout=5")
		if blocks, err := listBlocks(id); err == nil {
			for _, b := range blocks {
				if b.KName == want && b.Type == "part" && b.PKName == id {
					if _, err := imageStat("/dev/" + want); err == nil {
						return nil
					}
				}
			}
		}
		if !probed {
			// Best effort: ask the kernel to reread the partition table once.
			imageRunCmd("sudo", "partprobe", "/dev/"+id)
			probed = true
		}
		if imageNow().After(deadline) {
			return fmt.Errorf("%w: partition /dev/%s did not appear within %v", errImageFile, want, imagePartitionTimeout)
		}
		imageSleep(imagePollInterval)
	}
}

// assertUnmounted returns an error if loop device id or any of its partitions
// is mounted.
func assertUnmounted(id string) error {
	blocks, err := listBlocks(id)
	if err != nil {
		return fmt.Errorf("%w: could not confirm that /dev/%s is unmounted: %v", errDetach, id, err)
	}
	var mounted []string
	for _, b := range blocks {
		if b.MountPoint != "" {
			mounted = append(mounted, fmt.Sprintf("/dev/%s at %s", b.KName, b.MountPoint))
		}
	}
	if len(mounted) > 0 {
		return fmt.Errorf("%w: still mounted: %s", errDetach, strings.Join(mounted, ", "))
	}
	return nil
}

// assertDetached waits for path to have no loop devices attached. losetup
// reports success even when a busy device is only marked for automatic
// detach, so the result has to be checked.
func assertDetached(path string) error {
	deadline := imageNow().Add(imageDetachTimeout)
	for {
		out, err := imageRunCmd("losetup", "--associated", path)
		if err == nil && strings.TrimSpace(out) == "" {
			return nil
		}
		if imageNow().After(deadline) {
			if err != nil {
				return fmt.Errorf("%w: could not confirm that %q is detached: %v", errDetach, path, err)
			}
			return fmt.Errorf("%w: %q is still attached: %s", errDetach, path, strings.TrimSpace(out))
		}
		imageSleep(imagePollInterval)
	}
}

// newStorageDevice wraps storage.New for a loop device.
func newStorageDevice(id string) (installer.Device, error) {
	d, err := storage.New(id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("storage.New(%q) returned no device", id)
	}
	return d, nil
}

// imageDevice adapts a loop device to installer.Device. The storage package
// names the first partition "<id>1", which is right for sdX devices but not
// for loop devices, whose partitions are "<id>p1". For loop1 that name is
// loop11, which can be an unrelated loop device. imageDevice therefore waits
// for the real partition, refreshes the partition list, and refuses any
// partition that is not a child of the loop device.
type imageDevice struct {
	inner installer.Device
	id    string
	path  string
}

// Ensure imageDevice implements installer.Device.
var _ installer.Device = (*imageDevice)(nil)

// Dismount implements installer.Device.
func (d *imageDevice) Dismount() error {
	return d.inner.Dismount()
}

// Eject implements installer.Device. Loop devices cannot be ejected.
func (d *imageDevice) Eject() error {
	return nil
}

// FriendlyName implements installer.Device.
func (d *imageDevice) FriendlyName() string {
	return fmt.Sprintf("image file %s (/dev/%s)", d.path, d.id)
}

// Identifier implements installer.Device.
func (d *imageDevice) Identifier() string {
	return d.inner.Identifier()
}

// Partition implements installer.Device.
func (d *imageDevice) Partition(label string) error {
	if err := d.inner.Partition(label); err != nil {
		return err
	}
	if err := waitForLoopPartition(d.id); err != nil {
		return err
	}
	// Replace the partition list recorded by Partition with the one the
	// kernel reports.
	return d.inner.DetectPartitions(false)
}

// DetectPartitions implements installer.Device.
func (d *imageDevice) DetectPartitions(mount bool) error {
	return d.inner.DetectPartitions(mount)
}

// SelectPartition implements installer.Device.
func (d *imageDevice) SelectPartition(minSize uint64, fs storage.FileSystem) (*storage.Partition, error) {
	p, err := d.inner.SelectPartition(minSize, fs)
	if err != nil {
		return nil, err
	}
	if id := imagePartitionID(p); !isLoopChild(d.id, id) {
		return nil, fmt.Errorf("%w: refusing to use %q, which is not a partition of /dev/%s", errImageFile, id, d.id)
	}
	return p, nil
}

// Size implements installer.Device.
func (d *imageDevice) Size() uint64 {
	return d.inner.Size()
}

// Wipe implements installer.Device.
func (d *imageDevice) Wipe() error {
	return d.inner.Wipe()
}

// notifyInterrupt tells the user that cleanup is underway when ctx is
// cancelled. main cancels the context on the first SIGINT or SIGTERM and
// ignores these signals afterwards, so cleanup cannot be skipped with a second
// interrupt. The returned function stops the notification and waits for it to
// finish.
func notifyInterrupt(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			console.Print("Interrupt received; cleaning up.\n")
		case <-done:
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// imageJob tracks what has been set up while building an image file so that
// cleanup undoes exactly that.
type imageJob struct {
	final, partial string

	file *os.File    // The open partial file, nil once closed.
	info os.FileInfo // Identifies the partial file this job created.
	inst imageInstaller
	// attached is true from the attach attempt until the partial file is
	// confirmed to be detached, because losetup can attach the file even
	// when it reports an error.
	attached bool
	loopID   string // The verified loop device, empty once detached.
	dev      installer.Device
	size     uint64
	driveGB  int
}

// checkpoint returns an error if the user asked to stop.
func checkpoint(ctx context.Context, next string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w before %s", errInterrupted, next)
	}
	return nil
}

// build creates the partial image file and provisions the installer into it.
func (j *imageJob) build(ctx context.Context, conf *config.Configuration) error {
	f, err := os.OpenFile(j.partial, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %q already exists; remove it if it was left behind by an interrupted run", errImageFile, j.partial)
		}
		return fmt.Errorf("%w: %v", errImageFile, err)
	}
	j.file = f
	if j.info, err = f.Stat(); err != nil {
		return fmt.Errorf("%w: %v", errImageFile, err)
	}
	// The file may hold a seed, so only its owner can read it.
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("%w: %v", errImageFile, err)
	}
	uid, gid, ok, err := invokingOwner()
	if err != nil {
		return fmt.Errorf("%w: %v", errImageFile, err)
	}
	if ok {
		// Hand the file to the user who ran sudo so that they can open and
		// delete it, for example from the ChromeOS Files app.
		if err := f.Chown(uid, gid); err != nil {
			return fmt.Errorf("%w: %v", errImageFile, err)
		}
	}

	i, err := newInstaller(conf)
	if err != nil {
		return fmt.Errorf("%w: installer.New() returned %v", errInstaller, err)
	}
	j.inst = i
	if err := checkpoint(ctx, "download"); err != nil {
		return err
	}
	console.Printf("\nRetrieving image...")
	deck.Infof("Retrieving image...\n    %s ->\n    %s\n\n", conf.ImagePath(), i.Cache())
	if err := i.Retrieve(); err != nil {
		return fmt.Errorf("%w: Retrieve() returned %v", errRetrieve, err)
	}
	if err := checkpoint(ctx, "sizing"); err != nil {
		return err
	}
	payload, err := payloadSize(i.Cache(), conf)
	if err != nil {
		return err
	}
	j.size, j.driveGB = computeImageSize(payload)
	free, err := imageFreeBytes(filepath.Dir(j.partial))
	if err != nil {
		return fmt.Errorf("%w: could not check free space: %v", errImageFile, err)
	}
	if free < j.size {
		return fmt.Errorf("%w: %s is needed in %q but only %s is free", errImageFile, humanize.IBytes(j.size), filepath.Dir(j.partial), humanize.IBytes(free))
	}
	if err := f.Truncate(int64(j.size)); err != nil {
		return fmt.Errorf("%w: %v", errImageFile, err)
	}
	if err := checkpoint(ctx, "attaching the image file"); err != nil {
		return err
	}
	j.attached = true
	id, err := attachLoop(j.partial)
	if err != nil {
		return err
	}
	ino, err := imageFileInode(f)
	if err != nil {
		return fmt.Errorf("%w: %v", errAttach, err)
	}
	if err := verifyLoop(id, ino, j.partial, j.size); err != nil {
		return err
	}
	// Only a verified device is used and detached by name. Until then,
	// cleanup only detaches devices that losetup reports for the partial
	// file.
	j.loopID = id
	inner, err := imageNewDevice(id)
	if err != nil {
		return fmt.Errorf("%w: %v", errDevice, err)
	}
	j.dev = &imageDevice{inner: inner, id: id, path: j.final}

	// The steps below mirror the per-device loop in run().
	if err := checkpoint(ctx, "preparing the image file"); err != nil {
		return err
	}
	console.Printf("\nPreparing %s...", j.dev.FriendlyName())
	deck.InfofA("Preparing %s...", j.dev.FriendlyName()).With(deck.V(1)).Go()
	if err := i.Prepare(j.dev); err != nil {
		return fmt.Errorf("%w: Prepare(%q) returned %v", errPrepare, j.dev.FriendlyName(), err)
	}
	if err := checkpoint(ctx, "provisioning the image file"); err != nil {
		return err
	}
	console.Printf("Provisioning %s...", j.dev.FriendlyName())
	deck.InfofA("Provisioning %s...", j.dev.FriendlyName()).With(deck.V(1)).Go()
	if err := i.Provision(j.dev); err != nil {
		return fmt.Errorf("%w: Provision(%q) returned %v", errProvision, j.dev.FriendlyName(), err)
	}
	return nil
}

// payloadSize returns the number of bytes that are copied onto the installer
// partition.
func payloadSize(cache string, conf *config.Configuration) (uint64, error) {
	files := []string{conf.ImageFile()}
	if conf.NeedsConfig() {
		files = append(files, conf.FFUConfFile())
	}
	var total uint64
	for _, name := range files {
		fi, err := os.Stat(filepath.Join(cache, name))
		if err != nil {
			return 0, fmt.Errorf("%w: %v", errImageFile, err)
		}
		total += uint64(fi.Size())
	}
	return total, nil
}

// cleanup unmounts, finalizes, and detaches whatever build set up. It stops
// before deleting anything while a partition is still mounted, because
// installer.Finalize removes the cache folder that contains the mount point.
func (j *imageJob) cleanup() error {
	var errs []error
	if j.dev != nil {
		if err := j.dev.DetectPartitions(false); err != nil {
			errs = append(errs, fmt.Errorf("%w: DetectPartitions() returned %v", errFinalize, err))
		} else if err := j.dev.Dismount(); err != nil {
			errs = append(errs, fmt.Errorf("%w: Dismount() returned %v", errFinalize, err))
		}
		if err := assertUnmounted(j.loopID); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	if j.inst != nil {
		var targets []installer.Device
		if j.dev != nil {
			targets = append(targets, j.dev)
		}
		if err := j.inst.Finalize(targets, true); err != nil {
			errs = append(errs, fmt.Errorf("%w: Finalize() returned %v", errFinalize, err))
		}
	}
	if j.loopID != "" {
		// losetup only marks a mounted loop device for automatic detach and
		// still reports success, so check first.
		if err := assertUnmounted(j.loopID); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := detachLoop(j.loopID); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := assertDetached(j.partial); err != nil {
			return errors.Join(append(errs, err)...)
		}
		j.loopID = ""
		j.attached = false
	} else if j.attached {
		// The device that losetup printed was never verified, so it is not
		// detached by name. The partial file was created by this job, so any
		// loop device that uses it was attached by this job.
		if err := detachAssociated(j.partial); err != nil {
			return errors.Join(append(errs, err)...)
		}
		j.attached = false
	}
	return errors.Join(errs...)
}

// detachAssociated detaches every loop device that uses path and confirms
// that none remain.
func detachAssociated(path string) error {
	out, err := imageRunCmd("losetup", "--associated", path)
	if err != nil {
		return fmt.Errorf("%w: could not list the loop devices for %q: %v", errDetach, path, err)
	}
	for _, line := range strings.Split(out, "\n") {
		m := loopAssocRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if err := assertUnmounted(m[1]); err != nil {
			return err
		}
		if err := detachLoop(m[1]); err != nil {
			return err
		}
	}
	return assertDetached(path)
}

// publish flushes the partial file and moves it to its final name without
// replacing an existing file.
func (j *imageJob) publish() error {
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("%w: %v", errImageFile, err)
	}
	err := j.file.Close()
	j.file = nil
	if err != nil {
		return fmt.Errorf("%w: %v", errImageFile, err)
	}
	// A hard link fails if the final name exists, unlike a rename, which
	// would replace it.
	if err := os.Link(j.partial, j.final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %q already exists", errImageFile, j.final)
		}
		return fmt.Errorf("%w: could not create %q: %v; choose a folder on a file system that supports hard links", errImageFile, j.final, err)
	}
	if err := os.Remove(j.partial); err != nil {
		console.Printf("Warning: could not remove %q: %v", j.partial, err)
	}
	syncDir(filepath.Dir(j.final))
	return nil
}

// syncDir flushes a folder entry to disk on a best effort basis.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	d.Sync()
}

// discard closes and removes the partial file after a failure. The file is
// kept while a loop device still uses it, and it is only removed if it is
// still the file that this job created.
func (j *imageJob) discard() {
	if j.file != nil {
		j.file.Close()
		j.file = nil
	}
	if j.info == nil || j.attached {
		return
	}
	cur, err := os.Lstat(j.partial)
	if err != nil || !os.SameFile(j.info, cur) {
		return
	}
	if err := os.Remove(j.partial); err != nil {
		console.Printf("Warning: could not remove %q: %v", j.partial, err)
	}
}

// recoveryHint describes how to finish cleaning up by hand.
func (j *imageJob) recoveryHint() string {
	var b strings.Builder
	b.WriteString("Cleanup did not finish. To clean up manually:\n")
	switch {
	case j.loopID != "":
		fmt.Fprintf(&b, "  sudo umount /dev/%sp1\n", j.loopID)
		fmt.Fprintf(&b, "  sudo losetup --detach /dev/%s\n", j.loopID)
	case j.attached:
		fmt.Fprintf(&b, "  losetup --associated %q\n", j.partial)
		b.WriteString("  sudo losetup --detach <each device listed above>\n")
	}
	if _, err := os.Lstat(j.partial); err == nil {
		fmt.Fprintf(&b, "  rm %q\n", j.partial)
	}
	return b.String()
}

// printSuccess explains how to write the image to a USB drive. The path is
// printed last, unquoted and on its own line, so that it is easy to find and
// copy. It is also logged so that it appears in verbose mode, where console
// messages are not shown.
func (j *imageJob) printSuccess(seed bool) {
	drive := fmt.Sprintf("a USB drive of at least %s", humanize.IBytes(j.size))
	if j.driveGB > 0 {
		drive = fmt.Sprintf("a USB drive of %dGB or larger", j.driveGB)
	}
	console.Printf("\nWrote a %s installer image.", humanize.IBytes(j.size))
	console.Printf("To create the installer from Chrome, open the Device Recovery Utility, select Settings (gear icon) > Use local image, choose the image file below, and write it to %s.", drive)
	if seed {
		console.Printf("The image contains a time-limited seed. Write it to a drive soon and delete the image file afterwards.")
	}
	console.Printf("\nImage file: %s\n", j.final)
	deck.Infof("Wrote a %s installer image file: %s", humanize.IBytes(j.size), j.final)
}
