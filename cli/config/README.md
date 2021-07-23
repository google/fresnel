# CLI Configuration

<!--* freshness: { owner: '@alexherrero' reviewed: '2020-08-17' } *-->

The Fresnel CLI obtains all information for the distributions it makes available
from [deafults.go](defaults.go).

## Distributions

Distributions defines collections of related installers. In general, this refers
to operating system families (windows, linux) but can also refer to a collection
of utility installers, such as recovery disks.

Adding a map within entry to distributions within defaults.go adds a
distribution for the cli to use.

## Distribution

A distribution defines the specific installer you wish to make available.

```
  type distribution struct {
      os          OperatingSystem // windows or linux
      name        string // Friendly name: e.g. Corp Windows.
      label       string // If set, is used to set partition labels.
      seedServer  string // If set, a seed is obtained from here.
      seedFile    string // This file is hashed when obtaing a seed.
      seedDest    string // The relative path where the seed should be written.
      imageServer string // The base image is obtained here.
      images      map[string]string
  }
```

### Behaviors with specific fields

*   **label** - Sets the data partition of the installation media is this value.
*   **seedServer** - When configured, the CLI will attempt to retrieve a seed
    from your App Engine instance. See the
    [appengine documentation](../../appengine/README.md) for more information on
    seeds.
*   **seedFile** - When configured, this file is hashed and the hash send with
    the seed request.
*   **seedDest** - The relative path on the installation media where the seed
    should be written.
*   **imageServer** - The root path to the webserver that houses installation
    media images.

### Images

Images are defined within a distribution. Think of them as a set of variants for
an image. For example, you may have a stable and unstable installer for windows.
They are represented by a map of strings with the key being the label and the
value representing the relative path of the image file under imageServer.

## Example

The following example is a valid configuration.

```
 var distributions = map[string]distribution{
 "windows": distribution{
      os:          windows,
      label:       "INSTALLER",
      name:        "windows",
        seedServer:  "https://appengine.address.com/seed",
        seedFile:    "sources/boot.wim",
        seedDest:    "seed",
        imageServer: "https://image.host.com/folder",
        images: map[string]string{
      "default": "installer_img.iso",
        },
    },
    "linux": distribution{
      os:          linux,
      name:        "linux",
      imageServer: "",
        images: map[string]string{
          "default": "installer.img.gz",
      },
     },
 }
```
