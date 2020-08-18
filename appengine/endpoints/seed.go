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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/fresnel/models"
	"google.golang.org/appengine"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/user"
)

var (
	signSeed      = signSeedResponse
	supportedHash = map[int]bool{
		sha256.Size: true,
	}
)

// SeedRequestHandler implements http.Handler for signed URL requests.
type SeedRequestHandler struct{}

func (SeedRequestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	w.Header().Set("Content-Type", "application/json")

	// Seed to be used during error conditions
	errSeedResp := `{"Status":"%s","ErrorCode":%d}`

	sr, err := unmarshalSeedRequest(r)
	if err != nil {
		log.Errorf(ctx, "unmarshalSeedRequest returned error: %s", err)
		http.Error(w, fmt.Sprintf(errSeedResp, err, models.StatusJSONError), http.StatusInternalServerError)
		return
	}

	u := user.Current(ctx)
	if u == nil {
		log.Errorf(ctx, "seed requested without user information in context: #%s", ctx)
		http.Error(w, fmt.Sprintf(errSeedResp, "no user", models.StatusInvalidUser), http.StatusInternalServerError)
		return
	}

	hashCheck := os.Getenv("VERIFY_SEED_HASH")
	if hashCheck != "true" {
		log.Infof(ctx, "VERIFY_SEED_HASH is not set to true, hash validation will be logged but not enforced")
	}
	acceptedHashes, err := populateAllowlist(ctx)
	if err != nil {
		log.Errorf(ctx, "failed to populate hash allowlist: %v", err)
		if hashCheck == "true" {
			http.Error(w, fmt.Sprintf(errSeedResp, err, models.StatusSeedError), http.StatusInternalServerError)
			return
		}
	}

	if err := validateSeedRequest(u, sr, acceptedHashes); err != nil {
		log.Errorf(ctx, "validateSeedRequest(%s,%#v,%#v) returned: %v", u.String(), sr, acceptedHashes, err)
		if !strings.Contains(err.Error(), "not in allowlist") || hashCheck == "true" {
			http.Error(w, fmt.Sprintf(errSeedResp, err, models.StatusReqUnreadable), http.StatusInternalServerError)
			return
		}
	}
	log.Infof(ctx, "validated seed request from %s with hash %x", u.String(), sr.Hash)

	s := generateSeed(sr.Hash, u)
	log.Infof(ctx, "successfully generated Seed: %#v", s)

	resp, err := signSeed(ctx, s)
	if err != nil {
		log.Errorf(ctx, "signSeed returned: %v", err)
		http.Error(w, fmt.Sprintf(errSeedResp, err, models.StatusSignError), http.StatusInternalServerError)
		return
	}
	log.Infof(ctx, "successfully signed seed: %+v", resp.Seed)

	jsonResponse, err := json.Marshal(resp)
	if err != nil {
		es := fmt.Sprintf("json.Marshall(%v) returned: %v", resp, err)
		log.Errorf(ctx, es)
		http.Error(w, fmt.Sprintf(errSeedResp, err, models.StatusJSONError), http.StatusInternalServerError)
		return
	}

	if _, err = w.Write(jsonResponse); err != nil {
		log.Errorf(ctx, fmt.Sprintf("failed to write response to client: %s", err))
		return
	}

	if resp.ErrorCode == models.StatusSuccess {
		log.Infof(ctx, "successfully processed SeedRequest with response: %+v", resp)
	}
}

// generateSeed generates an object that contains the response to the media generation tool
// client request for a seed.
func generateSeed(hash []byte, u *user.User) models.Seed {
	return models.Seed{
		Issued:   time.Now(),
		Username: u.String(),
		Hash:     hash,
	}

}

// unmarshalSeedRequest parses a JSON object passed in an http request in to a models.SeedRequest object.
func unmarshalSeedRequest(r *http.Request) (models.SeedRequest, error) {
	var seedRequest models.SeedRequest

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return models.SeedRequest{},
			fmt.Errorf("error reading request body: %v", err)
	}

	if len(body) == 0 {
		return models.SeedRequest{},
			fmt.Errorf("received empty seed request")
	}

	if err := json.Unmarshal(body, &seedRequest); err != nil {
		return models.SeedRequest{},
			fmt.Errorf("unable to unmarshal JSON request: %v", err)
	}

	return seedRequest,
		nil
}

// validateSeedRequest ensures seed request is populated with a valid hash.
func validateSeedRequest(u *user.User, sr models.SeedRequest, ah map[string]bool) error {
	if len(u.String()) < 1 {
		return fmt.Errorf("no username detected: %s", u.String())
	}

	h := hex.EncodeToString(sr.Hash)
	if _, ok := ah[h]; ok {
		return nil
	}

	return fmt.Errorf("request hash %v not in allowlist: %#v", hex.EncodeToString(sr.Hash), ah)
}

// signSeed will generate a seed response from a valid seed.
func signSeedResponse(ctx context.Context, s models.Seed) (models.SeedResponse, error) {
	certs, err := appengine.PublicCertificates(ctx)
	if err != nil {
		return models.SeedResponse{}, fmt.Errorf("sign failed: appengine.PublicCertificates returned %v", err)
	}
	s.Certs = certs

	jsonSeed, err := json.Marshal(s)
	if err != nil {
		return models.SeedResponse{},
			fmt.Errorf("failed to marshal seed before signing: %v", err)
	}

	_, sig, err := appengine.SignBytes(ctx, jsonSeed)
	if err != nil {
		return models.SeedResponse{},
			fmt.Errorf("sign failed: %v", err)
	}

	// nil out hash so it's not sent to the client, the client will regenerate hash and send with sign requests.
	s.Hash = nil

	return models.SeedResponse{
			Status:    "success",
			ErrorCode: models.StatusSuccess,
			Seed:      s,
			Signature: sig,
		},
		nil
}

// populateAllowlist will return a map of hashes allowed to request a seed or signed url.
func populateAllowlist(ctx context.Context) (map[string]bool, error) {
	b := os.Getenv("BUCKET")
	if b == "" {
		return nil, errors.New("BUCKET environment variable not set")
	}

	ah, err := getAllowlist(ctx, b, "appengine_config/pe_allowlist.yaml")
	if err != nil {
		return nil, fmt.Errorf("retrieving allowlist returned error: %v", err)
	}
	return ah, nil
}
