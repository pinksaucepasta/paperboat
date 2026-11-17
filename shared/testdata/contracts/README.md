# Contract fixtures

Fixtures, schemas and vectors that tests of more than one module read. The prose contracts
live in [`shared/docs/contracts`](../../docs/contracts).

- **One copy.** A file more than one module reads lives here. A file only one module reads
  stays in that module's `testdata/contracts` (`cli/`, `server/`). Never copy one.
- **A reader or no file.** Every file here is read by a test. A fixture or schema nothing
  reads is deleted, not kept as documentation.
- **Change it with its producer.** Edit the fixture in the same commit as the code that
  produces or consumes it, then run the Go tests of every module that reads it
  (`git grep testdata/contracts`).

## Updating vectors

| Kind | Files | Rule |
|---|---|---|
| NDJSON/JSON vectors | `fixtures/`, `*-v1/fixtures/`, `states/`, `credentials/` | Hand-edited and append-only: add a vector for new behaviour, never loosen or drop one to make a test pass. |
| Byte goldens | `shared/runtimeattachment/testdata/` | Written by the server tests: `PAPERBOAT_UPDATE_GOLDEN=<that testdata dir> go test ./server/internal/runtimeattachment -run <failing test>` (needs PostgreSQL), then review the diff; edge and cli readers must still pass. |
| Signed or sealed vectors | `fixtures/credentials/terminal-operation.ed25519.json`, `environment-e2ee-v1/` | Fixed on purpose: other languages verify them. Regenerate only when the format version changes (`cli-vault.json`: seal a new vault with the CLI code, `pb env vault`, and keep `account_id` `user_cli_fixture`), and say so in the commit. |
| CLI reference docs | `cli/docs` | Generated: `make -C cli cli-docs`. |
