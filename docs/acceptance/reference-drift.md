# Reference refresh and offline drift review

Run from the repository root using a prebuilt `conformdiff` on PATH (or set
`CONFORMDIFF` to its absolute path). The check never starts a server, discovers
an endpoint, downloads a schema, or contacts the reference lab:

```sh
scripts/reference-drift.sh check jellyfin /path/to/retained-recording
conformdiff drift -protocol emby -fixtures testdata/conform/emby -candidate /path/to/new-emby
```

All profiles, requests and response formats in the current corpus must exist in
both recordings. Missing, malformed or empty corpora fail. Status, headers and
response schema use the existing protocol comparer, including request-specific
rules. Value differences reported by that comparer also fail drift review.
There are no extra ignore expressions or not-applicable manifest suppressions.
This is a reference comparison, not Plex byte-identity proof or client certification.
The semantic comparer intentionally does not compare every byte of media bodies.

For a refresh, first reserve a window from the Lab Keeper. Only the Lab Keeper
starts, stops or changes reference servers. Freeze the reference server version,
media hashes, library IDs, viewer state, client profiles and spec manifest hashes
in the review evidence. Reference credentials must stay in the environment and
out of logs, command arguments and commits.

```sh
export CONFORM_ALLOW_RECORD=1
export CONFORM_REFERENCE_URL=http://allocated-isolated-reference:8096
# Supply CONFORM_REFERENCE_TOKEN securely in the environment.
scripts/reference-drift.sh record jellyfin /path/to/new-recording
```

Record mode requires a new output directory, preserves partial/failing recordings
and writes record and drift reports there. It never overwrites the checked-in
baseline. A failing record stops before drift; retain its report and stderr.
Fixture-only captured requests cannot be reconstructed by HTTP recording: supply
fresh reviewed captures for those requests before checking a complete candidate.
Never copy an old fixture to conceal missing fresh evidence.

Review every difference, reference defect, discovery warning and recording error.
Re-run schema validation using the pinned local spec cache (`conformdiff validate`)
and the offline corpus tests. Open a review PR containing the reviewed fixtures,
updated manifests/profiles/spec pins where justified, provenance and raw gate
receipts. Do not automatically accept new fixtures or suppress drift. Credentials,
private addresses and account IDs must be redacted by the recorder and checked
before staging. This workflow does not create PRs or publish data on its own.

The PR job `.github/workflows/reference-drift.yml` compares the PR base recording
with the proposed recording using the current corpus and saves all protocol
reports as an artifact, including failures. New corpus requests without both
recordings fail rather than silently shrinking coverage. A manual dispatch checks
the current recordings against themselves to validate corpus coverage. A drift
failure requires a human review of the retained report; it never rewrites a pin.

The scheduled `.github/workflows/api-drift.yml` separately checks upstream API
schemas and reports issues. It requires network access; it does not refresh lab
fixtures. Its report is not offline conformance evidence. Public launch and final
certification remain pending the frozen candidate and native-device gates.
