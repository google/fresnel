# Fresnel CLI

<!--* freshness: { owner: 'mitd' reviewed: '2026-06-14' } *-->

The Fresnel CLI runs on a trusted machine that is authorized to build boot media
for an operating system installer. It's primary purpose is to prepare storage
media (USB or Removable Disk) that will install an operating system.

## Getting Started

Pre-compiled binaries are available as
[release assets](https://github.com/google/fresnel/releases).

Building Fresnel CLI manually:

1. Clone the repository
1. Install any missing imports with `go get -u`
1. Run `go build C:\Path\to\fresnel\src\cli`

## Subcommands

Subcommands are required in order to operate the CLI. A list of available
subcommands is available by using the help subcommand.

```
cli.exe help
```

A list of command line flags is available for each sub-command by calling help
with that subcommand as the parameter.

```
cli.exe help write
```

## Available Subcommands

Commonly used sub-commands are listed here. A full list is available through the
help subcommand.

### List

The list sub-command outputs the storage devices that are suitable for
provisioning your operating system installer. See parameters for a list of the
defaults used when determining what a suitable device is.

__**Usage**__

```
cli.exe list
```

### Server (Windows Only)

The server subcommand runs the Fresnel background service. This service sets up
a named pipe that allows unprivileged users to provision flash drives by
handling the privileged operations (such as raw physical disk writes) on their
behalf. The service must be executed natively as an Administrator or as the
`SYSTEM` account. Unprivileged instances of the Fresnel CLI will automatically
detect and connect to this background service to securely execute operations
like `write`. An allowed group must be passed in (local or domain) for this to
function. Refer to the --allowed_group flag information below.

#### Common Flags

**--show_fixed [bool]**

Default = [False]

Includes fixed disks when searching for suitable devices.

__**Example**__

```
cli.exe list --show_fixed
```

**--minimum**

[int] Default = 2 GB

The minimum size (in Gigabytes) of storage to search for.

__**Example**__

```
cli.exe list --minimum=8
```

**--maximum**

[int] Default = 0 (no maximum)

The maximum size (in Gigabytes) of storage to search for.

__**Example**__

```
cli.exe list --maximum=64
```

**--allowed_group [string]** Default =
[None](Restricted to `SYSTEM` and `Administrators`) The local group name allowed
to access the service's named pipe. Providing a group name grants members of
that group `Generic Read/Write` (GRGW) permissions to the named pipe, enabling
unprivileged users within that group to securely use the background service for
provisioning.

**Example**

```
cli.exe server --allowed_group="Users"
```

### Write

The write subcommand writes an operating system installer to storage media. The
list of available operating system installers is configured by modifying the
config package and its [deafults.go](config/defaults.go) file.

__**Usage Examples**__

```
cli.exe write -distro=windows -track=stable 1

cli write -distro=linux -track=stable sda
```

#### Common Flags

**--distro [string]**

Default = [None]

The distribution you wish to provision onto the selected media. The options for
this value are configured by adding an entry in the map for distributions in
[defaults.go](config/defaults.go). A distribution is generally defined as the
operating system you wish to install (e.g. windows or linux). It can represent
any collection of related images that you wish to make avaialble.

**--track [string]**

Default = [None]

The track indicates the specific installer within a distribution to provision
onto the selected media. For example, you may have a stable, testing and
unstable versions of Windows that you wish to make available.

__**Examples**__

```
cli.exe write --distro=windows -track=stable 1

cli write --distro=linux -track=unstable sda
```

## Important Behaviors

Specific behaviors are automatically triggered by configuring fields for your
distribution within [config.go](config/defaults.go). For example, seeds are
automatically retrieved when the write command is running if a seedServer and
seedFile is added to the distribution configuration. For more information on
these, see the documentation for [config](config/README.md).

### Unprivileged Execution (Windows)

Writing raw data to physical disks on Windows traditionally requires
administrative privileges. Fresnel supports unprivileged execution by utilizing
the `server` subcommand. When the Fresnel background service is running, an
unprivileged user can execute the `write` subcommand. The CLI will automatically
connect to the background service's named pipe to proxy the privileged
provisioning steps, while handling file downloads and cache access under the
user's own identity context.
