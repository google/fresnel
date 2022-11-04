# Fresnel App Engine

<!--* freshness: { owner: '@alexherrero' reviewed: '2020-08-17' } *-->

Fresnel App Engine runs in your Google Cloud Platform (GCP) project. It serves
as the intermediary between a client and the resources needed to build trust for
your OS installer.

## Project Selection

Fresnel can run in your existing GCP organization, either in its own project, or
within a project you already run.

1.  Go to https://console.cloud.google.com
1.  Select your project, or create a new one if necessary.

## Installation

1.  Prepare your project to host Fresnel App Engine by following
    [these instructions](https://cloud.google.com/appengine/docs/standard/go/console).
1.  Prepare your app.yaml and pe_allowlist.yaml files.
1.  Test and deploy the application, including these files using
    [gcloud deploy](https://cloud.google.com/appengine/docs/standard/go/testing-and-deploying-your-app#deploying_your_application).
1.  Note the address of your AppEngine instance, and update your CLI
    configuration with this address. The CLI will use this address to obtain
    seeds during provisioning.

## Endpoints

### /seed

Used by the Fresnel CLI to obtain seeds when the distribution is configured to
request them. Seeds are signed using the
[AppEngine Identity API](https://cloud.google.com/appengine/docs/standard/go111/appidentity#asserting_identity_to_third-party_services),
and the CLI asserts that seeds come from the expected App Engine instance.

### /sign

Sign is available for use with your OS installer. It fulfills requests for a
[signed url](https://cloud.google.com/storage/docs/access-control/signed-urls)
representing a resource in a google cloud bucket, typically present in the same
cloud project as Fresnel App Engine. See below for additional information on how
to use the cloud bucket.

If you wish to allow an OS Installer to retrieve a file using the /sign endpoint
do the following:

1.  Retrieve a seed beforehand using the /seed endpoint.
1.  Co-locate the seed with your installation image.
1.  When your installer requires a resource from the cloud bucket, send a JSON
    encoded request to the /sign endpoint that includes your seed.
1.  The /sign endpoint will respond with a signed-url that you can use to
    retrieve the resource.
1.  Continue provisioning.

This process can be repeated for as many files/resources as are required by your
installer.

### /sign request format

Installers making requests for signed-url's should submit those requests to the
/sign endpoint using the following structure. See the models package in this
repository for additional information.

```
type SignRequest struct {
    Seed      Seed
    Signature []byte
    Mac       []string
    Path      string
    Hash      []byte
}
```

## app.yaml

Your application should be deployed using an app.yaml configured for your
project. See the [examples folder](examples/default.yaml) for a starter
configuration.

### Env Variables

These are required in order to configure your App Engine instance for your
project and use case. They are declared in your app.yaml. See the
[example](examples/default.yaml) for more information.

*   BUCKET [string]: The identifier for the cloud bucket you want to use with
    the /sign endpoint.
*   SIGNED_URL_DURATION [string]: Signed URLs provided by /sign will expire
    after this duration.
*   SEED_VALIDITY_DURATION [string]: Seeds will be considered stale and not be
    accepted by the /sign endpoint after this duration.
*   VERIFY_SEED [string]: 'true' or 'false' determines if seeds are checked when
    making requests to /sign.
*   VERIFY_SEED_SIGNATURE [string]: 'true' or 'false' determines if seed
    signatures are checked when making requests to /sign.
*   VERIFY_SEED_SIGNATURE_FALLBACK [string]: 'true' or 'false' if /sign requests
    provide their own certificate chain, allow these to be used to validate the
    identity of signer.
*   VERIFY_SEED_HASH [string]: 'true' or 'false' when making a request to /seed,
    the hash is checked against pe_allowlist.yaml to see if it is permitted.
*   VERIFY_SIGN_HASH [string]: 'true' or 'false' a seed hash is verified
    cryptographically on requests to /sign and pe_allowlist.yaml is checked for
    the presence of that hash.

## Allowlist

The pe_allowlist.yaml file lists the hashes that may be signed when requesting
seeds. It is also checked when requests are made to /sign to determine if the
seed making the request was generated using an acceptable hash.

pe_allowlist.yaml must be stored in your cloud bucket in the a folder named
'appengine_config'.

See [pe_allowlist.yaml](examples/pe_allowlist.yaml) in the examples folder for
more information.

## Cloud Bucket

Configure a cloud bucket where your pe_allowlist and any resources you wish to
use with the /sign endpoint can be stored. Your cloud bucket must allow at least
read access for the project where you are running your App Engine project.
