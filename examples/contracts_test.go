package examples_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func exampleFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func requireText(t *testing.T, text string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(text, value) {
			t.Errorf("missing contract text %q", value)
		}
	}
}

func requireOrder(t *testing.T, text string, values ...string) {
	t.Helper()
	position := -1
	for _, value := range values {
		next := strings.Index(text, value)
		if next <= position {
			t.Fatalf("%q is missing or out of order", value)
		}
		position = next
	}
}

func TestAAPDurableEvidenceAndRequestContracts(t *testing.T) {
	capture := exampleFile(t, "ansible/capture.yml")
	requireText(t, capture,
		"when: not capture_cache.stat.exists", "lock_key | default('') | length > 0",
		`capture_request_id: "capture-{{ maintenance_id | hash('sha256') }}"`,
		`method: PUT`, `body: "{{ capture_submission }}"`, `status_code: [200, 201, 409, 500, 503]`)
	requireOrder(t, capture,
		"Save discovery before calling Lazarus", "Load the saved discovery document",
		"Prepare the saved document for this request and any retry", "Save the discovery document in the AAP job output",
		"Submit capture from the saved file")

	maintenance := exampleFile(t, "ansible/maintenance_state.yml")
	requireText(t, maintenance,
		"'new', 'captured', 'stopping'", "maintenance_get.json.maintenance.state == 'failed'",
		"awx_job_id | default('local')", "maintenance_get.etag", "maintenance_state) | hash('sha256')",
		`method: PATCH`, `If-Match: "{{ maintenance_get.etag }}"`,
		"maintenance_reconcile.json.maintenance.state == maintenance_state", "not in [500, 503]")

	target := exampleFile(t, "ansible/target_state.yml")
	requireText(t, target,
		"target_state == 'skipped'", "lazarus_admin_token | length > 0", "target_get.etag",
		"target_state) | hash('sha256')", `method: PATCH`, `If-Match: "{{ target_get.etag }}"`,
		"target_reconcile.json.state == target_state", "not in [500, 503]")

	observation := exampleFile(t, "ansible/observe.yml")
	requireText(t, observation,
		"awx_job_id | default('local')", "observation_target.etag", "observation.check) | hash('sha256')",
		`method: POST`, `If-Match: "{{ observation_target.etag }}"`,
		`body: "{{ observation }}"`, `status_code: [204, 409, 412, 500, 503]`)
}

func TestAAPFailureAndResumeTopology(t *testing.T) {
	workflow := exampleFile(t, "aap/workflow.yml")
	blocks := strings.Split(workflow, "\n      - identifier: ")
	identifiers := make(map[string]bool, len(blocks)-1)
	for _, block := range blocks[1:] {
		identifier := strings.TrimSpace(strings.SplitN(block, "\n", 2)[0])
		identifiers[identifier] = true
		if identifier != "create" && identifier != "mark_failed" && !strings.Contains(block, "failure_nodes: [mark_failed]") {
			t.Errorf("workflow node %s has no failure path", identifier)
		}
	}
	for _, match := range regexp.MustCompile(`(?:success|failure)_nodes: \[([^]]+)\]`).FindAllStringSubmatch(workflow, -1) {
		for _, reference := range strings.Split(match[1], ",") {
			if !identifiers[strings.TrimSpace(reference)] {
				t.Errorf("workflow references missing node %q", reference)
			}
		}
	}
	requireText(t, workflow,
		"maintenance_state: failed", "Reopen this same maintenance and resume without rediscovery",
		"name: Lazarus - reopen failed maintenance", `credentials: ["{{ lazarus_admin_credential }}"]`,
		"ask_variables_on_launch: true")
	readme := exampleFile(t, "aap/README.md")
	requireText(t, readme,
		"Do not relaunch the whole workflow", "same `maintenance_id`", "without create or capture",
		"Parallelize only targets with different `lock_key` values", "Chain targets sharing a lock key")
}
