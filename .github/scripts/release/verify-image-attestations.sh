#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# For every release image the registry holds for VERSION, checks that a build
# provenance attestation from this repository's release workflow names its
# digest, and writes the digests to DIGESTS_FILE in the form the draft notes
# read. Runs when a rerun found the image set complete and built nothing:
# those images were attested by the run that built them, and one that was
# not is refused rather than attested now.
#
#   GITHUB_REPOSITORY=<owner/repo> VERSION=<tag> KO_DOCKER_REPO=<registry path> \
#     GH_TOKEN=<token> DIGESTS_FILE=<path> \
#     .github/scripts/release/verify-image-attestations.sh
#
# Used by the images job in .github/workflows/release.yaml.

set -o errexit -o nounset -o pipefail

signer="${GITHUB_REPOSITORY}/.github/workflows/release.yaml"
published="$(go run ./cmd/ate-setup publish release-images --list-published)"
mkdir -p "$(dirname "${DIGESTS_FILE}")"
: > "${DIGESTS_FILE}"
failed=0
# Each line is repo/image:tag@sha256:hex.
while read -r ref; do
  digest="${ref##*@}"
  image="${ref%@*}"
  image="${image%:*}"
  if gh attestation verify "oci://${image}@${digest}" --repo "${GITHUB_REPOSITORY}" --signer-workflow "${signer}"; then
    printf '%s  %s\n' "${digest#sha256:}" "${image}" >>"${DIGESTS_FILE}"
  else
    echo "::error::${ref} has no build provenance attestation from ${signer}; run the workflow again with overwrite to rebuild and attest the set"
    failed=1
  fi
done <<<"${published}"
exit "${failed}"
