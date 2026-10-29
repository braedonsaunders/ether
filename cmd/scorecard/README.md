# HTTP latency measurement (experimental)

This opt-in tool measures full response time for configured GET requests. It is
only one component of the Prism scorecard; it does not establish fixture parity,
profile compatibility, streaming startup, seek, features, or device experience.
It makes `samples` total requests to each server at each specified concurrency,
with a 30-second timeout. Non-2xx responses and transport errors are counted;
any errors make the comparison inconclusive. p50 and p95 use nearest rank among
successful responses. Reference is measured before Ether, so repeat and reverse
order externally when assessing cache or time effects. A mixed result means
one server won p50 and the other p95. No statistical significance is implied.

Set `SCORECARD_CONFIG` to a private JSON file outside the repository, or pass
`-config`. Supply endpoint URLs and authentication headers separately for each
server. Output contains names and aggregate measurements, never URLs or headers.
Use only read-only endpoints; a GET endpoint can still have side effects. Verify
that the servers, viewers, media and operations are equivalent before comparison.

```json
{
  "samples": 100,
  "concurrency": [1, 10, 50],
  "scenarios": [{
    "name": "home",
    "reference": {"url": "http://reference/Users/example/Views", "headers": {}},
    "ether": {"url": "http://ether/Users/example/Views", "headers": {}}
  }]
}
```

Run `go run ./cmd/scorecard > report.json`. Keep the configuration private; it
may contain credentials. This tool does not authenticate or discover IDs.
