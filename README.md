# Fresnel

| Support | Travis CI | Contributing | Open Issues | License |
|----------------|------------|------|---------|-------------|
[![Google Groups - Fresnel](https://img.shields.io/badge/Support-Google%20Groups-blue)](https://groups.google.com/forum/#!forum/fresnel-discuss) | [![Travis CI Build Status](https://img.shields.io/travis/google/fresnel)](https://travis-ci.org/google/fresnel) | [![Contributing](https://img.shields.io/badge/contributions-welcome-brightgreen)](https://github.com/google/fresnel/blob/master/CONTRIBUTING.md) | [![Open Issues](https://img.shields.io/github/issues/google/fresnel)](https://github.com/google/fresnel/issues) | [![License](https://img.shields.io/badge/License-Apache%202.0-orange.svg)](https://github.com/google/fresnel/blob/master/LICENSE)

[Fresnel](https://en.wikipedia.org/wiki/Fresnel_lens) */fray-NEL/* projects Windows out into the world.

Fresnel is an infrastructure service which allows operating system images to be
retrieved and provisioned from anywhere where internet access is available. It
allows an authorized user to create sanctioned boot media that can be used to
provision a machine, and it allows the installer to obtain files needed to build
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
media. It also provides a place where the installer can obtain the files needed
to build trust prior to connecting to the business network. Once the
provisioning process gains trust, trusted connectivity can be provided by a
VPN or another solution to provides access to the remaining files needed to
complete the provisioning process.

## Documentation

See the [App Engine documentation](appengine/README.md) for information on
installing and configuring Fresnel App Engine and for information on how to
make requests for signed-urls from App Engine.

See the [CLI Documentation](cli/README.md) for information on using the Fresnel
CLI to provision your installer.

## Contact

We have a public discussion list at
[fresnel-discuss@googlegroups.com](https://groups.google.com/forum/#!forum/fresnel-discuss)


## Disclaimer

This is not an official Google product.
