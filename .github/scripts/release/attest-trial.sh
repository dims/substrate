#!/usr/bin/env bash
# Trial checks for the release workflow, run in the images job after the
# attest step against the in-job registry. Not for the PR.
#
#   VERSION=<tag> KO_DOCKER_REPO=<registry path> GITHUB_REPOSITORY=<owner/repo> \
#     GH_TOKEN=<token> .github/scripts/release/attest-trial.sh
set -o errexit -o nounset -o pipefail

signer="${GITHUB_REPOSITORY}/.github/workflows/release.yaml"

echo "== digests file"
cat bin/release-images.txt
[[ "$(wc -l <bin/release-images.txt)" -eq 11 ]]
grep -q ' .*/envoy-dataplane$' bin/release-images.txt

echo "== a rerun builds nothing"
ATE_PUBLISH_DIGESTS_FILE=bin/rerun.txt go run ./cmd/ate-setup publish release-images
[[ ! -e bin/rerun.txt ]]

echo "== the published images verify, with the same digests"
DIGESTS_FILE=bin/verify.txt .github/scripts/release/verify-image-attestations.sh
diff <(sort bin/release-images.txt) <(sort bin/verify.txt)

echo "== gh attestation verify by tag"
for image in envoy-dataplane ateapi; do
  gh attestation verify "oci://${KO_DOCKER_REPO}/${image}:${VERSION}" --repo "${GITHUB_REPOSITORY}" --signer-workflow "${signer}"
done

echo "== the attestation statement"
hex="$(awk '/ .*\/ateapi$/ {print $1}' bin/release-images.txt)"
gh attestation verify "oci://${KO_DOCKER_REPO}/ateapi@sha256:${hex}" --repo "${GITHUB_REPOSITORY}" --signer-workflow "${signer}" --format json \
  | jq '.[0].verificationResult.statement | {subjects: (.subject | length), names: [.subject[].name], ref: .predicate.buildDefinition.externalParameters.workflow.ref, path: .predicate.buildDefinition.externalParameters.workflow.path, commit: .predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit}'

echo "== a renamed image fails verification"
go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 copy "${KO_DOCKER_REPO}/ateapi:${VERSION}" "${KO_DOCKER_REPO}/atelet:${VERSION}"
if DIGESTS_FILE=bin/tamper.txt .github/scripts/release/verify-image-attestations.sh; then
  echo "::error::the renamed image verified"
  exit 1
fi
echo "TRIAL_OK"
# Second commit so the branch head differs from the trial tag.
