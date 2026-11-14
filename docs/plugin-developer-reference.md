# Plugin developer reference

The console reference lives at `/ether/plugins/reference/getting-started`.
The Plugins header, Studio header, and Code home all link to the same reference.
Every topic has a stable `/plugins/reference/:topic` URL. Search is local across
all guides and generated declarations, including collapsed source, with Cmd/Ctrl K
or `/` to focus and Escape to clear. Search queries are URL-backed (`?q=`).

`web/src/pages/plugins/reference-guides.json` contains the authored chapters,
service explanations, and copyable examples. Contract sections are generated:

```sh
go generate ./internal/plugins/devreference
go test ./internal/plugins/devreference -run TestReferenceMatchesContract
```

The drift test runs with the ordinary Go CI package gate; it compares the entire
checked-in generated JSON with current source. Regenerate after changing manifest
validation, schema, types, host services, SDKs, conformance kit, or examples.
The generator uses Go AST declarations, preserving comments/signatures/errors,
adds field tables with wire presence, and recursively flattens schema properties
with requiredness and constraints. Schema requiredness and Go wire presence are
kept distinct: omitempty does not prove a capability field is optional.
SDK and example source discovery includes new files automatically. New runtime
bindings or contract source files outside these roots must be added to the source
list in reference.go. The generated corpus is imported only by the lazy reference
route; opening Plugins or Studio does not download or index it.

Developer prose, chapter names, schema tables, API declarations and examples stay
in English to preserve exact API vocabulary and make snippets searchable against
the code. UI controls (search, results, copy, navigation controls) use the console
translation catalogue. No runtime translation or network search is performed.

Hosted SDK imports must be bundled into the entry artifact before packaging;
bare imports do not resolve inside the hosted virtual filesystem. Remote and
hosted APIs differ: folder/secret services are hosted only. Test paid accounts
through the owner's connection test after offline conformance.
