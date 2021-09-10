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

package endpoints

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/fresnel/models"
	"google.golang.org/appengine/aetest"
	"google.golang.org/appengine/user"
)

const (
	testHash = "0123456789abcdeffedcba9876543210"
)

func TestValidateSeedRequestSuccess(t *testing.T) {
	testGood := []struct {
		desc string
		u    user.User
		req  models.SeedRequest
	}{
		{
			"valid request",
			user.User{Email: "test@googleplex.com"},
			models.SeedRequest{Hash: []byte("00000000000000000000000000000000")},
		},
	}
	for _, tt := range testGood {
		ah := make(map[string]bool)
		ah[hex.EncodeToString(tt.req.Hash)] = true
		err := validateSeedRequest(&tt.u, tt.req, ah)
		if err != nil {
			t.Errorf("%s: validateSeedRequest returned: %s; expected nil", tt.desc, err)
		}
	}
}

func TestValidateSeedRequestFailure(t *testing.T) {
	testBad := []struct {
		desc string
		u    user.User
		req  models.SeedRequest
		err  string
	}{
		{
			"null request",
			user.User{},
			models.SeedRequest{Hash: []byte(nil)},
			"no username detected",
		},
		{
			"invalid hash",
			user.User{Email: "test@googleplex.com"},
			models.SeedRequest{Hash: []byte("0000000000000000000000000000000000")},
			"not in allowlist",
		},
		{
			"no user",
			user.User{},
			models.SeedRequest{Hash: []byte("00000000000000000000000000000000")},
			"no username detected",
		},
	}
	ah := make(map[string]bool)
	ah[hex.EncodeToString([]byte("00000000000000000000000000000000"))] = true
	for _, tt := range testBad {
		err := validateSeedRequest(&tt.u, tt.req, ah)
		if err == nil {
			t.Errorf("testing %s: validateSeedRequest returned nil expected err", tt.desc)
		}
		if !strings.Contains(err.Error(), tt.err) {
			t.Errorf("testing %s: expected string '%s' was not found in returned error '%s'", tt.desc, tt.err, err)
		}
	}
}

func TestUnmarshalSeedRequestSuccess(t *testing.T) {
	testGood := []struct {
		desc string
		body io.Reader
	}{
		{
			"valid request",
			bytes.NewReader([]byte(fmt.Sprintf(`{"Hash":"%s"}`, testHash))),
		},
	}

	for _, tt := range testGood {
		req := httptest.NewRequest(http.MethodPost, "/seed", tt.body)
		sr, err := unmarshalSeedRequest(req)
		if err != nil {
			t.Errorf("%s for unmarshalSeedRequest resulted in err where none expected: %s", tt.desc, err)
		}
		if bytes.Equal(sr.Hash, []byte(testHash)) {
			t.Errorf("%s failed to produce expected seed request from unmarshalSeedRequest\n  got: %s\n  want: %s", tt.desc, sr.Hash, testHash)
		}
	}
}

func TestUnmarshalSeedRequestFailure(t *testing.T) {
	testBad := []struct {
		desc string
		body io.Reader
		err  string
	}{
		{
			"null request",
			nil,
			"empty",
		},
		{
			"invalid json",
			bytes.NewReader([]byte("this should fail")),
			"unable to unmarshal JSON",
		},
		{
			"ioreader error",
			errReader(0),
			"error reading",
		},
	}
	for _, tt := range testBad {
		req := httptest.NewRequest(http.MethodPost, "/seed", tt.body)
		_, err := unmarshalSeedRequest(req)
		if err == nil {
			t.Errorf("testing %s: unmarshalSeedRequest received %s expected error containing %s", tt.desc, err, tt.err)
		} else {
			if !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: unmarshalSeedRequest got: %s want: %s", tt.desc, err, tt.err)
			}
		}
	}
}

func TestSignSeedFailure(t *testing.T) {
	seed := models.Seed{Username: "test@googleplex.com"}
	// Ensuring we don't pass an appengine context to ensure signing fails.
	ss, err := signSeedResponse(context.Background(), seed)
	if err == nil {
		t.Fatalf("signSeedResponse(%v) returned nil, want error.\n%v", seed, ss)
	}
	if !strings.Contains(err.Error(), "appengine.PublicCertificates") {
		t.Errorf(`"signSeedResponse(%v) got err: %v expected error to contain "sign"`, seed, err)
	}
}

func serveHTTPValid(t *testing.T, inst aetest.Instance) (*httptest.ResponseRecorder, *http.Request) {
	b, err := generateTestSeedRequest()
	if err != nil {
		t.Fatalf("failed to generate test seed request: %v", err)
	}

	rb := bytes.NewReader(b)
	r, err := inst.NewRequest(http.MethodPost, "/seed", rb)
	if err != nil {
		t.Fatalf("could not mock appengine request: %v", err)
	}

	u := user.User{Email: "test@googleplex.com"}
	w := httptest.NewRecorder()

	aetest.Login(&u, r)

	r.Header.Add("Content-Type", "application/json")

	return w, r
}

func generateTestSeedRequest() ([]byte, error) {
	h, err := prepTestHash()
	if err != nil {
		return []byte(nil), fmt.Errorf("could not create test hash prepTestHash returned: %v", err)
	}
	sr := models.SeedRequest{Hash: h}
	return json.Marshal(sr)
}

func generateBadTestSeedRequest() ([]byte, error) {
	h, err := hex.DecodeString("BEEF")
	if err != nil {
		return nil, fmt.Errorf("failed to generate bad test hash: %v", err)
	}
	sr := models.SeedRequest{Hash: h}
	return json.Marshal(sr)
}

func serveHTTPFailUnmarshal(t *testing.T, inst aetest.Instance) (*httptest.ResponseRecorder, *http.Request) {
	b := []byte("bad json")
	rb := bytes.NewReader(b)
	r, err := inst.NewRequest(http.MethodPost, "/seed", rb)
	if err != nil {
		t.Fatalf("could not mock appengine request: %v", err)
	}

	u := user.User{Email: "test@googleplex.com"}
	aetest.Login(&u, r)

	w := httptest.NewRecorder()
	r.Header.Add("Content-Type", "application/json")
	return w, r
}

func serveHTTPFailUser(t *testing.T, inst aetest.Instance) (*httptest.ResponseRecorder, *http.Request) {
	b, err := generateTestSeedRequest()
	if err != nil {
		t.Fatalf("failed to generate test seed request: %v", err)
	}

	rb := bytes.NewReader(b)
	r, err := inst.NewRequest(http.MethodPost, "/seed", rb)
	if err != nil {
		t.Fatalf("could not mock appengine request: %v", err)
	}

	aetest.Logout(r)

	w := httptest.NewRecorder()
	r.Header.Add("Content-Type", "application/json")
	return w, r
}

func serveHTTPFailValidate(t *testing.T, inst aetest.Instance) (*httptest.ResponseRecorder, *http.Request) {
	b, err := generateBadTestSeedRequest()
	if err != nil {
		t.Fatalf("failed to generate test seed request: %v", err)
	}

	rb := bytes.NewReader(b)
	r, err := inst.NewRequest(http.MethodPost, "/seed", rb)
	if err != nil {
		t.Fatalf("could not mock appengine request: %v", err)
	}

	u := user.User{Email: "test@googleplex.com"}
	aetest.Login(&u, r)

	w := httptest.NewRecorder()
	r.Header.Add("Content-Type", "application/json")
	return w, r
}

func serveHTTPFailSign(t *testing.T, inst aetest.Instance) (*httptest.ResponseRecorder, *http.Request) {
	// replace signSeed with a function guaranteed to return an error for this test.
	signSeed = func(context.Context, models.Seed) (models.SeedResponse, error) {
		return models.SeedResponse{}, fmt.Errorf("test failure")
	}

	return serveHTTPValid(t, inst)
}
