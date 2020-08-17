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

// Package appengine is an web-application that provides a public API
// for imaging Windows clients. It provides endpoints to permit
// pre-authorization for builds, and for machines being built to obtain
// required binaries from a GCS cloud bucket.
package main

import (
	"net/http"

	"github.com/google/fresnel/appengine/endpoints"
	"google.golang.org/appengine"
)

func main() {
	http.Handle("/sign", &endpoints.SignRequestHandler{})
	http.Handle("/seed", &endpoints.SeedRequestHandler{})

	appengine.Main()
}
