## Phase
Phase 2 (B) — idempotent installer secrets via /root/.cyclopus-install.env state file. Commit 07012c2.

## What Was Done
- scripts/install.sh (+93/−23): STATE_FILE=/root/.cyclopus-install.env; load-or-generate block for six secrets (AUTH_BOOTSTRAP_SECRET, AUTH_TOKEN_PEPPER, DB_PASSWORD, OO_PASSWORD, BESZEL_PUBLIC_KEY, BESZEL_PRIVATE_KEY — Beszel keys base64-encoded)
- First run: generate all six (incl. ed25519 keypair moved up from observability section), write state file under umask 0077 (0600)
- Rerun: whitelist-validate every line (anchored to the six key names + comment lines), source, validate all six non-empty, base64-decode keys with fail-context, structural key checks
- PG role block: IF NOT EXISTS create ELSE ALTER ROLE PASSWORD reconciliation (heals pre-Phase-B installs)
- openobserve-auth k8s secret now via 0600 temp manifest + kubectl apply -f (was --from-literal argv leak)
- Cleanup trap: dead SECRETS_FILE partial-install branch replaced with STATE_FILE idempotent-rerun warning

## Key Decisions
- Both Beszel keys base64 (not raw public key): one charset whitelist `[A-Za-z0-9+/=]` covers the whole file — simpler than mixed patterns
- Whitelist anchored to exact key names after review caught PATH="aGV4" would pass a generic [A-Z_]+ pattern (command-hijack vector)
- ALTER ROLE on every rerun (not just create): converges role password to state file; fixes legacy installs where the password existed nowhere on disk
- ssh-keygen moved before the PG apt block → explicit openssh-client availability guard added

## Findings Fixed
- Legacy-install desync (no PG password on disk) → ALTER ROLE reconciliation
- "delete it to regenerate" footgun message → warning about OO password + Beszel enrollment
- Unvalidated base64 decodes → fail-wrapped decodes + structural case checks
- Sourcing injection via tampered file → anchored whitelist before source
- kubectl --from-literal argv exposure → 0600 manifest apply
- Dead cleanup-trap branch → STATE_FILE warning

## DoD Results
- STATE_FILE defined; six-var validation; incomplete → explicit fail: PASS
- umask 0077 → 0600; six KEY lines; keys base64: PASS
- Observability block consumes variables only: PASS
- $SECRETS_FILE render unchanged, correct both paths: PASS
- bash -n + shellcheck exit 0: PASS
- No other files modified: PASS
- Adversarial re-review: REVIEW CLEAN

## Watch Out For
- STATE_FILE persists forever at /root/.cyclopus-install.env (0600) — unlike SECRETS_FILE, users are NOT told to delete it; it's the rerun idempotency anchor. Rotation is out of scope (would need OO/Beszel/PG coordinated rotation).
- A non-empty garbage private key that still starts with the OpenSSH header is not detectable without an ssh-keygen -y round-trip — known limitation, not implemented.
- Written file must pass its own whitelist — if you ever add a value with spaces/special chars to STATE_FILE, the anchored charset pattern must change too.
- docs/ is gitignored in this repo; learnings commits need git add -f.
