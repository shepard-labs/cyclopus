package service

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const compatibleWorkflow = `name: deploy
run-name: cyclopus:${{ inputs.orchestration_job_id }}
on:
  workflow_dispatch:
    inputs:
      orchestration_job_id: {type: string, required: true}
      commit_sha: {type: string, required: true}
permissions:
  contents: read
  packages: write
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: {ref: "${{ inputs.commit_sha }}"}
      - env:
          CYCLOPUS_COMMIT_SHA: "${{ inputs.commit_sha }}"
        run: |
          [[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-fA-F]{40}$ ]]
          test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"
      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ghcr.io/acme/api:${{ inputs.commit_sha }}
`

func TestAnalyzeGitHubWorkflowStrictContract(t *testing.T) {
	if got := AnalyzeGitHubWorkflow([]byte(compatibleWorkflow), "ghcr.io/acme/api"); !got.Compatible || len(got.Reasons) != 0 {
		t.Fatalf("compatible analysis = %+v", got)
	}
	tests := map[string]struct{ workflow, image, reason string }{
		"missing input":           {strings.Replace(compatibleWorkflow, "commit_sha: {type: string, required: true}", "other: {type: string, required: true}", 1), "ghcr.io/acme/api", "workflow_dispatch_inputs_invalid"},
		"wrong run name":          {strings.Replace(compatibleWorkflow, "cyclopus:${{ inputs.orchestration_job_id }}", "deploy", 1), "ghcr.io/acme/api", "run_name_invalid"},
		"wrong checkout":          {strings.Replace(compatibleWorkflow, `ref: "${{ inputs.commit_sha }}"`, `ref: main`, 1), "ghcr.io/acme/api", "checkout_commit_sha_missing"},
		"checkout path":           {strings.Replace(compatibleWorkflow, `with: {ref: "${{ inputs.commit_sha }}"}`, `with: {ref: "${{ inputs.commit_sha }}", path: source}`, 1), "ghcr.io/acme/api", "checkout_commit_sha_missing"},
		"checkout repository":     {strings.Replace(compatibleWorkflow, `with: {ref: "${{ inputs.commit_sha }}"}`, `with: {ref: "${{ inputs.commit_sha }}", repository: acme/other}`, 1), "ghcr.io/acme/api", "checkout_commit_sha_missing"},
		"checkout extra input":    {strings.Replace(compatibleWorkflow, `with: {ref: "${{ inputs.commit_sha }}"}`, `with: {ref: "${{ inputs.commit_sha }}", fetch-depth: 1}`, 1), "ghcr.io/acme/api", "checkout_commit_sha_missing"},
		"missing guard":           {strings.Replace(compatibleWorkflow, `test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"`, "echo build", 1), "ghcr.io/acme/api", "commit_sha_guard_missing"},
		"direct expression guard": {strings.Replace(compatibleWorkflow, `test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"`, `test "$(git rev-parse HEAD)" = "${{ inputs.commit_sha }}"`, 1), "ghcr.io/acme/api", "commit_sha_guard_missing"},
		"missing guard env":       {strings.Replace(compatibleWorkflow, "      - env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"\n        run:", "      - run:", 1), "ghcr.io/acme/api", "commit_sha_guard_missing"},
		"unsafe checkout ref":     {strings.Replace(compatibleWorkflow, "actions/checkout@v4", "actions/checkout@main", 1), "ghcr.io/acme/api", "checkout_commit_sha_missing"},
		"checkout sha ref":        {strings.Replace(compatibleWorkflow, "actions/checkout@v4", "actions/checkout@0123456789012345678901234567890123456789", 1), "ghcr.io/acme/api", "checkout_commit_sha_missing"},
		"empty checkout ref":      {strings.Replace(compatibleWorkflow, "actions/checkout@v4", "actions/checkout@", 1), "ghcr.io/acme/api", "checkout_commit_sha_missing"},
		"build branch ref":        {strings.Replace(compatibleWorkflow, "docker/build-push-action@v6", "docker/build-push-action@main", 1), "ghcr.io/acme/api", "build_push_tag_invalid"},
		"missing build context":   {strings.Replace(compatibleWorkflow, "          context: .\n", "", 1), "ghcr.io/acme/api", "build_push_tag_invalid"},
		"remote build context":    {strings.Replace(compatibleWorkflow, "context: .", "context: https://github.com/acme/api.git", 1), "ghcr.io/acme/api", "build_push_tag_invalid"},
		"subdir build context":    {strings.Replace(compatibleWorkflow, "context: .", "context: ./service", 1), "ghcr.io/acme/api", "build_push_tag_invalid"},
		"default build context":   {strings.Replace(compatibleWorkflow, "context: .", "context: '{{defaultContext}}'", 1), "ghcr.io/acme/api", "build_push_tag_invalid"},
		"dynamic build context":   {strings.Replace(compatibleWorkflow, "context: .", "context: ${{ inputs.context }}", 1), "ghcr.io/acme/api", "build_push_tag_invalid"},
		"dynamic tag":             {strings.Replace(compatibleWorkflow, `ghcr.io/acme/api:${{ inputs.commit_sha }}`, `${{ env.IMAGE }}:${{ inputs.commit_sha }}`, 1), "", "build_push_tag_invalid"},
		"image mismatch":          {compatibleWorkflow, "ghcr.io/acme/other", "image_repository_mismatch"},
		"workflow call":           {strings.Replace(compatibleWorkflow, "  workflow_dispatch:", "  workflow_call:\n  workflow_dispatch:", 1), "ghcr.io/acme/api", "reusable_workflow_unsupported"},
		"matrix":                  {strings.Replace(compatibleWorkflow, "    runs-on:", "    strategy: {matrix: {arch: [amd64]}}\n    runs-on:", 1), "ghcr.io/acme/api", "workflow_indirection_unsupported"},
		"alias":                   {compatibleWorkflow + "extra: &x value\ncopy: *x\n", "ghcr.io/acme/api", "workflow_yaml_unsupported"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := AnalyzeGitHubWorkflow([]byte(test.workflow), test.image)
			if got.Compatible || !contains(got.Reasons, test.reason) {
				t.Fatalf("analysis = %+v, want %s", got, test.reason)
			}
		})
	}
}

func TestAnalyzeGitHubWorkflowLimitsAndYAML11On(t *testing.T) {
	if got := AnalyzeGitHubWorkflow([]byte(compatibleWorkflow), ""); !got.Compatible {
		t.Fatalf("YAML on key analysis = %+v", got)
	}
	if got := AnalyzeGitHubWorkflow([]byte(strings.Repeat("x", maxWorkflowBytes+1)), ""); !contains(got.Reasons, "workflow_too_large") {
		t.Fatalf("size analysis = %+v", got)
	}
	nested := "on: workflow_dispatch\n"
	for range maxWorkflowDepth + 2 {
		nested += "a: {"
	}
	if got := AnalyzeGitHubWorkflow([]byte(nested), ""); got.Compatible {
		t.Fatal("deep workflow unexpectedly compatible")
	}
}

func TestAnalyzeGitHubWorkflowRejectsDuplicateScalarKeysRecursively(t *testing.T) {
	tests := map[string]string{
		"top-level jobs safe then unsafe": compatibleWorkflow + "jobs: unsafe\n",
		"top-level jobs unsafe then safe": strings.Replace(compatibleWorkflow, "jobs:\n", "jobs: unsafe\njobs:\n", 1),
		"top-level on":                    strings.Replace(compatibleWorkflow, "on:\n", "on: unsafe\non:\n", 1),
		"top-level run-name":              strings.Replace(compatibleWorkflow, "run-name: cyclopus:${{ inputs.orchestration_job_id }}", "run-name: unsafe\nrun-name: cyclopus:${{ inputs.orchestration_job_id }}", 1),
		"job id":                          strings.Replace(compatibleWorkflow, "  deploy:\n", "  deploy: unsafe\n  deploy:\n", 1),
		"step uses unsafe then safe":      strings.Replace(compatibleWorkflow, "      - uses: actions/checkout@v4", "      - uses: unsafe/action@v1\n        uses: actions/checkout@v4", 1),
		"step uses safe then unsafe":      strings.Replace(compatibleWorkflow, "      - uses: actions/checkout@v4", "      - uses: actions/checkout@v4\n        uses: unsafe/action@v1", 1),
		"step with unsafe then safe":      strings.Replace(compatibleWorkflow, "        with: {ref:", "        with: {unsafe: value}\n        with: {ref:", 1),
		"step with safe then unsafe":      strings.Replace(compatibleWorkflow, `        with: {ref: "${{ inputs.commit_sha }}"}`, "        with: {ref: \"${{ inputs.commit_sha }}\"}\n        with: {unsafe: value}", 1),
		"step env unsafe then safe":       strings.Replace(compatibleWorkflow, "      - env:\n", "      - env: {UNSAFE: value}\n        env:\n", 1),
		"step env safe then unsafe":       strings.Replace(compatibleWorkflow, "        run: |", "        env: {UNSAFE: value}\n        run: |", 1),
		"step run unsafe then safe":       strings.Replace(compatibleWorkflow, "        run: |", "        run: unsafe\n        run: |", 1),
		"step run safe then unsafe":       strings.Replace(compatibleWorkflow, "          test \"$(git rev-parse HEAD)\" = \"$CYCLOPUS_COMMIT_SHA\"", "          test \"$(git rev-parse HEAD)\" = \"$CYCLOPUS_COMMIT_SHA\"\n        run: unsafe", 1),
		"nested with unsafe then safe":    strings.Replace(compatibleWorkflow, `with: {ref: "${{ inputs.commit_sha }}"}`, `with: {ref: unsafe, ref: "${{ inputs.commit_sha }}"}`, 1),
		"nested with safe then unsafe":    strings.Replace(compatibleWorkflow, `with: {ref: "${{ inputs.commit_sha }}"}`, `with: {ref: "${{ inputs.commit_sha }}", ref: unsafe}`, 1),
		"nested env unsafe then safe":     strings.Replace(compatibleWorkflow, `CYCLOPUS_COMMIT_SHA: "${{ inputs.commit_sha }}"`, "CYCLOPUS_COMMIT_SHA: unsafe\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1),
		"nested env safe then unsafe":     strings.Replace(compatibleWorkflow, `CYCLOPUS_COMMIT_SHA: "${{ inputs.commit_sha }}"`, "CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"\n          CYCLOPUS_COMMIT_SHA: unsafe", 1),
	}
	for name, workflow := range tests {
		t.Run(name, func(t *testing.T) {
			got := AnalyzeGitHubWorkflow([]byte(workflow), "ghcr.io/acme/api")
			if got.Compatible || len(got.Reasons) != 1 || got.Reasons[0] != "workflow_yaml_invalid" {
				t.Fatalf("analysis = %+v", got)
			}
		})
	}
}

func TestAnalyzeGitHubWorkflowRejectsParserEquivalentAndInvalidMappingKeys(t *testing.T) {
	tests := map[string]string{
		"alternate booleans": compatibleWorkflow + "extra: {true: first, TRUE: second}\n",
		"alternate decimals": compatibleWorkflow + "extra: {1: first, 01: second}\n",
		"alternate hex":      compatibleWorkflow + "extra: {1: first, 0x1: second}\n",
		"alternate nulls":    compatibleWorkflow + "extra: {null: first, ~: second}\n",
		"complex sequence":   compatibleWorkflow + "? [complex, key]\n: value\n",
		"complex mapping":    compatibleWorkflow + "? {complex: key}\n: value\n",
		"merge plain":        compatibleWorkflow + "extra: {<<: value}\n",
		"merge quoted":       compatibleWorkflow + "extra: {\"<<\": value}\n",
		"merge explicit tag": compatibleWorkflow + "extra: {!!str <<: value}\n",
		"merge alias":        compatibleWorkflow + "base: &base {safe: value}\nextra: {<<: *base}\n",
		"nested job":         strings.Replace(compatibleWorkflow, "    runs-on: ubuntu-latest", "    true: first\n    True: second\n    runs-on: ubuntu-latest", 1),
		"nested step":        strings.Replace(compatibleWorkflow, "      - uses: actions/checkout@v4", "      - 1: first\n        01: second\n        uses: actions/checkout@v4", 1),
	}
	for name, workflow := range tests {
		t.Run(name, func(t *testing.T) {
			got := AnalyzeGitHubWorkflow([]byte(workflow), "ghcr.io/acme/api")
			if got.Compatible || len(got.Reasons) != 1 || got.Reasons[0] != "workflow_yaml_invalid" {
				t.Fatalf("analysis = %+v", got)
			}
		})
	}
}

func TestAnalyzeGitHubWorkflowRejectsUnsupportedTopLevelKeys(t *testing.T) {
	for name, extra := range map[string]string{
		"quoted and boolean": "extra: {true: boolean, \"true\": string}\n",
		"distinct numbers":   "extra: {1: one, 2: two}\n",
		"on remains string":  "extra: {on: event, true: boolean}\n",
	} {
		t.Run(name, func(t *testing.T) {
			got := AnalyzeGitHubWorkflow([]byte(compatibleWorkflow+extra), "ghcr.io/acme/api")
			if got.Compatible || !contains(got.Reasons, "workflow_key_unsupported") {
				t.Fatalf("analysis = %+v", got)
			}
		})
	}
}

func TestAnalyzeGitHubWorkflowValidatesEntireWorkflowV1(t *testing.T) {
	checkout := "      - uses: actions/checkout@v4\n        with: {ref: \"${{ inputs.commit_sha }}\"}\n"
	guard := "      - env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\""
	build := "      - uses: docker/build-push-action@v6\n        with:\n          context: .\n          push: true\n          tags: ghcr.io/acme/api:${{ inputs.commit_sha }}\n"
	login := "      - uses: docker/login-action@v3\n        with:\n          registry: ghcr.io\n          username: ${{ github.actor }}\n          password: ${{ secrets.GITHUB_TOKEN }}\n"
	setup := "      - uses: docker/setup-buildx-action@v3\n"
	tests := map[string]string{
		"top env BASH_ENV":       strings.Replace(compatibleWorkflow, "jobs:\n", "env: {BASH_ENV: /tmp/pwn}\njobs:\n", 1),
		"job env BASH_ENV":       strings.Replace(compatibleWorkflow, "    runs-on:", "    env: {BASH_ENV: /tmp/pwn}\n    runs-on:", 1),
		"checkout env BASH_ENV":  strings.Replace(compatibleWorkflow, "      - uses: actions/checkout@v4", "      - env: {BASH_ENV: /tmp/pwn}\n        uses: actions/checkout@v4", 1),
		"login env BASH_ENV":     strings.Replace(compatibleWorkflow, guard, login+"        env: {BASH_ENV: /tmp/pwn}\n"+guard, 1),
		"setup env BASH_ENV":     strings.Replace(compatibleWorkflow, guard, setup+"        env: {BASH_ENV: /tmp/pwn}\n"+guard, 1),
		"build env BASH_ENV":     strings.Replace(compatibleWorkflow, "      - uses: docker/build-push-action@v6", "      - env: {BASH_ENV: /tmp/pwn}\n        uses: docker/build-push-action@v6", 1),
		"guard extra env":        strings.Replace(compatibleWorkflow, "          CYCLOPUS_COMMIT_SHA:", "          BASH_ENV: /tmp/pwn\n          CYCLOPUS_COMMIT_SHA:", 1),
		"extra job":              compatibleWorkflow + "  other: {runs-on: ubuntu-latest, steps: []}\n",
		"step before checkout":   strings.Replace(compatibleWorkflow, checkout, "      - run: echo before\n"+checkout, 1),
		"step after build":       compatibleWorkflow + "      - run: echo after\n",
		"duplicate checkout":     strings.Replace(compatibleWorkflow, guard, checkout+guard, 1),
		"duplicate login":        strings.Replace(compatibleWorkflow, guard, login+login+guard, 1),
		"duplicate setup":        strings.Replace(compatibleWorkflow, guard, setup+setup+guard, 1),
		"duplicate build":        strings.Replace(compatibleWorkflow, build, build+build, 1),
		"second unguarded build": strings.Replace(compatibleWorkflow, guard, build+guard, 1),
		"second run":             strings.Replace(compatibleWorkflow, guard, "      - run: echo unsafe\n"+guard, 1),
		"reversed setup login":   strings.Replace(compatibleWorkflow, guard, setup+login+guard, 1),
		"unknown top key":        strings.Replace(compatibleWorkflow, "jobs:\n", "concurrency: deploy\njobs:\n", 1),
		"unknown job key":        strings.Replace(compatibleWorkflow, "    runs-on:", "    timeout-minutes: 10\n    runs-on:", 1),
		"permissions expression": strings.Replace(compatibleWorkflow, "packages: write", "packages: ${{ inputs.permission }}", 1),
	}
	for name, workflow := range tests {
		t.Run(name, func(t *testing.T) {
			if got := AnalyzeGitHubWorkflow([]byte(workflow), "ghcr.io/acme/api"); got.Compatible {
				t.Fatalf("analysis unexpectedly compatible: %+v", got)
			}
		})
	}
	valid := strings.Replace(compatibleWorkflow, guard, login+setup+guard, 1)
	if got := AnalyzeGitHubWorkflow([]byte(valid), "ghcr.io/acme/api"); !got.Compatible {
		t.Fatalf("exact optional sequence analysis = %+v", got)
	}
}

func TestAnalyzeGitHubWorkflowRequiresSameJobAndOrderedSteps(t *testing.T) {
	for name, workflow := range map[string]string{
		"reverse": strings.Replace(compatibleWorkflow,
			`      - uses: actions/checkout@v4
        with: {ref: "${{ inputs.commit_sha }}"}
      - env:
          CYCLOPUS_COMMIT_SHA: "${{ inputs.commit_sha }}"
        run: |
          [[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-fA-F]{40}$ ]]
          test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"`,
			`      - env:
          CYCLOPUS_COMMIT_SHA: "${{ inputs.commit_sha }}"
        run: |
          [[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-fA-F]{40}$ ]]
          test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"
      - uses: actions/checkout@v4
        with: {ref: "${{ inputs.commit_sha }}"}`, 1),
		"cross job": strings.Replace(compatibleWorkflow, "      - uses: docker/build-push-action@v6", "  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: docker/build-push-action@v6", 1),
	} {
		t.Run(name, func(t *testing.T) {
			got := AnalyzeGitHubWorkflow([]byte(workflow), "ghcr.io/acme/api")
			if got.Compatible || !(contains(got.Reasons, "checkout_guard_build_job_order_invalid") || contains(got.Reasons, "jobs_invalid_or_limit_exceeded")) {
				t.Fatalf("analysis = %+v", got)
			}
		})
	}
}

func TestAnalyzeGitHubWorkflowRejectsExecutionControlBypasses(t *testing.T) {
	checkout := "      - uses: actions/checkout@v4"
	guard := "      - env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\""
	build := "      - uses: docker/build-push-action@v6"
	tests := map[string]struct {
		workflow string
		reason   string
	}{
		"checkout continue true":    {strings.Replace(compatibleWorkflow, checkout, checkout+"\n        continue-on-error: true", 1), "checkout_execution_control_unsupported"},
		"checkout conditional":      {strings.Replace(compatibleWorkflow, checkout, checkout+"\n        if: success()", 1), "checkout_execution_control_unsupported"},
		"guard continue true":       {strings.Replace(compatibleWorkflow, guard, "      - continue-on-error: true\n        env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1), "guard_execution_control_unsupported"},
		"guard continue false":      {strings.Replace(compatibleWorkflow, guard, "      - continue-on-error: false\n        env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1), "guard_execution_control_unsupported"},
		"guard continue expression": {strings.Replace(compatibleWorkflow, guard, "      - continue-on-error: ${{ inputs.allow_failure }}\n        env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1), "guard_execution_control_unsupported"},
		"guard if always":           {strings.Replace(compatibleWorkflow, guard, "      - if: always()\n        env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1), "guard_execution_control_unsupported"},
		"guard if success":          {strings.Replace(compatibleWorkflow, guard, "      - if: success()\n        env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1), "guard_execution_control_unsupported"},
		"guard if expression":       {strings.Replace(compatibleWorkflow, guard, "      - if: ${{ inputs.run_guard }}\n        env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1), "guard_execution_control_unsupported"},
		"guard shell sh":            {strings.Replace(compatibleWorkflow, guard, "      - shell: sh\n        env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1), "guard_execution_control_unsupported"},
		"guard shell custom bash":   {strings.Replace(compatibleWorkflow, guard, "      - shell: bash -e {0}\n        env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1), "guard_execution_control_unsupported"},
		"build continue false":      {strings.Replace(compatibleWorkflow, build, build+"\n        continue-on-error: false", 1), "build_execution_control_unsupported"},
		"build if failure":          {strings.Replace(compatibleWorkflow, build, build+"\n        if: failure()", 1), "build_execution_control_unsupported"},
		"build run plus uses":       {strings.Replace(compatibleWorkflow, build, build+"\n        run: echo bypass", 1), "build_execution_control_unsupported"},
		"job continue true":         {strings.Replace(compatibleWorkflow, "    runs-on:", "    continue-on-error: true\n    runs-on:", 1), "job_execution_control_unsupported"},
		"job continue false":        {strings.Replace(compatibleWorkflow, "    runs-on:", "    continue-on-error: false\n    runs-on:", 1), "job_execution_control_unsupported"},
		"job continue expression":   {strings.Replace(compatibleWorkflow, "    runs-on:", "    continue-on-error: ${{ inputs.allow_failure }}\n    runs-on:", 1), "job_execution_control_unsupported"},
		"job if":                    {strings.Replace(compatibleWorkflow, "    runs-on:", "    if: always()\n    runs-on:", 1), "job_execution_control_unsupported"},
		"job defaults":              {strings.Replace(compatibleWorkflow, "    runs-on:", "    defaults: {run: {shell: sh}}\n    runs-on:", 1), "job_execution_control_unsupported"},
		"job container":             {strings.Replace(compatibleWorkflow, "    runs-on:", "    container: alpine:3.20\n    runs-on:", 1), "job_execution_control_unsupported"},
		"job services":              {strings.Replace(compatibleWorkflow, "    runs-on:", "    services: {db: {image: postgres:16}}\n    runs-on:", 1), "job_execution_control_unsupported"},
		"job environment":           {strings.Replace(compatibleWorkflow, "    runs-on:", "    environment: production\n    runs-on:", 1), "job_execution_control_unsupported"},
		"workflow defaults":         {strings.Replace(compatibleWorkflow, "jobs:\n", "defaults: {run: {shell: sh}}\njobs:\n", 1), "workflow_defaults_unsupported"},
		"windows runner":            {strings.Replace(compatibleWorkflow, "ubuntu-latest", "windows-latest", 1), "runner_unsupported"},
		"macos runner":              {strings.Replace(compatibleWorkflow, "ubuntu-latest", "macos-latest", 1), "runner_unsupported"},
		"self hosted runner":        {strings.Replace(compatibleWorkflow, "ubuntu-latest", "self-hosted", 1), "runner_unsupported"},
		"runner array":              {strings.Replace(compatibleWorkflow, "ubuntu-latest", "[self-hosted, linux]", 1), "runner_unsupported"},
		"dynamic runner":            {strings.Replace(compatibleWorkflow, "ubuntu-latest", "${{ inputs.runner }}", 1), "runner_unsupported"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := AnalyzeGitHubWorkflow([]byte(test.workflow), "ghcr.io/acme/api")
			if got.Compatible || !contains(got.Reasons, test.reason) {
				t.Fatalf("analysis = %+v, want %s", got, test.reason)
			}
		})
	}
}

func TestAnalyzeGitHubWorkflowRejectsBuildBeforeGuardEvenWithLaterValidBuild(t *testing.T) {
	earlyBuild := `      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ghcr.io/acme/api:${{ inputs.commit_sha }}
`
	workflow := strings.Replace(compatibleWorkflow, "      - env:\n", earlyBuild+"      - env:\n", 1)
	got := AnalyzeGitHubWorkflow([]byte(workflow), "ghcr.io/acme/api")
	if got.Compatible || !contains(got.Reasons, "checkout_guard_build_job_order_invalid") {
		t.Fatalf("analysis = %+v", got)
	}
}

func TestAnalyzeGitHubWorkflowWorkspaceIntegritySequence(t *testing.T) {
	guard := "      - env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\""
	approved := `      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/setup-buildx-action@v3
`
	valid := strings.Replace(compatibleWorkflow, guard, approved+guard, 1)
	if got := AnalyzeGitHubWorkflow([]byte(valid), "ghcr.io/acme/api"); !got.Compatible {
		t.Fatalf("approved setup sequence analysis = %+v", got)
	}
	tests := map[string]string{
		"guard build not adjacent": strings.Replace(compatibleWorkflow, "      - uses: docker/build-push-action@v6", "      - uses: docker/setup-buildx-action@v3\n      - uses: docker/build-push-action@v6", 1),
		"second checkout":          strings.Replace(compatibleWorkflow, guard, "      - uses: actions/checkout@v4\n        with: {ref: \"${{ inputs.commit_sha }}\"}\n"+guard, 1),
		"mutating run":             strings.Replace(compatibleWorkflow, guard, "      - run: rm -rf .git\n"+guard, 1),
		"unrecognized action":      strings.Replace(compatibleWorkflow, guard, "      - uses: acme/setup@v1\n"+guard, 1),
	}
	for name, workflow := range tests {
		t.Run(name, func(t *testing.T) {
			got := AnalyzeGitHubWorkflow([]byte(workflow), "ghcr.io/acme/api")
			if got.Compatible || !contains(got.Reasons, "checkout_guard_build_job_order_invalid") {
				t.Fatalf("analysis = %+v", got)
			}
		})
	}
}

func TestAnalyzeGitHubWorkflowRejectsUnsafeDockerActionInputs(t *testing.T) {
	guard := "      - env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\""
	login := `      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
`
	setup := "      - uses: docker/setup-buildx-action@v3\n"
	withOptionalActions := strings.Replace(compatibleWorkflow, guard, login+setup+guard, 1)
	if got := AnalyzeGitHubWorkflow([]byte(withOptionalActions), "ghcr.io/acme/api"); !got.Compatible {
		t.Fatalf("valid optional action sequence analysis = %+v", got)
	}

	tests := map[string]string{
		"login registry":      strings.Replace(withOptionalActions, "registry: ghcr.io", "registry: docker.io", 1),
		"login username":      strings.Replace(withOptionalActions, "username: ${{ github.actor }}", "username: attacker", 1),
		"login password":      strings.Replace(withOptionalActions, "password: ${{ secrets.GITHUB_TOKEN }}", "password: ${{ secrets.OTHER_TOKEN }}", 1),
		"login extra key":     strings.Replace(withOptionalActions, "          password: ${{ secrets.GITHUB_TOKEN }}", "          password: ${{ secrets.GITHUB_TOKEN }}\n          logout: false", 1),
		"setup endpoint":      strings.Replace(withOptionalActions, setup, "      - uses: docker/setup-buildx-action@v3\n        with: {endpoint: tcp://attacker:1234}\n", 1),
		"setup driver":        strings.Replace(withOptionalActions, setup, "      - uses: docker/setup-buildx-action@v3\n        with: {driver: remote}\n", 1),
		"setup config":        strings.Replace(withOptionalActions, setup, "      - uses: docker/setup-buildx-action@v3\n        with: {config: /tmp/buildkitd.toml}\n", 1),
		"setup empty with":    strings.Replace(withOptionalActions, setup, "      - uses: docker/setup-buildx-action@v3\n        with: {}\n", 1),
		"multiline tags":      strings.Replace(compatibleWorkflow, "          tags: ghcr.io/acme/api:${{ inputs.commit_sha }}", "          tags: |\n            ghcr.io/acme/api:${{ inputs.commit_sha }}\n            ghcr.io/acme/api:latest", 1),
		"build unknown input": strings.Replace(compatibleWorkflow, "          tags: ghcr.io/acme/api:${{ inputs.commit_sha }}", "          tags: ghcr.io/acme/api:${{ inputs.commit_sha }}\n          labels: org.opencontainers.image.source=attacker", 1),
	}
	for _, key := range []string{"file", "target", "outputs", "secrets", "secret-envs", "build-args", "build-contexts", "platforms", "cache", "provenance"} {
		tests["build "+key] = strings.Replace(compatibleWorkflow, "          tags: ghcr.io/acme/api:${{ inputs.commit_sha }}", "          tags: ghcr.io/acme/api:${{ inputs.commit_sha }}\n          "+key+": unsafe", 1)
	}
	for name, workflow := range tests {
		t.Run(name, func(t *testing.T) {
			if got := AnalyzeGitHubWorkflow([]byte(workflow), "ghcr.io/acme/api"); got.Compatible {
				t.Fatalf("analysis unexpectedly compatible: %+v", got)
			}
		})
	}
}

func TestAnalyzeGitHubWorkflowRejectsUnexpectedStepKeys(t *testing.T) {
	guard := "      - env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\""
	tests := map[string]string{
		"checkout id":               strings.Replace(compatibleWorkflow, "      - uses: actions/checkout@v4", "      - id: checkout\n        uses: actions/checkout@v4", 1),
		"guard working directory":   strings.Replace(compatibleWorkflow, guard, "      - working-directory: /tmp\n        env:\n          CYCLOPUS_COMMIT_SHA: \"${{ inputs.commit_sha }}\"", 1),
		"build timeout minutes":     strings.Replace(compatibleWorkflow, "      - uses: docker/build-push-action@v6", "      - timeout-minutes: 1\n        uses: docker/build-push-action@v6", 1),
		"build continue misspelled": strings.Replace(compatibleWorkflow, "      - uses: docker/build-push-action@v6", "      - continue_on_error: true\n        uses: docker/build-push-action@v6", 1),
	}
	for name, workflow := range tests {
		t.Run(name, func(t *testing.T) {
			if got := AnalyzeGitHubWorkflow([]byte(workflow), "ghcr.io/acme/api"); got.Compatible {
				t.Fatalf("analysis unexpectedly compatible: %+v", got)
			}
		})
	}
}

func TestCommitSHAGuardStrictGrammar(t *testing.T) {
	envYAML := func(value string) *yaml.Node {
		var document yaml.Node
		if err := yaml.Unmarshal([]byte("env:\n  CYCLOPUS_COMMIT_SHA: \""+value+"\"\n"), &document); err != nil {
			t.Fatal(err)
		}
		return mappingValue(document.Content[0], "env")
	}
	env := envYAML(`${{ inputs.commit_sha }}`)
	accepted := []string{
		`[[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-fA-F]{40}$ ]]` + "\n" + `test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"`,
		`[[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-fA-F]{40}$ ]]` + "\n" + `[ "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA" ]`,
		"set -e\n" + `[[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-fA-F]{40}$ ]]` + "\n" + `test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"`,
	}
	for _, run := range accepted {
		if !validCommitSHAGuard(env, run) {
			t.Fatalf("guard rejected: %q", run)
		}
	}
	for _, run := range []string{
		`test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"`,
		`[[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-f]{40}$ ]]` + "\n" + `test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"`,
		`[[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-fA-F]{40}$ ]]` + "\n" + `test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA" || true`,
		"# check\n" + `[[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-fA-F]{40}$ ]]` + "\n" + `test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"`,
		`[[ "${{ inputs.commit_sha }}" =~ ^[0-9a-fA-F]{40}$ ]]` + "\n" + `test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"`,
	} {
		if validCommitSHAGuard(env, run) {
			t.Fatalf("guard accepted: %q", run)
		}
	}
	if validCommitSHAGuard(nil, accepted[0]) || validCommitSHAGuard(envYAML(`${{ github.event.issue.title }}`), accepted[0]) {
		t.Fatal("guard accepted unsafe or missing environment mapping")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
