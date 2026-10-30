# Release notes

Every release must have a reviewed `release-notes/<tag>.md` in the tagged commit,
for example `release-notes/v0.2.48.md`. The release workflow refuses to build
without this file and applies its contents before publishing the draft.

Describe the changes since the previous release in language an owner can use:

- Lead with the major features and fixes, grouped by their effect on users.
- Explain how to enable optional features and any required upgrade steps.
- Link relevant issues or pull requests and the complete commit comparison.
- State material limitations. Distinguish an offline regression fix from a
  reporter's unverified live-client symptoms.
- Include only changes present in the tagged commit. Development branches,
  planned features, and fixes awaiting integration are not shipped features.
- Coordinate security notes with remediation. Do not publish private findings
  through an ordinary release-note draft.

Keep the GitHub release body and this file aligned. Historical notes here were
backfilled from published releases; their original tags predate this requirement.
