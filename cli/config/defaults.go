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

package config

import "fmt"

// distributions configures the options for different operating system
// installers.
var (
	// distributions configures the options for different operating system
	// installers.
	distributions = map[string]distribution{
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
				"stable":  "installer_img.iso",
			},
		},
		"windowsffu": distribution{
			os:          windows,
			label:       "INSTALLER",
			name:        "windows",
			imageServer: "https://image.host.com/folder",
			confServer:  "https://config.host.com/folder",
			images: map[string]string{
				"default":  "installer_img.iso",
				"stable":   "installer_img.iso",
				"unstable": "installer_img.iso",
			},
			configs: map[string]string{
				"default":  "installer_config.yaml",
				"stable":   "installer_config.yaml",
				"unstable": "installer_config.yaml",
			},
		},
		"linux": distribution{
			os:          linux,
			name:        "linux",
			imageServer: "",
			images: map[string]string{
				"default": "installer.img.gz",
				"stable":  "installer.img.gz",
			},
		},
	}

	// ErrUSBwriteAccess contains the Error message visible to users when USB write access is forbidden.
	ErrUSBwriteAccess = fmt.Errorf("contact IT helpdesk for help")
)
