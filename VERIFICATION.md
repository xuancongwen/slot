# Initial verification

Verified September 26, 2026, using Go 1.27.1 on macOS / Apple M1 Max.

- All 17 automated tests passed with the Go race detector.
- `go vet ./...` passed.
- `govulncheck` reported no known vulnerabilities in the checked build.
- Optimized, CGO-free builds succeeded for macOS ARM64 and Linux AMD64. Each binary is approximately 14 MB.
- Slot-generation microbenchmark: approximately 44 microseconds per day, 7.7 KB allocated, 33 allocations. This measures local availability calculation only, not Google API latency or end-to-end HTTP throughput.
- Public and admin listeners each returned a healthy status on their separate local ports.
- Sign-in page inspected visually in the in-app browser; registration page inspected through its rendered accessibility tree. The admin and booking templates also render in the HTTP integration tests.
- Docker Compose configuration validation passed. The container image could not be built/run because the local Docker daemon was not running. Its runtime remains unverified.
- Real Google consent, live calendars, and invitation delivery remain to be tested with the owner's Google OAuth credentials. The automated suite uses fake Calendar providers and local HTTP servers, including real OAuth-library token-refresh requests against a local test server.

Preview data is kept outside the deliverable in `work/preview-data`. The source package contains no test accounts, credentials, or populated database.
