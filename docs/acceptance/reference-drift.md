# Reference refresh and offline drift review

Run from the repository root using a prebuilt `conformdiff` on PATH (or set
`CONFORMDIFF` to its absolute path). The check never starts a server, discovers
an endpoint, downloads a schema, or contacts the reference lab:

```sh
scripts/reference-drift.sh check jellyfin /path/to/retained-recording
conformdiff drift -protocol emby -fixtures testdata/conform/emby -candidate /path/to/new-emby
```

All profiles, requests and response formats in the current corpus must exist in
both recordings. Missing, malformed or empty corpora fail. Every captured response field is compared, including status, headers, body,
range metadata, truncation and reference-defect annotations. JSON envelope
formatting and map order do not count as changes; response bodies remain exact
bytes. Any change fails review and reports the before/after SHA-256 hashes.
There is no redaction, ignore expression or not-applicable suppression at this
step. Inspect the fixture diff to review the content; raw bodies are not echoed
into reports. This detects recording drift, not protocol validity. In particular,
identical partial captures may still fail separate range or schema validation.
Run those conformance gates as well. This is not Plex byte-identity proof or
client certification.

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
before staging. `scripts/reference-drift.sh` does not create pull requests or publish data on its own.

The PR job `.github/workflows/reference-drift.yml` compares the PR base recording
with the proposed recording using the current corpus and saves all protocol
reports as an artifact, including failures. New corpus requests without both
recordings fail rather than silently shrinking coverage. A manual dispatch checks
the current recordings against themselves to validate corpus coverage. A drift
failure requires a human review of the retained report; it never rewrites a pin.

The scheduled `.github/workflows/api-drift.yml` resolves the latest stable
Jellyfin and Emby releases and the current Silo `aio` image, diffs the pinned
OpenAPI descriptions, and opens or updates a pull request. The pull request
lists endpoint changes Ether implements and any blocking conformance diffs.
It does not merge itself, and it does not commit lab credentials.

That workflow cannot reach the reference lab. Re-record and, when a Lab Keeper
window is open, roll the containers from bb-worker-server or gpu-workstation:

```sh
# scripts/lab/lab.env mode 600, then scripts/lab/discover.sh
DRIFT_WATCH_APPLY=1 DRIFT_WATCH_OPEN_PR=1 scripts/drift-watch.sh run
```

`DRIFT_WATCH_APPLY=1` is refused on macOS and on 10.0.0.43. Recordings stay in
gitignored `drift-watch-out/` unless `DRIFT_WATCH_COMMIT_FIXTURES=1`. Set
`DRIFT_WATCH_SKIP_RECORD=1` to publish the OpenAPI and image plan only. The
report is `docs/acceptance/drift-watch-report.json` on the drift branch. It is
not offline conformance evidence.

Client certification is the published matrix in the user guide, checked by
`scripts/certification-gate`. Minor and patch tags at or after 0.3 do not
publish until that minor is listed in `docs/certification/matrix.json`. Native
certification is still pending.
