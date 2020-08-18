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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/fresnel/models"
	"google.golang.org/appengine/aetest"
	"google.golang.org/appengine"
)

const bucket = "test"

var (
	goodSeed = models.Seed{
		Issued:   time.Now(),
		Username: "test",
	}
	expiredSeed = models.Seed{
		Issued:   time.Now().Add(time.Hour * -169),
		Username: "test",
	}
	bogusSeed = models.Seed{Username: "bogus"}

	// Invalid JSON that cannot be unmarshalled correctly.
	badJSON = []byte(`{"name":bogus?}`)
)

func fakeGoodBucketFile(ctx context.Context, b, f string) (io.Reader, error) {
	return bytes.NewReader([]byte("- 314aaa98adcbd86339fb4eece6050b8ae2d38ff8ebb416e231bb7724c99b830d")), nil
}

// prepEnvVariables takes a map of variables and their values and sets the environment appropriately; it returns a cleanup function that unsets any values set during the call.
func prepEnvVariables(envVars map[string]string) (func() error, error) {
	for key, value := range envVars {
		err := os.Setenv(key, value)
		if err != nil {
			return func() error { return nil }, fmt.Errorf("could not set env variable %v, got err %v", key, err)
		}
	}
	return func() error {
		for key := range envVars {
			err := os.Unsetenv(key)
			if err != nil {
				return fmt.Errorf("failed to cleanup environment variable %s, got: %v", key, err)
			}
		}
		return nil
	}, nil
}

// prepTestHash returns an accepted hash as a []byte or an error.
func prepTestHash() ([]byte, error) {
	return hex.DecodeString("314aaa98adcbd86339fb4eece6050b8ae2d38ff8ebb416e231bb7724c99b830d")
}

// prepTestSignRequest returns a valid sign request and mocks out the allowed hash file
func prepSignTestRequest() (models.SignRequest, error) {
	h, err := prepTestHash()
	if err != nil {
		return models.SignRequest{}, fmt.Errorf("could not create test hash prepTestHash returned: %v", err)
	}

	return models.SignRequest{
		Seed: goodSeed,
		Mac:  []string{"123456789ABC", "12:34:56:78:9A:BC"},
		Path: "dummy/folder/file.txt",
		Hash: h,
	}, nil
}

func aeInstance() (aetest.Instance, error) {
	return aetest.NewInstance(nil)
}

func signTestSeed(inst aetest.Instance, seed models.Seed) (models.SeedResponse, error) {
	r, err := inst.NewRequest("POST", "/seed", bytes.NewReader([]byte("")))
	if err != nil {
		return models.SeedResponse{}, fmt.Errorf("failed to generate test request: %v", err)
	}
	return signSeedResponse(appengine.NewContext(r), seed)
}

// newRequest takes an active test appengine instance, and returns a request associated
// with the test instance, with the parameters specified. An existing ae test instance
// is used to conserve resources during testing.
func newRequest(inst aetest.Instance, method, url string, body io.Reader) (*http.Request, error) {
	r, err := inst.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("NewRequest(%s, %s) returned %v", method, url, err)
	}
	return r, nil
}

// errReader is an io.Reader that always returns an error when you
// attempt to read from it.
type errReader int

func (errReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("failure")
}

func TestUnmarshalSignRequest(t *testing.T) {
	goodReq, err := prepSignTestRequest()
	if err != nil {
		t.Fatalf("failed to prep good sign test request: %v", err)
	}

	good, err := json.Marshal(goodReq)
	if err != nil {
		t.Fatalf("setup, json.Marshal(%v) returned %v", goodReq, err)
	}

	type result struct {
		statusCode models.StatusCode
		err        error
	}

	tests := []struct {
		desc string
		in   io.Reader
		out  result
	}{
		{
			"valid http request",
			bytes.NewReader(good),
			result{
				statusCode: models.StatusSuccess,
				err:        nil,
			},
		},
		{
			"unreadable http request body",
			errReader(0),
			result{
				statusCode: models.StatusReqUnreadable,
				err:        errors.New("unable to read"),
			},
		},
		{
			"empty request body",
			nil,
			result{
				statusCode: models.StatusJSONError,
				err:        errors.New("empty"),
			},
		},
		{
			"unable to unmarshal json",
			bytes.NewReader(badJSON),
			result{
				statusCode: models.StatusJSONError,
				err:        errors.New("unable to unmarshal"),
			},
		},
	}

	for _, tt := range tests {
		t.Logf("Running '%s'; expecting %d %v", tt.desc, tt.out.statusCode, tt.out.err)

		r := httptest.NewRequest(http.MethodPost, "/sign", tt.in)
		_, got, err := unmarshalSignRequest(r)
		if got != tt.out.statusCode {
			t.Errorf("%s; got %d %v, want %d %v",
				tt.desc, got, err, tt.out.statusCode, tt.out.err)
		}
		if err == tt.out.err {
			continue
		}
		if !strings.Contains(err.Error(), tt.out.err.Error()) {
			t.Errorf("%s; got %v %d, want %v %d",
				tt.desc, err, got, tt.out.err, tt.out.statusCode)
		}
	}
}
