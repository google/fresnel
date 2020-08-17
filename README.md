# Fresnel

Fresnel is an infrastructure service which allows operating system images to be
retrieved and provisioned from anywhere where internet access is available. It
allows an authorized user to create sanctioned boot media that can be used to
provision the system, and it allows the system to obtain files needed to build
trust.

## Overview

In a traditional deployment scenario, a device must be on-premises at a
business location to be provisioned with a business appropriate operating system
image. The imaging platform generally relies on PXE boot to permit the client
system to begin the provisioning process.

The process introduces dependencies on the local network:

*   PXE must be locally available in order to obtain a sanctioned boot image.
*   The sanctioned boot image must come from a trustworthy source.
*   The boot image must be able to obtain files/resources.
*   The local network is considered trusted and is used to provide all of the
    above.

A basic solution to this problem is to provide all needed files and media on
pre-created boot media, but this introduces limitations. The boot media often
quickly becomes stale and it is undesirable to store any type of secret on
such media.

Fresnel addresses these limitations by providing an intermediary broker for the
bootstrap and provisioning process. The Fresnel infrastructure provides a place
where an authorized user can obtain and generate up-to-date sanctioned boot
media. It also provides a place where the provisioning process can obtain the
files needed to build trust prior to connecting to the business network. Once
the provisioning process gains trust, trusted connectivity can be provided by a
VPN or another solution to provides access to the remaining files needed to
complete the provisioning process.

## Documentation

See the [Project Documentation](doc/index.md) for more information.

## Disclaimer

This is not an official Google product.
