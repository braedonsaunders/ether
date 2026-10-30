#!/bin/sh
# Freeze the actual delivered PostgreSQL DDL, using its translator and lexer.
# Run from the repository root after extracting schema.sql with extract9503.go.
set -eu
fixture=internal/app/testdata/prism-upgrade
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
for name in translate lex; do
 git show 9503ece9d544e5f111f2afa531e87a2c3ad0e6c1:internal/store/$name.go | sed 's/^package store$/package main/' > "$scratch/$name.go"
done
cat > "$scratch/main.go" <<'GO'
package main
import("os"; "strings")
func main() {
 raw, err := os.ReadFile(os.Args[1]); if err != nil { panic(err) }
 parts := strings.Split(string(raw), "\n-- prism-upgrade-migration-end\n")
 for i, part := range parts { parts[i] = TranslateDDL(part) }
 if err := os.WriteFile(os.Args[2], []byte(strings.Join(parts,"\n-- prism-upgrade-migration-end\n")), 0644); err != nil { panic(err) }
}
GO
GO111MODULE=off GOWORK=off go run "$scratch/main.go" "$scratch/translate.go" "$scratch/lex.go" "$fixture/schema.sql" "$fixture/schema-postgres.sql"
python3 - <<'PY'
import hashlib
import json
from pathlib import Path
fixture = Path('internal/app/testdata/prism-upgrade')
manifest = fixture / 'manifest.json'
value = json.loads(manifest.read_text())
value['postgres_sha256'] = hashlib.sha256((fixture / 'schema-postgres.sql').read_bytes()).hexdigest()
value['postgres_source'] = '9503 internal/store/{translate,lex}.go; no production RegisterDialect overrides'
manifest.write_text(json.dumps(value, indent=2) + '\n')
PY
