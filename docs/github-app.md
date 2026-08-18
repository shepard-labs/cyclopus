# GitHub App setup for webhook routing

Cyclopus supports one GitHub App per Cyclopus instance. The App supplies installation credentials, repository discovery, workflow dispatch, and the webhook signing secret used by `POST /webhooks/github`.

For runtime limits, rollout, verification, alerts, and recovery, see [GitHub webhook operations](operations.md).

## Create the singleton App

1. Sign in to Cyclopus as an administrator.
2. Start the GitHub App manifest flow at `/github/app-manifest/start`.
3. GitHub returns to `/github/app-manifest/callback` with a one-time manifest code.
4. Cyclopus exchanges the code, stores the App metadata in the singleton `github_app` row, and writes the App ID, private key, and webhook secret to the retained `cyclopus-github-app` Kubernetes Secret.
5. GitHub then opens `/github/app/setup` to finish installation and repository selection.

The manifest is public and uses these URLs relative to `CYCLOPUS_EXTERNAL_URL`:

| Purpose | Path |
|---|---|
| Manifest start | `/github/app-manifest/start` |
| Manifest callback | `/github/app-manifest/callback` |
| Post-creation setup | `/github/app/setup` |
| Webhook delivery | `/webhooks/github` |

After creation, verify the App's **Webhook URL** is exactly:

```text
https://<cyclopus-host>/webhooks/github
```

The current manifest builder in `internal/service/github_onboarding.go` still emits the legacy hook path `/github/webhooks` and includes the `repository` event. Until that production-code drift is corrected, edit the App after creation: set the URL to `/webhooks/github` and remove the **Repository** subscription. Cyclopus does not serve `/github/webhooks`.

## Required permissions

Configure only these repository permissions:

| Permission | Access | Why |
|---|---|---|
| Metadata | Read-only | Resolve stable repository and installation identity. |
| Contents | Read-only | Read branch refs and compatible workflow files. |
| Actions | Read and write | Dispatch and observe the configured workflow. |

No account, organization, issue, pull-request, check, deployment, or administration permission is required for Phase 5 routing.

## Exact webhook subscriptions

Subscribe to exactly:

- **Push** (`push`)
- **Installation** (`installation`)
- **Installation repositories** (`installation_repositories`)

Do not subscribe to pull-request events. Cyclopus does not use PR numbers for preview identity. Do not subscribe to `repository`, `workflow_run`, `check_run`, `release`, or tag events for this flow.

Cyclopus handles:

- `installation`: `created`, `deleted`, `suspend`, `unsuspend`, and `new_permissions_accepted`
- `installation_repositories`: `added` and `removed`
- `push`: branch updates and GitHub's all-zero `after` value for branch deletion
- `ping`: authenticated connectivity check

An authenticated `ping` is stateless: Cyclopus returns HTTP `202` without resolving a tenant and without creating a `github_webhook_deliveries` row. Replay and conflict semantics therefore do not apply to `ping`.

## Webhook HMAC secret references

Cyclopus resolves HMAC keys from Kubernetes Secret references. It does not accept inline signing keys.

The managed active reference defaults to:

```text
k8s-secret://<control-plane-namespace>/cyclopus-github-app#webhook-secret
```

The related configuration is:

| Environment variable | Meaning |
|---|---|
| `CYCLOPUS_WEBHOOK_GITHUB_ACTIVE_SECRET_REF` | Active HMAC secret reference. Defaults to the managed App Secret reference. |
| `CYCLOPUS_WEBHOOK_GITHUB_ACTIVE_KEY_VERSION` | Immutable label stored with newly accepted deliveries. Default `managed`. |
| `CYCLOPUS_WEBHOOK_GITHUB_PREVIOUS_SECRET_REF` | Optional previous HMAC secret reference during rotation. |
| `CYCLOPUS_WEBHOOK_GITHUB_PREVIOUS_KEY_VERSION` | Required immutable label when a previous reference is set. |
| `CYCLOPUS_WEBHOOK_GITHUB_SECRET_OVERLAP_UNTIL` | Required RFC3339 cutoff for accepting the previous key. |
| `CYCLOPUS_WEBHOOK_ALLOWED_SECRET_REFS` | Comma-separated extra exact references that the runtime Secret policy may read. |
| `CYCLOPUS_WEBHOOK_RELEASE_NAME` | Required when extra allowed references are configured. |

Extra signing Secrets must:

- be in `CYCLOPUS_KUBERNETES_INSTANCE_NAMESPACE`;
- have a name beginning with `cyclopus-webhook-`;
- be immutable and owned by the Helm release;
- carry `cyclopus.app/credential-purpose=webhook-signing`; and
- appear as an exact reference in `CYCLOPUS_WEBHOOK_ALLOWED_SECRET_REFS`.

### Zero-window rotation

1. Create the new immutable Secret without changing GitHub.
2. Deploy Cyclopus with the new Secret as **active**, the old Secret as **previous**, distinct immutable version labels, and a bounded future overlap cutoff.
3. Wait for `/readyz` to return `200`; readiness proves that the active reference resolves to a non-empty secret.
4. Change the GitHub App webhook secret to the new value.
5. Send a GitHub ping and verify HTTP `202` plus an `accepted` ingress metric.
6. Keep the previous reference until the overlap cutoff and until no in-flight delivery needs operational comparison with the old version label.
7. Remove the previous reference, version, and cutoff; then remove the old Kubernetes Secret according to the credential-retention policy.

Cyclopus tries the active key first. It tries the previous key only when all previous-key settings are present and the current time is before `CYCLOPUS_WEBHOOK_GITHUB_SECRET_OVERLAP_UNTIL`. It records the matched version in `github_webhook_deliveries.signature_key_version`.

## Compatible workflow contract

Each imported app stores one workflow path matching `.github/workflows/<file>.yml` or `.yaml`. Cyclopus validates the workflow with `internal/service/github_workflow.go`; the contract is covered by `internal/service/github_workflow_test.go`.

The workflow must:

- use only `workflow_dispatch`;
- require string inputs `orchestration_job_id` and `commit_sha`;
- set `run-name: cyclopus:${{ inputs.orchestration_job_id }}`;
- grant workflow permissions `contents: read` and `packages: write`;
- run one job on `ubuntu-latest`;
- check out `actions/checkout@v4` with `ref: ${{ inputs.commit_sha }}`;
- verify that `git rev-parse HEAD` equals the supplied 40-hex commit SHA; and
- build and push with `docker/build-push-action@v6`, using the exact configured image repository tagged with `${{ inputs.commit_sha }}`.

Optional steps are limited to the validated `docker/login-action@v3` and `docker/setup-buildx-action@v3` forms. Reusable workflows, matrices, job-level execution controls, dynamic runners, repository mutation, and extra dispatch inputs are rejected.

Webhook routing preserves this contract: production and preview pushes enqueue the stored workflow path with exactly `orchestration_job_id` and `commit_sha`. Cyclopus never edits repository workflow files.

## Post-setup checks

1. Confirm the App is installed on the intended repositories.
2. Confirm the exact permissions and three subscriptions above.
3. Confirm the Webhook URL ends in `/webhooks/github`.
4. Confirm `/readyz` returns `200`.
5. Send a GitHub ping and confirm `202`.
6. Follow the production-push, preview-push, and branch-deletion procedures in [operations.md](operations.md#verification-procedures).
