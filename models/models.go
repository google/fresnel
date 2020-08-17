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

// Package models provides data structures for imaging requests, responses,
// and status codes.
package models

import (
	"time"

	"google.golang.org/appengine"
)

// StatusCode represents an appengine status code, and is used to communicate
// reasons for request and result rejections, as well as internal failures.
type StatusCode int

// Internal Status Messages, provided to the client as part of response messages.
const (
	StatusSuccess     StatusCode = 0
	StatusConfigError StatusCode = iota + 100
	StatusReqUnreadable
	StatusJSONError
	StatusSignError
	StatusSeedError
	StatusSeedInvalidHash
	StatusInvalidUser
)

// SignRequest models the data that a client can submit as part
// of a sign request.
type SignRequest struct {
	Seed      Seed
	Signature []byte
	Mac       []string
	Path      string
	Hash      []byte
}

// SignResponse models the response to a client sign request.
type SignResponse struct {
	Status    string
	ErrorCode StatusCode
	SignedURL string
}

// SeedRequest models the data that a client must submit as part of a Seed
// request
type SeedRequest struct {
	Hash []byte
}

// SeedResponse models the data that is passed back to the client when a seed
// request is successfully processed.
type SeedResponse struct {
	Status    string
	ErrorCode StatusCode
	Seed      Seed
	Signature []byte
}

// SeedFile models the file that is stored on disk by the bootstraper. It is
// similar to SeedResponse, but does not contain the uneccessary Status and
// ErrorCode fields, which can contain data not intended to be stored on
// disk.
type SeedFile struct {
	Seed      Seed
	Signature []byte
}

// Seed represents the data that validates proof of origin for a request. It
// is always accompanied by a signature that is used to decrypt and validate
// its contents.
type Seed struct {
	Issued   time.Time
	Username string
	Certs    []appengine.Certificate
	Hash     []byte
}
