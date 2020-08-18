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

// Package endpoints provides the functions used to receive requests
// and serve data via imaging.
package endpoints

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/fresnel/models"
	"google.golang.org/appengine"
	"google.golang.org/appengine/log"
	"cloud.google.com/go/storage"
	"gopkg.in/yaml.v2"
)

var (
	macRegEx         = "([^0-9,a-f,A-F,:])"
	bucketFileFinder = bucketFileHandle
)

// SignRequestHandler implements http.Handler for signed URL requests.
type SignRequestHandler struct{}

func (SignRequestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	errResp := `{"Status":"%s","ErrorCode":%d}`

	ctx := appengine.NewContext(r)
	w.Header().Set("Content-Type", "application/json")

	resp := signResponse(ctx, r)

	if resp.ErrorCode != models.StatusSuccess {
		w.WriteHeader(http.StatusInternalServerError)
	}

	jsonResponse, err := json.Marshal(resp)
	if err != nil {
		es := fmt.Sprintf("json.Marshall(%#v) returned: %v", resp, err)
		log.Errorf(ctx, es)
		http.Error(w, fmt.Sprintf(errResp, err, models.StatusJSONError), http.StatusInternalServerError)
		return
	}

	if _, err = w.Write(jsonResponse); err != nil {
		log.Errorf(ctx, fmt.Sprintf("failed to write response to client: %s", err))
		return
	}
	log.Infof(ctx, "successfully returned response %#v to client", resp)
	return
}

// signResponse processes a signed URL request and provides a valid response to the client.
func signResponse(ctx context.Context, r *http.Request) models.SignResponse {
	bucket := os.Getenv("BUCKET")
	if bucket == "" {
		log.Errorf(ctx, "BUCKET environment variable not set for %v", ctx)
		return models.SignResponse{Status: "Environment variable not set", ErrorCode: models.StatusConfigError}
	}

	d := os.Getenv("SIGNED_URL_DURATION")
	if d == "" {
		log.Errorf(ctx, "SIGNED_URL_DURATION environment variable not set for %v", ctx)
		return models.SignResponse{Status: "Environment variable not set", ErrorCode: models.StatusConfigError}
	}

	duration, err := time.ParseDuration(d)
	if err != nil {
		log.Errorf(ctx, "SIGNED_URL_DURATION was '%s', which is not a valid time duration.", d)
		return models.SignResponse{Status: "Environment variable not set", ErrorCode: models.StatusConfigError}
	}

	resp, req := ProcessSignRequest(ctx, r, bucket, duration)
	if resp.ErrorCode != models.StatusSuccess {
		log.Warningf(ctx, "could not process SignRequest %v", resp)
	}

	if resp.ErrorCode == models.StatusSuccess {
		log.Infof(ctx, "successfully processed SignRequest for seed issued to %#v at:%#v Response: %q", req.Seed.Username, req.Seed.Issued, resp.SignedURL)
	}
	return resp
}

// ProcessSignRequest takes a models.SignRequest that is provided by a client,
// validates and processes it. A response is always provided using models.SignResponse.
func ProcessSignRequest(ctx context.Context, r *http.Request, bucket string, duration time.Duration) (models.SignResponse, models.SignRequest) {
	req, code, err := unmarshalSignRequest(r)
	if err != nil {
		log.Errorf(ctx, "unmarshalSignRequest called with: %#v, returned error: %s", r, err)
		return models.SignResponse{
			Status:    err.Error(),
			ErrorCode: code,
		}, req
	}

	if err := validSignRequest(ctx, req); err != nil {
		return models.SignResponse{
			Status:    err.Error(),
			ErrorCode: models.StatusSignError,
		}, req
	}

	url, err := signedURL(ctx, bucket, req.Path, duration)
	if err != nil {
		return models.SignResponse{
			Status:    err.Error(),
			ErrorCode: models.StatusSignError,
		}, req
	}

	return models.SignResponse{
		Status:    "Success",
		ErrorCode: models.StatusSuccess,
		SignedURL: url,
	}, req
}

// unmarshalSignRequest takes an incoming request, returning a models.SignRequest and
// and a models.StatusCode code representing whether it was read successfully.
func unmarshalSignRequest(r *http.Request) (models.SignRequest, models.StatusCode, error) {
	var signRequest models.SignRequest
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return models.SignRequest{},
			models.StatusReqUnreadable,
			errors.New("unable to read HTTP request body")
	}

	if len(body) == 0 {
		return models.SignRequest{},
			models.StatusJSONError,
			errors.New("empty HTTP JSON request body")
	}

	if err = json.Unmarshal(body, &signRequest); err != nil {
		return models.SignRequest{},
			models.StatusJSONError,
			fmt.Errorf("unable to unmarshal JSON request, error: %v", err)
	}

	return signRequest,
		models.StatusSuccess,
		nil
}

func validSignRequest(ctx context.Context, sr models.SignRequest) error {
	for _, mac := range sr.Mac {
		m := strings.Replace(mac, ":", "", -1)
		// A valid Mac is neither shorter nor longer than 12 characters.
		if len(m) < 12 {
			return fmt.Errorf("%s is too short(%d) to be a Mac address", m, len(m))
		}
		if len(m) > 12 {
			return fmt.Errorf("%s is too long(%d) to be a Mac address", m, len(m))
		}
		// A valid Mac address can only contain hexadecimal characters and colons.
		matched, err := regexp.MatchString(macRegEx, mac)
		if err != nil {
			return fmt.Errorf("regexp.MatchString(%s) returned %v", mac, err)
		}
		if matched {
			return fmt.Errorf("%s is not a valid mac address", mac)
		}
	}

	hashCheck := os.Getenv("VERIFY_SIGN_HASH")
	if hashCheck != "true" {
		log.Infof(ctx, "VERIFY_SIGN_HASH is not set to true, hash validation will be logged but not enforced")
	}
	err := validSignHash(ctx, sr.Hash)
	if err != nil {
		log.Warningf(ctx, "failed to validate sign request hash: %v", err)
	}
	if err != nil && hashCheck == "true" {
		return fmt.Errorf("validSignHash returned %v", err)
	}

	// insert hash into seed to validate signature
	sr.Seed.Hash = sr.Hash
	if err := validSeed(ctx, sr.Seed, sr.Signature); err != nil {
		return fmt.Errorf("validSeed returned %v", err)
	}

	if len(sr.Path) < 1 {
		return errors.New("sign request path cannot be empty")
	}

	return nil
}

// validSignHash takes the current context and the hash submitted with the sign
// request and determines if the submitted hash is in a list of acceptable hashes
// which is stored in a cloud bucket.
func validSignHash(ctx context.Context, requestHash []byte) error {
	b := os.Getenv("BUCKET")
	if b == "" {
		return fmt.Errorf("BUCKET environment variable not set for %v", ctx)
	}
	acceptedHashes, err := getAllowlist(ctx, b, "appengine_config/pe_allowlist.yaml")
	if err != nil {
		return fmt.Errorf("retrieving allowlist returned error: %v", err)
	}

	log.Infof(ctx, "retrieved acceptable hashes: %#v", acceptedHashes)

	h := hex.EncodeToString(requestHash)
	if _, ok := acceptedHashes[h]; ok {
		log.Infof(ctx, "%v passed validation", h)
		return nil
	}
	return fmt.Errorf("submitted hash %v not in accepted hash list", hex.EncodeToString(requestHash))
}

// validSeed takes a seed and its signature, verifies the seed contents and
// optionally the signature. Verification attempts to use the current set
// of appengine.PublicCertificates first, and can fall back to those included
// in the seed. If the requested validation fails, an error is returned.
func validSeed(ctx context.Context, seed models.Seed, sig []byte) error {
	// Return immediately if seed verification is disabled.
	enabled := os.Getenv("VERIFY_SEED")
	if enabled != "true" {
		log.Infof(ctx, "VERIFY_SEED=%s or not set, skipping seed verification.", enabled)
		return nil
	}

	// Check that the username is present
	if len(seed.Username) < 3 {
		return fmt.Errorf("the username '%s' is invalid or empty", seed.Username)
	}

	// Check that the seed is not expired or invalid.
	validityPeriod := os.Getenv("SEED_VALIDITY_DURATION")
	if validityPeriod == "" {
		return errors.New("SEED_VALIDITY_DURATION environment variable is not present")
	}
	d, err := time.ParseDuration(validityPeriod)
	if err != nil {
		return fmt.Errorf("time.parseDuration(%s) returned %v", validityPeriod, err)
	}
	expires := seed.Issued.Add(d)
	now := time.Now()
	if seed.Issued.After(now) {
		return fmt.Errorf("seed issued in the future %s", seed.Issued)
	}
	if expires.Before(now) {
		return fmt.Errorf("seed expired on %s, current date is %s", expires, now)
	}

	// Skip signature verification if it is not enabled.
	sigCheck := os.Getenv("VERIFY_SEED_SIGNATURE")
	if sigCheck != "true" {
		log.Infof(ctx, "VERIFY_SEED_SIGNATURE=%s or not set, skipping seed signature check", sigCheck)
		return nil
	}

	if err := validSeedSignature(ctx, seed, sig); err != nil {
		return fmt.Errorf("validSeedSignature returned %v", err)
	}

	return nil
}

func validSeedSignature(ctx context.Context, seed models.Seed, sig []byte) error {
	// Check the seed signature using the App Identity.
	// https://cloud.google.com/appengine/docs/standard/go/appidentity/
	certs, err := appengine.PublicCertificates(ctx)
	if err != nil {
		return fmt.Errorf("appengine.PublicCertificates(%+v) returned %v", ctx, err)
	}

	enableFallback := os.Getenv("VERIFY_SEED_SIGNATURE_FALLBACK")
	if enableFallback == "true" {
		log.Infof(ctx, "VERIFY_SEED_SIGNATURE_FALLBACK=%s, adding certificates from seed for fallback verification", enableFallback)
		certs = append(certs, seed.Certs...)
	}

	log.Infof(ctx, "attempting signature verification using %d certs", len(certs))
	for _, cert := range certs {
		block, _ := pem.Decode(cert.Data)
		if block == nil {
			log.Infof(ctx, "pem.Decode returned an empty block for data '%s'.", cert.Data)
			continue
		}

		x509Cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			log.Infof(ctx, "x509.ParseCertificate(%s) returned %v.", block.Bytes, err)
			continue
		}

		pubkey, ok := x509Cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			log.Infof(ctx, "certificate '%v' issued by '%v' is does not contain an RSA public key.", x509Cert.Subject, x509Cert.Issuer)
			continue
		}

		jsonSeed, err := json.Marshal(seed)
		if err != nil {
			log.Warningf(ctx, "failed to marshal seed for signature verification: %v", err)
			continue
		}
		seedHash := crypto.SHA256
		h := seedHash.New()
		h.Write(jsonSeed)
		hashed := h.Sum(nil)
		if err := rsa.VerifyPKCS1v15(pubkey, seedHash, hashed, sig); err != nil {
			log.Infof(ctx, "unable to verify seed %#v with signature '%s' using certificate '%#v'", seed, sig, x509Cert.Subject)
			continue
		}

		log.Infof(ctx, "successfully verified signature using certificate '%#v'", x509Cert.Subject)
		return nil
	}

	return fmt.Errorf("unable to verify signature for seed issued on '%v' to %s", seed.Issued, seed.Username)
}

// signedURL takes a bucket name and relative file path, and returns an
// equivalent signed URL using the appengine built-in service account.
// https://cloud.google.com/appengine/docs/standard/go/appidentity/
func signedURL(ctx context.Context, bucket, file string, duration time.Duration) (string, error) {
	sa, err := appengine.ServiceAccount(ctx)
	if err != nil {
		return "", fmt.Errorf("appengine.ServiceAccount returned %v", err)
	}

	return storage.SignedURL(bucket, file, &storage.SignedURLOptions{
		GoogleAccessID: sa,
		SignBytes: func(b []byte) ([]byte, error) {
			_, sig, err := appengine.SignBytes(ctx, b)
			return sig, err
		},
		Method:  "GET",
		Expires: time.Now().Add(time.Minute * duration),
	})
}

// getAllowlist returns a map of hashes and whether they are acceptable.
func getAllowlist(ctx context.Context, b string, f string) (map[string]bool, error) {
	log.Infof(ctx, "reading acceptable hashes from cloud bucket")
	h, err := bucketFileFinder(ctx, b, f)
	if err != nil {
		return nil, fmt.Errorf("bucketFileFinder returned: %v", err)
	}

	y, err := ioutil.ReadAll(h)
	if err != nil {
		return nil, fmt.Errorf("reading allowlist contents returned: %v", err)
	}

	var wls []string
	if err := yaml.Unmarshal(y, &wls); err != nil {
		return nil, fmt.Errorf("failed parsing allowlist: %v", err)
	}

	mwl := make(map[string]bool)
	for _, e := range wls {
		mwl[strings.ToLower(e)] = true
	}
	return mwl, nil
}

func bucketFileHandle(ctx context.Context, b string, f string) (io.Reader, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create cloud storage client: %v", err)
	}
	bh := client.Bucket(b)
	fh := bh.Object(f)
	return fh.NewReader(ctx)
}
