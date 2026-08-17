package service

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	maxWorkflowBytes      = 256 << 10
	maxWorkflowDepth      = 24
	maxWorkflowSteps      = 128
	maxWorkflowExpression = 512
)

type WorkflowAnalysis struct {
	Compatible bool
	Reasons    []string
}

func AnalyzeGitHubWorkflow(content []byte, imageRepository string) WorkflowAnalysis {
	reasons := map[string]bool{}
	add := func(reason string) { reasons[reason] = true }
	if len(content) == 0 || len(content) > maxWorkflowBytes {
		add("workflow_too_large")
		return orderedWorkflowAnalysis(reasons)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil || len(document.Content) != 1 {
		add("workflow_yaml_invalid")
		return orderedWorkflowAnalysis(reasons)
	}
	if invalidYAMLMappingKeys(document.Content[0]) {
		add("workflow_yaml_invalid")
		return orderedWorkflowAnalysis(reasons)
	}
	if invalidYAMLNode(document.Content[0], 0) {
		add("workflow_yaml_unsupported")
		return orderedWorkflowAnalysis(reasons)
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		add("workflow_top_level_invalid")
		return orderedWorkflowAnalysis(reasons)
	}
	if !mappingKeysAllowed(root, "name", "run-name", "on", "permissions", "jobs") {
		add("workflow_key_unsupported")
	}
	if !validWorkflowPermissions(mappingValue(root, "permissions")) {
		add("workflow_permissions_invalid")
	}
	if mappingValue(root, "env") != nil {
		add("workflow_env_unsupported")
	}
	on := mappingValue(root, "on")
	if on == nil {
		add("workflow_dispatch_missing")
	} else {
		dispatch := workflowDispatch(on)
		if dispatch == nil || on.Kind != yaml.MappingNode || !mappingKeysAllowed(on, "workflow_dispatch") {
			add("workflow_dispatch_missing")
		} else {
			if dispatch.Kind != yaml.MappingNode || !mappingKeysAllowed(dispatch, "inputs") {
				add("workflow_dispatch_inputs_invalid")
			}
			inputs := mappingValue(dispatch, "inputs")
			if inputs == nil || inputs.Kind != yaml.MappingNode || !mappingKeysAllowed(inputs, "orchestration_job_id", "commit_sha") {
				add("workflow_dispatch_inputs_invalid")
			}
			for _, name := range []string{"orchestration_job_id", "commit_sha"} {
				input := mappingValue(inputs, name)
				if input == nil || input.Kind != yaml.MappingNode || !mappingKeysAllowed(input, "type", "required") || scalar(mappingValue(input, "type")) != "string" || scalar(mappingValue(input, "required")) != "true" {
					add("workflow_dispatch_inputs_invalid")
				}
			}
		}
		if mappingValue(on, "workflow_call") != nil {
			add("reusable_workflow_unsupported")
		}
	}
	if scalar(mappingValue(root, "run-name")) != `cyclopus:${{ inputs.orchestration_job_id }}` {
		add("run_name_invalid")
	}
	if mappingValue(root, "defaults") != nil {
		add("workflow_defaults_unsupported")
	}
	jobs := mappingValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode || len(jobs.Content) != 2 {
		add("jobs_invalid_or_limit_exceeded")
		return orderedWorkflowAnalysis(reasons)
	}
	job := jobs.Content[1]
	if job.Kind != yaml.MappingNode || mappingValue(job, "uses") != nil || mappingValue(job, "strategy") != nil {
		add("workflow_indirection_unsupported")
		return orderedWorkflowAnalysis(reasons)
	}
	if !mappingKeysAllowed(job, "name", "runs-on", "steps") {
		add("job_key_unsupported")
	}
	if mappingValue(job, "env") != nil {
		add("job_env_unsupported")
	}
	if mappingValue(job, "continue-on-error") != nil || mappingValue(job, "if") != nil || mappingValue(job, "defaults") != nil || mappingValue(job, "container") != nil || mappingValue(job, "services") != nil || mappingValue(job, "environment") != nil {
		add("job_execution_control_unsupported")
	}
	if !supportedLinuxRunner(mappingValue(job, "runs-on")) {
		add("runner_unsupported")
	}
	steps := mappingValue(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		add("checkout_guard_build_job_order_invalid")
		return orderedWorkflowAnalysis(reasons)
	}
	if len(steps.Content) > maxWorkflowSteps {
		add("steps_limit_exceeded")
	}
	for _, step := range steps.Content {
		uses := scalar(mappingValue(step, "uses"))
		switch {
		case validActionUse(uses, "actions/checkout", "v4") && validCheckoutInputs(mappingValue(step, "with")):
			if actionStepExecutionControlUnsupported(step) {
				add("checkout_execution_control_unsupported")
			}
		case validActionUse(uses, "docker/build-push-action", "v6"):
			if actionStepExecutionControlUnsupported(step) {
				add("build_execution_control_unsupported")
			}
		case uses == "" && validCommitSHAGuard(mappingValue(step, "env"), scalar(mappingValue(step, "run"))):
			if runStepExecutionControlUnsupported(step) {
				add("guard_execution_control_unsupported")
			}
		}
	}
	if !validExactWorkflowSteps(steps, imageRepository, add) {
		add("checkout_guard_build_job_order_invalid")
	}
	return orderedWorkflowAnalysis(reasons)
}

func invalidYAMLMappingKeys(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.MappingNode {
		keys := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Value == "<<" {
				return true
			}
			canonical, ok := canonicalYAMLScalarKey(key)
			if !ok {
				return true
			}
			if _, exists := keys[canonical]; exists {
				return true
			}
			keys[canonical] = struct{}{}
		}
		if len(node.Content)%2 != 0 {
			return true
		}
	}
	for _, child := range node.Content {
		if invalidYAMLMappingKeys(child) {
			return true
		}
	}
	return false
}

func canonicalYAMLScalarKey(node *yaml.Node) (string, bool) {
	var value any
	if err := node.Decode(&value); err != nil {
		return "", false
	}
	return node.ShortTag() + "\x00" + fmt.Sprintf("%T:%v", value, value), true
}

func validActionUse(value, repository, version string) bool {
	want := repository + "@" + version
	return value == want
}

func supportedLinuxRunner(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Value == "ubuntu-latest"
}

func validCheckoutInputs(with *yaml.Node) bool {
	return with != nil && with.Kind == yaml.MappingNode && len(with.Content) == 2 && with.Content[0].Value == "ref" && scalar(with.Content[1]) == `${{ inputs.commit_sha }}`
}

func actionStepExecutionControlUnsupported(step *yaml.Node) bool {
	return mappingValue(step, "continue-on-error") != nil || mappingValue(step, "if") != nil || mappingValue(step, "shell") != nil || mappingValue(step, "run") != nil || mappingValue(step, "env") != nil
}

func runStepExecutionControlUnsupported(step *yaml.Node) bool {
	return mappingValue(step, "continue-on-error") != nil || mappingValue(step, "if") != nil || mappingValue(step, "shell") != nil
}

func validExactWorkflowSteps(steps *yaml.Node, imageRepository string, add func(string)) bool {
	if len(steps.Content) < 3 || len(steps.Content) > 5 {
		return false
	}
	if !validCheckoutStep(steps.Content[0]) {
		add("checkout_commit_sha_missing")
		return false
	}
	index := 1
	if index < len(steps.Content) && validActionUse(scalar(mappingValue(steps.Content[index], "uses")), "docker/login-action", "v3") {
		if !validLoginStep(steps.Content[index]) {
			return false
		}
		index++
	}
	if index < len(steps.Content) && validActionUse(scalar(mappingValue(steps.Content[index], "uses")), "docker/setup-buildx-action", "v3") {
		if !validSetupBuildxStep(steps.Content[index]) {
			return false
		}
		index++
	}
	if index >= len(steps.Content) || !validGuardStep(steps.Content[index]) {
		add("commit_sha_guard_missing")
		return false
	}
	index++
	if index >= len(steps.Content) || !validBuildStep(steps.Content[index], imageRepository, add) {
		add("build_push_tag_invalid")
		return false
	}
	return index == len(steps.Content)-1
}

func validCheckoutStep(step *yaml.Node) bool {
	return step != nil && step.Kind == yaml.MappingNode && mappingKeysAllowed(step, "name", "uses", "with") &&
		validActionUse(scalar(mappingValue(step, "uses")), "actions/checkout", "v4") && validCheckoutInputs(mappingValue(step, "with"))
}

func validLoginStep(step *yaml.Node) bool {
	if step == nil || step.Kind != yaml.MappingNode || !mappingKeysAllowed(step, "name", "uses", "with") {
		return false
	}
	with := mappingValue(step, "with")
	return with != nil && with.Kind == yaml.MappingNode && len(with.Content) == 6 &&
		mappingKeysAllowed(with, "registry", "username", "password") &&
		scalar(mappingValue(with, "registry")) == "ghcr.io" &&
		scalar(mappingValue(with, "username")) == `${{ github.actor }}` &&
		scalar(mappingValue(with, "password")) == `${{ secrets.GITHUB_TOKEN }}`
}

func validSetupBuildxStep(step *yaml.Node) bool {
	return step != nil && step.Kind == yaml.MappingNode && mappingKeysAllowed(step, "name", "uses")
}

func validGuardStep(step *yaml.Node) bool {
	return step != nil && step.Kind == yaml.MappingNode && mappingKeysAllowed(step, "name", "env", "run") &&
		validCommitSHAGuard(mappingValue(step, "env"), scalar(mappingValue(step, "run")))
}

func validBuildStep(step *yaml.Node, imageRepository string, add func(string)) bool {
	if step == nil || step.Kind != yaml.MappingNode || !mappingKeysAllowed(step, "name", "uses", "with") ||
		!validActionUse(scalar(mappingValue(step, "uses")), "docker/build-push-action", "v6") {
		return false
	}
	with := mappingValue(step, "with")
	push, tags, context := scalar(mappingValue(with, "push")), scalar(mappingValue(with, "tags")), scalar(mappingValue(with, "context"))
	wantedSuffix := `:${{ inputs.commit_sha }}`
	if with == nil || with.Kind != yaml.MappingNode || len(with.Content) != 6 || !mappingKeysAllowed(with, "context", "push", "tags") ||
		context != "." || push != "true" || strings.ContainsAny(tags, "\r\n") || !strings.HasSuffix(tags, wantedSuffix) || strings.Contains(strings.TrimSuffix(tags, wantedSuffix), "${{") {
		return false
	}
	if imageRepository != "" && tags != imageRepository+wantedSuffix {
		add("image_repository_mismatch")
		return false
	}
	return true
}

func mappingKeysAllowed(node *yaml.Node, allowed ...string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	keys := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		keys[key] = struct{}{}
	}
	for index := 0; index < len(node.Content); index += 2 {
		if _, ok := keys[node.Content[index].Value]; !ok {
			return false
		}
	}
	return true
}

func validWorkflowPermissions(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.MappingNode && len(node.Content) == 4 &&
		scalar(mappingValue(node, "contents")) == "read" && scalar(mappingValue(node, "packages")) == "write" &&
		mappingKeysAllowed(node, "contents", "packages")
}

func validCommitSHAGuard(env *yaml.Node, run string) bool {
	if env == nil || env.Kind != yaml.MappingNode || len(env.Content) != 2 || env.Content[0].Value != "CYCLOPUS_COMMIT_SHA" || scalar(env.Content[1]) != `${{ inputs.commit_sha }}` {
		return false
	}
	if len(run) == 0 || len(run) > maxWorkflowExpression || strings.Contains(run, `${{`) {
		return false
	}
	run = strings.TrimSuffix(run, "\n")
	lines := strings.Split(run, "\n")
	if len(lines) == 3 && lines[0] == "set -e" {
		lines = lines[1:]
	}
	if len(lines) != 2 {
		return false
	}
	if lines[0] != `[[ "$CYCLOPUS_COMMIT_SHA" =~ ^[0-9a-fA-F]{40}$ ]]` {
		return false
	}
	return lines[1] == `test "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA"` || lines[1] == `[ "$(git rev-parse HEAD)" = "$CYCLOPUS_COMMIT_SHA" ]`
}

func invalidYAMLNode(node *yaml.Node, depth int) bool {
	if node == nil || depth > maxWorkflowDepth || node.Kind == yaml.AliasNode || node.Anchor != "" || len(node.Value) > maxWorkflowExpression && strings.Contains(node.Value, "${{") {
		return true
	}
	for _, child := range node.Content {
		if invalidYAMLNode(child, depth+1) {
			return true
		}
	}
	return false
}

func workflowDispatch(on *yaml.Node) *yaml.Node {
	if on.Kind == yaml.ScalarNode && on.Value == "workflow_dispatch" {
		return &yaml.Node{Kind: yaml.MappingNode}
	}
	if on.Kind != yaml.MappingNode {
		return nil
	}
	return mappingValue(on, "workflow_dispatch")
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func scalar(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func orderedWorkflowAnalysis(reasons map[string]bool) WorkflowAnalysis {
	values := make([]string, 0, len(reasons))
	for reason := range reasons {
		values = append(values, reason)
	}
	sort.Strings(values)
	return WorkflowAnalysis{Compatible: len(values) == 0, Reasons: values}
}

func validWorkflowPath(path string) bool {
	return len(path) <= 255 && regexp.MustCompile(`^\.github/workflows/[^/\s]+\.(?:yml|yaml)$`).MatchString(path)
}
