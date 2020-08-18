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
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/fresnel/models"
	"google.golang.org/appengine/aetest"
	"google.golang.org/appengine"
	"github.com/google/go-cmp/cmp"
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

func TestSignRequestHandler(t *testing.T) {
	bucketFileFinder = fakeGoodBucketFile
	goodReq, err := prepSignTestRequest()
	if err != nil {
		t.Errorf("prepping test failed: %v", err)
	}
	type resp struct {
		error string
		code  models.StatusCode
	}

	tests := []struct {
		desc   string
		env    map[string]string
		in     models.SignRequest
		out    resp
		status int
	}{
		{
			desc: "valid request",
			env: map[string]string{
				"BUCKET":                 bucket,
				"SIGNED_URL_DURATION":    "5m",
				"VERIFY_SEED":            "true",
				"SEED_VALIDITY_DURATION": "720h",
			},
			in:     goodReq,
			out:    resp{error: "", code: 0},
			status: http.StatusOK,
		},
		{
			desc: "missing bucket",
			env: map[string]string{
				"BUCKET":                 "",
				"SIGNED_URL_DURATION":    "5m",
				"VERIFY_SEED":            "true",
				"SEED_VALIDITY_DURATION": "720h",
			},
			in:     goodReq,
			out:    resp{error: "variable not set", code: models.StatusConfigError},
			status: http.StatusInternalServerError,
		},
		{
			desc: "missing SIGNED_URL_DURATION",
			env: map[string]string{
				"BUCKET":                 bucket,
				"SIGNED_URL_DURATION":    "",
				"VERIFY_SEED":            "true",
				"SEED_VALIDITY_DURATION": "720h",
			},
			in:     goodReq,
			out:    resp{error: "variable not set", code: models.StatusConfigError},
			status: http.StatusInternalServerError,
		},
		{
			desc: "malformed SIGNED_URL_DURATION",
			env: map[string]string{
				"BUCKET":                 bucket,
				"SIGNED_URL_DURATION":    "5z",
				"VERIFY_SEED":            "true",
				"SEED_VALIDITY_DURATION": "720h",
			},
			in:     goodReq,
			out:    resp{error: "variable not set", code: models.StatusConfigError},
			status: http.StatusInternalServerError,
		},
		{
			desc: "bogus seed with verification on",
			env: map[string]string{
				"BUCKET":                 bucket,
				"SIGNED_URL_DURATION":    "5m",
				"VERIFY_SEED":            "true",
				"SEED_VALIDITY_DURATION": "720h",
			},
			in: models.SignRequest{
				Seed: bogusSeed,
				Mac:  []string{"123456789ABC", "12:34:56:78:9A:BC"},
				Path: "dummy/folder/file.txt",
				Hash: goodReq.Hash,
			},
			out:    resp{error: "seed expired on", code: models.StatusSignError},
			status: http.StatusInternalServerError,
		},
		{
			desc: "bogus seed with verification off",
			env: map[string]string{
				"BUCKET":                 bucket,
				"SIGNED_URL_DURATION":    "5m",
				"VERIFY_SEED":            "false",
				"SEED_VALIDITY_DURATION": "720h",
			},
			in: models.SignRequest{
				Seed: bogusSeed,
				Mac:  []string{"123456789ABC", "12:34:56:78:9A:BC"},
				Path: "dummy/folder/file.txt",
				Hash: goodReq.Hash,
			},
			out:    resp{error: "", code: 0},
			status: http.StatusOK,
		},
	}

	inst, err := aeInstance()
	if err != nil {
		t.Fatalf("aeInstance() returned %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {

			cleanEnvVariables, err := prepEnvVariables(tt.env)
			defer cleanEnvVariables()
			if err != nil {
				t.Fatalf("failed to prep test environment variables: %v", err)
			}

			seedResp, err := signTestSeed(inst, tt.in.Seed)
			if err != nil {
				t.Fatalf("%s failed to prep signed test seed: %v", tt.desc, err)
			}
			tt.in.Signature = seedResp.Signature

			jsonRequest, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("%s, json.Marshal(%v) returned %v", tt.desc, tt.in, err)
				return
			}
			req, err := newRequest(inst, "POST", "/sign", bytes.NewReader(jsonRequest))
			if err != nil {
				t.Errorf("%s, newRequest returned %v while processing %v", tt.desc, err, jsonRequest)
				return
			}

			rr := httptest.NewRecorder()
			handler := &SignRequestHandler{}
			handler.ServeHTTP(rr, req)
			raw, err := ioutil.ReadAll(rr.Body)
			if err != nil {
				t.Errorf("%s, ioutil.ReadAll(%v) of response body returned %v", tt.desc, rr.Body, err)
			}
			if rr.Code != tt.status {
				t.Errorf("test:%s with request %#v; got status: %v (body: %v), want status: %v", tt.desc, tt.in, string(raw), rr.Code, tt.status)
			}

			var signResp models.SignResponse
			if err := json.Unmarshal(raw, &signResp); err != nil {
				t.Errorf("%s unable to unmarshal sign response:%v, error was: %v", tt.desc, string(raw), err)
			}

			if !strings.Contains(signResp.Status, tt.out.error) || signResp.ErrorCode != tt.out.code {
				t.Errorf("test %s: got response body: %s with statuscode: %d, want body to contain: %s with code: %d",
					tt.desc, raw, rr.Code, tt.out.error, tt.out.code)
			}
		})
	}
	bucketFileFinder = bucketFileHandle
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

func TestProcessSignRequest(t *testing.T) {
	goodReq, err := prepSignTestRequest()
	if err != nil {
		t.Fatalf("failed to prep good sign test request: %v", err)
	}

	tests := []struct {
		desc     string
		bucket   string
		duration time.Duration
		in       models.SignRequest
		out      models.SignResponse
	}{
		{
			"valid request",
			"test",
			5 * time.Minute,
			goodReq,
			models.SignResponse{ErrorCode: models.StatusSuccess, Status: "Success"},
		},
	}

	inst, err := aeInstance()
	if err != nil {
		t.Fatalf("aeInstance() returned %v", err)
	}
	defer inst.Close()

	for _, tt := range tests {

		jsonRequest, err := json.Marshal(tt.in)
		if err != nil {
			t.Fatalf("%s, json.Marshal(%v) returned %v", tt.desc, tt.in, err)
			continue
		}
		httpReq, err := newRequest(inst, "POST", "/sign", bytes.NewReader(jsonRequest))
		if err != nil {
			t.Fatalf("%s, newRequest returned %v while processing %v", tt.desc, err, jsonRequest)
			continue
		}

		resp, sreq := ProcessSignRequest(appengine.NewContext(httpReq), httpReq, tt.bucket, tt.duration)

		if resp.ErrorCode != tt.out.ErrorCode {
			t.Errorf("%s; got %d %v, want %d %v",
				tt.desc, resp.ErrorCode, resp.Status, tt.out.ErrorCode, tt.out.Status)
		}
		if !strings.Contains(resp.Status, tt.out.Status) {
			t.Errorf("%s; got %d %v, want %d %v",
				tt.desc, resp.ErrorCode, resp.Status, tt.out.ErrorCode, tt.out.Status)
		}

		if !cmp.Equal(tt.in, sreq) {
			t.Errorf("%s: ProcessSignRequest parsed request as: %#v, wanted: %#v", tt.desc, sreq, tt.in)
		}
	}
}

func TestSignRequest(t *testing.T) {
	bucketFileFinder = fakeGoodBucketFile
	goodReq, err := prepSignTestRequest()
	if err != nil {
		t.Fatalf("failed to prep good sign test request: %v", err)
	}

	tests := []struct {
		desc  string
		in    models.SignRequest
		regex string
		out   error
	}{
		{
			"valid request",
			goodReq,
			macRegEx,
			nil,
		},
		{
			"bad regex",
			goodReq,
			"[",
			errors.New("regexp.MatchString"),
		},
		{
			"mac address too long",
			models.SignRequest{Seed: goodSeed, Mac: []string{"123456789ABCLONG"}, Path: "dummy/folder/file.txt"},
			macRegEx,
			errors.New("is too long"),
		},
		{
			"mac address too short",
			models.SignRequest{Seed: goodSeed, Mac: []string{"123456ABC"}, Path: "dummy/folder/file.txt"},
			macRegEx,
			errors.New("is too short"),
		},
		{
			"mac contains invalid characters",
			models.SignRequest{Seed: goodSeed, Mac: []string{"12345678ZZZZ"}, Path: "dummy/folder/file.txt"},
			macRegEx,
			errors.New("not a valid mac"),
		},
		{
			"mac formatted incorrectly",
			models.SignRequest{Seed: goodSeed, Mac: []string{"123.567.89AB"}, Path: "dummy/folder/file.txt"},
			macRegEx,
			errors.New("not a valid mac"),
		},
		{
			"empty path",
			models.SignRequest{Seed: goodSeed, Mac: []string{"123456789ABC"}, Hash: goodReq.Hash, Path: ""},
			macRegEx,
			errors.New("cannot be empty"),
		},
		{
			"invalid hash length",
			models.SignRequest{
				Seed: goodSeed,
				Mac:  []string{"123456789ABC", "12:34:56:78:9A:BC"},
				Path: "dummy/folder/file.txt",
				Hash: []byte("123456789123456789123456789"),
			},
			macRegEx,
			errors.New("not in accepted hash list"),
		},
	}

	// Create the necessary AppEngine context for logging.
	cleanup, err := prepEnvVariables(map[string]string{"VERIFY_SEED": "true", "VERIFY_SIGN_HASH": "true", "BUCKET": "test", "SEED_VALIDITY_DURATION": "180h"})
	if err != nil {
		t.Fatalf("failed to prep test environment variables: %v", err)
	}
	defer cleanup()
	inst, err := aeInstance()
	if err != nil {
		t.Fatalf("aeInstance() returned %v", err)
	}
	r, err := newRequest(inst, "POST", "/sign", bytes.NewReader([]byte("test")))
	if err != nil {
		t.Fatalf("newRequest returned %v", err)
	}
	ctx := appengine.NewContext(r)

	for _, tt := range tests {
		macRegEx = tt.regex

		err := validSignRequest(ctx, tt.in)
		if err == tt.out {
			continue
		}

		if err == nil && tt.out != nil {
			t.Errorf("validSignRequest returned nil want error with %v", tt.out)
			continue
		}

		if err != nil && tt.out == nil {
			t.Errorf("validSignRequest returned %v want nil", err)
			continue
		}

		if !strings.Contains(err.Error(), tt.out.Error()) {
			t.Errorf("%s; got %v, want %v",
				tt.desc, err, tt.out)
		}
	}

}

func TestValidSeed(t *testing.T) {
	inst, err := aeInstance()
	if err != nil {
		t.Fatalf("aeInstance() returned %v", err)
	}
	r, err := newRequest(inst, "POST", "/sign", bytes.NewReader([]byte("test")))
	if err != nil {
		t.Fatalf("newRequest returned %v", err)
	}
	ctx := appengine.NewContext(r)

	goodResponse, err := signSeedResponse(ctx, goodSeed)
	if err != nil {
		t.Fatalf("signSeedResponse(%v) returned %v", goodSeed, err)
	}

	bogusResponse, err := signSeedResponse(ctx, bogusSeed)
	if err != nil {
		t.Fatalf("signSeedResponse(%v) returned %v", bogusSeed, err)
	}

	tests := []struct {
		desc string
		env  map[string]string
		in   models.Seed
		sig  []byte
		out  error
	}{
		{
			desc: "valid signed seed",
			env: map[string]string{"VERIFY_SEED": "true",
				"SEED_VALIDITY_DURATION": "300m",
				"VERIFY_SEED_SIGNATURE":  "true"},
			in:  goodResponse.Seed,
			sig: goodResponse.Signature,
			out: nil,
		},
		{
			desc: "valid signed seed - seed verify off",
			env: map[string]string{"VERIFY_SEED": "false",
				"SEED_VALIDITY_DURATION": "300m",
				"VERIFY_SEED_SIGNATURE":  "true"},
			in:  goodResponse.Seed,
			sig: goodResponse.Signature,
			out: nil,
		},
		{
			desc: "recently expired seed - VERIFY_SEED on",
			env: map[string]string{"VERIFY_SEED": "true",
				"SEED_VALIDITY_DURATION": "300m",
				"VERIFY_SEED_SIGNATURE":  "true"},
			in:  expiredSeed,
			sig: []byte("0"),
			out: errors.New("seed expired"),
		},
		{
			desc: "recently expired seed - VERIFY_SEED off",
			env:  map[string]string{"VERIFY_SEED": "false"},
			in:   expiredSeed,
			sig:  []byte("0"),
			out:  nil,
		},
		{
			desc: "long expired seed",
			env: map[string]string{"VERIFY_SEED": "true",
				"SEED_VALIDITY_DURATION": "300m",
				"VERIFY_SEED_SIGNATURE":  "true"},
			in:  bogusSeed,
			sig: []byte("0"),
			out: errors.New("seed expired"),
		},
		{
			desc: "empty seed",
			env: map[string]string{"VERIFY_SEED": "true",
				"SEED_VALIDITY_DURATION": "300m",
				"VERIFY_SEED_SIGNATURE":  "true"},
			in:  models.Seed{},
			sig: []byte("0"),
			out: errors.New("invalid or empty"),
		},
		{
			desc: "invalid signature - VERIFY_SEED_SIGNATURE on",
			env: map[string]string{"VERIFY_SEED": "true",
				"SEED_VALIDITY_DURATION": "300m",
				"VERIFY_SEED_SIGNATURE":  "true"},
			in:  goodSeed,
			sig: bogusResponse.Signature,
			out: errors.New("unable to verify"),
		},
		{
			desc: "invalid signature -  VERIFY_SEED_SIGNATURE off",
			env: map[string]string{"VERIFY_SEED": "true",
				"SEED_VALIDITY_DURATION": "300m",
				"VERIFY_SEED_SIGNATURE":  "false"},
			in:  goodSeed,
			sig: bogusResponse.Signature,
			out: nil,
		},
	}

	for _, tt := range tests {
		cleanup, err := prepEnvVariables(tt.env)
		if err != nil {
			t.Errorf("failed to prep test environment variables: %v", err)
		}

		err = validSeed(ctx, tt.in, tt.sig)
		if err := cleanup(); err != nil {
			t.Errorf("failed to cleanup env variables: %v", err)
		}
		if err == tt.out {
			continue
		}
		if (err == nil) && (tt.out != nil) {
			t.Errorf("test %s calling validSeed: got nil want %v", tt.desc, tt.out)
			continue
		}
		if (err != nil) && (tt.out == nil) {
			t.Errorf("%s: got %v want nil", tt.desc, err)
			continue
		}

		if !strings.Contains(err.Error(), tt.out.Error()) {
			t.Errorf("%s; got %v, want %v",
				tt.desc, err, tt.out)
		}
	}
}

func TestGetAllowlist(t *testing.T) {
	ctx, cleanup, err := aetest.NewContext()
	if err != nil {
		t.Fatalf("could not create test instance NewContext returned: %v", err)
	}
	defer cleanup()
	handle := bucketFileFinder
	tests := []struct {
		desc string
		bf   func(context.Context, string, string) (io.Reader, error)
		om   map[string]bool
		err  error
	}{
		{
			desc: "good file",
			bf:   fakeGoodBucketFile,
			om: map[string]bool{
				"314aaa98adcbd86339fb4eece6050b8ae2d38ff8ebb416e231bb7724c99b830d": true,
			},
			err: nil,
		},
		{
			desc: "empty file",
			bf: func(ctx context.Context, b string, f string) (io.Reader, error) {
				return bytes.NewReader([]byte("")), nil
			},
			om:  map[string]bool{},
			err: nil,
		},
		{
			desc: "bad reader",
			bf:   func(ctx context.Context, b string, f string) (io.Reader, error) { return errReader(0), nil },
			om:   nil,
			err:  errors.New("reading allowlist contents returned"),
		},
		{
			desc: "bad yaml",
			bf: func(ctx context.Context, b string, f string) (io.Reader, error) {
				return bytes.NewReader([]byte(`\\bad\n \\\/\a\490a7 yaml`)), nil
			},
			om:  nil,
			err: errors.New("failed parsing allowlist"),
		},
		{
			desc: "bad file",
			bf: func(ctx context.Context, b string, f string) (io.Reader, error) {
				return nil, errors.New("broken file")
			},
			om:  nil,
			err: errors.New("bucketFileFinder returned"),
		},
	}
	for _, tt := range tests {
		bucketFileFinder = tt.bf
		m, err := getAllowlist(ctx, "bucket", "file")
		if err != nil && tt.err != nil {
			if !strings.Contains(err.Error(), tt.err.Error()) {
				t.Errorf("%s, getAllowlist got err: %v, want %v", tt.desc, err, tt.err)
			}
		} else if err != nil && tt.err == nil {
			t.Errorf("%s, getAllowlist got err %v, expected nil", tt.desc, err)
		} else if err == nil && tt.err != nil {
			t.Errorf("%s, getAllowlist got nil err, expected %v", tt.desc, tt.err)
		}
		if !cmp.Equal(tt.om, m) {
			t.Errorf("%s, getAllowlist got map: %#v want %#v", tt.desc, m, tt.om)
		}
		bucketFileFinder = handle
	}
}

func TestValidSignHash(t *testing.T) {
	bucketFileFinder = fakeGoodBucketFile

	inst, err := aeInstance()
	if err != nil {
		t.Fatalf("aeInstance() returned %v", err)
	}
	defer inst.Close()

	r, err := newRequest(inst, "POST", "/sign", bytes.NewReader([]byte("test")))
	if err != nil {
		t.Errorf("failed to setup TestGoodValidSignHash, newRequest returned: %v", err)
	}
	ctx := appengine.NewContext(r)

	tests := []struct {
		desc string
		env  map[string]string
		hash string
		out  string
	}{
		{
			desc: "good hash",
			env:  map[string]string{"BUCKET": "test"},
			hash: "314aaa98adcbd86339fb4eece6050b8ae2d38ff8ebb416e231bb7724c99b830d",
			out:  "",
		},
		{
			desc: "bad hash",
			env:  map[string]string{"BUCKET": "test"},
			hash: "746869735f73686f756c645f6661696c",
			out:  "not in accepted hash list",
		},
		{
			desc: "bad bucket",
			env:  map[string]string{},
			hash: "746869735f73686f756c645f6661696c",
			out:  "BUCKET environment variable not set for",
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			cleanup, err := prepEnvVariables(tt.env)
			defer cleanup()
			if err != nil {
				t.Fatalf("failed to prep test environment variables: %v", err)
			}
			rh, err := hex.DecodeString(tt.hash)
			if err != nil {
				t.Fatalf("failed to setup TestGoodValidSignHash: %v", err)
			}

			err = validSignHash(ctx, rh)

			if tt.out != "" && err == nil {
				t.Errorf("validSignHash returned: nil want error containing %s", tt.out)
				return
			}
			if tt.out == "" && err != nil {
				t.Errorf("validSignHash returned %#v want nil", err)
				return
			}
			if tt.out == "" && err == nil {
				return
			}
			if !strings.Contains(err.Error(), tt.out) {
				t.Errorf("validSignHash returned: %#v want error containing %s", err, tt.out)
			}
		})
	}
	bucketFileFinder = bucketFileHandle
}
