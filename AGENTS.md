# AGENTS.md

Single source of truth for AI coding agents. `CLAUDE.md` and `GEMINI.md` defer to it.

## Project
<!-- Agents read this first: purpose, layout, exact build/test/run commands. -->
- Purpose: Self-hosted Google Calendar booking app for a home lab. One Go binary, one SQLite database (`modernc.org/sqlite`, CGO-free), separate public (`:8080`) and admin (`127.0.0.1:8081`) listeners, server-rendered HTML with templates and static assets embedded.
- Build: `make build` (Go 1.26+)
- Test: `make check` (gofmt check, vet, race tests); benchmarks: `make bench`
- Run: `make run` (data in `./data`; config via env vars, see README), or `make up` for Docker Compose

<!-- agentinit:begin v0.1.0. Re-running agentinit replaces this block; edit outside the markers. -->
## Workflow
- Ask when the task is ambiguous. Prefer small, reviewable changes.
- Run formatter, linter, and tests before declaring done. Report failures verbatim; never claim unverified success.
- One logical change per commit: imperative subject under 50 chars, body says *why*. No secrets, build artifacts, or unrelated changes.
- No history rewrites or destructive git (`push --force`, `reset --hard`, `clean -f`) without explicit approval.
- Never commit to `master` directly. Branch per task (`fix/null-config`), open a PR for every merge into `master`, and do not merge it yourself unless told to.
- No AI attribution (`Co-Authored-By` trailers, "Generated with" footers) in commits, PRs, or code.

## Code
- Match the surrounding code's style, idioms, and structure. Consistency beats preference.
- Simplest thing that works. No speculative abstraction or configurability (YAGNI).
- DRY, but extract on the third repetition, not the first. Duplication beats the wrong abstraction.
- Names carry meaning; code reads without comments. Comment *why* (intent, trade-offs, gotchas), never *what*. Delete stale comments.
- Document public APIs and non-obvious decisions; never restate a signature.
- Small units, one responsibility. Explicit errors at boundaries; no silent catch-alls.
- Standard library first. Justify every new dependency; pin it in the lockfile.
- Validate at boundaries, parameterize queries, never log secrets.
- Test behavior, not implementation. Bug fixes start with a failing test. Tests are fast, deterministic, isolated.
- Stay in scope: no behavior, API, or formatting changes the task did not ask for.

## Languages

### Go
- `gofmt`, `go vet`, and `golangci-lint` (if configured) clean.
- Handle every error; wrap with `fmt.Errorf("doing x: %w", err)`; no `panic` outside `main`.
- Accept interfaces, return structs; small interfaces; `context.Context` first.
- No package-level mutable state or `init()`; useful zero values.
- Table-driven tests with `t.Run`; `go test -race ./...` passes.

### TypeScript
- `strict: true`. No `any` (use `unknown` and narrow); no `!` or `as` without a justifying comment.
- Discriminated unions over optional fields; `readonly` where possible; derive types (`typeof`, `satisfies`, `as const`) rather than duplicate.
- Union literals over enums; export types beside the values they describe.
- Otherwise as JavaScript: ES modules, `const` by default, `async`/`await` with every rejection handled, Prettier and ESLint clean.
<!-- agentinit:end -->
