Deployed 2026-10-31 06:42 UTC to isu01, isu02, isu03.
SHA256: e495196b7b459e823b2d5f5ceb258271b868e94c46b93121cdbdc970819e2abe
All three services active and enabled; binary hashes match.
Backup suffix: before-asia-api-20261031T064249Z.
Includes HTTP Asia writes / critical authorization, local async replica payload reads, and merged CPU optimizations.
Full go test ./... and go test -race ./... passed before deployment.
Signed ready and Europe replica_health passed after deployment.
E2E fixture registration, login and both regional GET /api/me passed. Full posting consistency check stopped because current posting window was null; no competition state was altered to open a window.
Europe Go sockets confirmed localhost:5432 and Asia:80, without direct primary:5432 sockets at inspection.
Official benchmark performance/pass remains unverified for this deployment.
Deploy future builds from this worktree to preserve both HTTP routing and CPU changes.
