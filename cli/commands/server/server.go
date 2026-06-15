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

// Package server provides a background service to allow for unprivileged execution.
package server

import (
	"flag"
	"github.com/google/subcommands"
)

func init() {
	subcommands.Register(&serverCmd{name: "server"}, "")
}

type serverCmd struct {
	name         string
	allowedGroup string
}

func (c *serverCmd) Name() string     { return c.name }
func (c *serverCmd) Synopsis() string { return "Run the Fresnel background service" }
func (c *serverCmd) Usage() string {
	return "server\n  Runs the background service for unprivileged execution.\n"
}
func (c *serverCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&c.allowedGroup, "allowed_group", "", "Group name allowed to access the service named pipe. Defaults to Administrators only.")
}

// WriteResponse represents a response from the service.
type WriteResponse struct {
	Error string
}
