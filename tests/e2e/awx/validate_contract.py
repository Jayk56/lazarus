#!/usr/bin/env python3
"""Fail when the deterministic AWX fixture contract drifts."""

from pathlib import Path
import re

import yaml


ROOT = Path(__file__).parent


def load(path: Path):
    return yaml.safe_load(path.read_text(encoding="utf-8"))


fixture = load(ROOT / "fixture.yml")
targets = fixture["fixture_targets"]
target_ids = [target["target_id"] for target in targets]
assert target_ids == [
    "web-01",
    "web-02",
    "web-03",
    "web-04",
    "web-05",
    "service-01",
    "service-02",
    "service-03",
    "nodeagent",
    "dmgr",
]
assert len(target_ids) == len(set(target_ids)) == 10
assert [target["target_id"] for target in targets if target["initial_state"] == "stopped"] == [
    "web-04",
    "web-05",
]
assert fixture["fixture_stop_app_services"] == [
    "web-01",
    "web-02",
    "web-03",
    "service-01",
    "service-02",
    "service-03",
]
assert fixture["fixture_start_app_services"] == [
    "service-03",
    "service-02",
    "service-01",
    "web-03",
    "web-02",
    "web-01",
]
assert len(fixture["fixture_expected_transition_order"]) == 32
assert fixture["fixture_expected_transition_order"][:2] == ["web-01:healthy", "web-01:starting"]
assert fixture["fixture_expected_transition_order"][-2:] == ["web-01:stopped", "web-01:stopping"]
assert fixture["fixture_expected_profiles"] == {
    "happy": {
        "maintenance_state": "completed",
        "maintenance_version": 39,
        "maintenance_etag": '"39"',
        "running_target_state": "healthy",
        "running_target_version": 10,
        "stopped_target_state": "stopped",
        "stopped_target_version": 6,
        "event_count": 55,
        "maintenance_event_count": 7,
        "target_transition_count": 32,
        "observation_count": 16,
        "reopened_event_count": 0,
    },
    "recovery": {
        "maintenance_state": "completed",
        "maintenance_version": 41,
        "maintenance_etag": '"41"',
        "running_target_state": "healthy",
        "running_target_version": 12,
        "stopped_target_state": "stopped",
        "stopped_target_version": 8,
        "event_count": 57,
        "maintenance_event_count": 9,
        "target_transition_count": 32,
        "observation_count": 16,
        "reopened_event_count": 1,
    },
}
assert fixture["fixture_failure_expected"] == {
    "maintenance_state": "failed",
    "maintenance_version": 7,
    "maintenance_etag": '"7"',
    "event_count": 8,
    "maintenance_event_count": 4,
    "target_transition_count": 3,
    "observation_count": 1,
}
assert fixture["fixture_failure_expected_transition_order"] == [
    "web-02:stopping",
    "web-01:stopped",
    "web-01:stopping",
]

workflow_configuration = load(ROOT / "workflow.yml")
controller_templates = workflow_configuration["controller_templates"]
workflows = workflow_configuration["controller_workflows"]
workflow = workflows[0]
recovery_workflow = workflows[1]
assert len(controller_templates) == 9
assert all(template["ask_variables_on_launch"] is True for template in controller_templates)
assert (
    workflow_configuration["awx_e2e_awx_execution_environment"]
    == "Lazarus AWX EE (24.6.1)"
)
assert (
    workflow_configuration["awx_e2e_awx_execution_environment_image"]
    == "quay.io/ansible/awx-ee:24.6.1"
)
assert len(workflows) == 2
assert workflow["allow_simultaneous"] is False
nodes = {node["identifier"]: node for node in workflow["simplified_workflow_nodes"]}
happy_path = [
    "seed",
    "create",
    "discover",
    "capture",
    "maintenance_stopping",
    "stop_app_services",
    "stop_nodeagent",
    "stop_dmgr",
    "maintenance_stopped",
    "maintenance_waiting",
    "maintenance_starting",
    "start_dmgr",
    "start_nodeagent",
    "start_app_services",
    "complete",
    "assert_contract",
]
assert set(nodes) == set(happy_path) | {"mark_failed"}
for current, following in zip(happy_path, happy_path[1:]):
    assert nodes[current].get("success_nodes") == [following]
assert "success_nodes" not in nodes[happy_path[-1]]
for identifier in happy_path[2:-1]:
    assert nodes[identifier].get("failure_nodes") == ["mark_failed"]
for identifier in ("seed", "create", "assert_contract", "mark_failed"):
    assert "failure_nodes" not in nodes[identifier]

expected_node_data = {
    "maintenance_stopping": {"maintenance_state": "stopping"},
    "stop_app_services": {"fixture_group": "stop_app_services"},
    "stop_nodeagent": {"fixture_group": "stop_nodeagent"},
    "stop_dmgr": {"fixture_group": "stop_dmgr"},
    "maintenance_stopped": {"maintenance_state": "stopped"},
    "maintenance_waiting": {"maintenance_state": "waiting"},
    "maintenance_starting": {"maintenance_state": "starting"},
    "start_dmgr": {"fixture_group": "start_dmgr"},
    "start_nodeagent": {"fixture_group": "start_nodeagent"},
    "start_app_services": {"fixture_group": "start_app_services"},
    "complete": {"maintenance_state": "completed"},
    "mark_failed": {"maintenance_state": "failed"},
}
for identifier, extra_data in expected_node_data.items():
    assert nodes[identifier]["extra_data"] == extra_data

recovery_nodes = {
    node["identifier"]: node for node in recovery_workflow["simplified_workflow_nodes"]
}
recovery_success = {
    "seed": ["create"],
    "create": ["discover"],
    "discover": ["capture"],
    "capture": ["maintenance_stopping"],
    "maintenance_stopping": ["stop_app_services_injected"],
    "stop_app_services_injected": ["assert_failed"],
    "mark_failed_initial": ["assert_failed"],
    "assert_failed": ["reopen"],
    "reopen": ["resume_stop_app_services"],
    "resume_stop_app_services": ["stop_nodeagent"],
    "stop_nodeagent": ["stop_dmgr"],
    "stop_dmgr": ["maintenance_stopped"],
    "maintenance_stopped": ["maintenance_waiting"],
    "maintenance_waiting": ["maintenance_starting"],
    "maintenance_starting": ["start_dmgr"],
    "start_dmgr": ["start_nodeagent"],
    "start_nodeagent": ["start_app_services"],
    "start_app_services": ["complete"],
    "complete": ["assert_recovery"],
}
assert len(recovery_nodes) == 22
assert set(recovery_nodes) == set(recovery_success) | {
    "assert_recovery",
    "mark_failed_pre_injection",
    "mark_failed_recovery",
}
for identifier, success_nodes in recovery_success.items():
    assert recovery_nodes[identifier].get("success_nodes") == success_nodes
for identifier in (
    "assert_recovery",
    "mark_failed_pre_injection",
    "mark_failed_recovery",
):
    assert "success_nodes" not in recovery_nodes[identifier]

assert recovery_nodes["capture"].get("failure_nodes") == ["mark_failed_pre_injection"]
assert recovery_nodes["maintenance_stopping"].get("failure_nodes") == [
    "mark_failed_pre_injection"
]
assert recovery_nodes["stop_app_services_injected"].get("failure_nodes") == [
    "mark_failed_initial"
]
for identifier in (
    "resume_stop_app_services",
    "stop_nodeagent",
    "stop_dmgr",
    "maintenance_stopped",
    "maintenance_waiting",
    "maintenance_starting",
    "start_dmgr",
    "start_nodeagent",
    "start_app_services",
    "complete",
):
    assert recovery_nodes[identifier].get("failure_nodes") == ["mark_failed_recovery"]
for identifier in (
    "seed",
    "create",
    "discover",
    "mark_failed_pre_injection",
    "mark_failed_initial",
    "assert_failed",
    "reopen",
    "assert_recovery",
    "mark_failed_recovery",
):
    assert "failure_nodes" not in recovery_nodes[identifier]

assert recovery_nodes["stop_app_services_injected"]["extra_data"] == {
    "fixture_group": "stop_app_services",
    "fixture_failure_target_id": "web-02",
}
assert recovery_nodes["resume_stop_app_services"]["extra_data"] == {
    "fixture_group": "stop_app_services"
}
assert recovery_nodes["mark_failed_initial"]["extra_data"] == {
    "maintenance_state": "failed",
    "maintenance_request_scope": "failure-recovery-initial-failed",
}
assert recovery_nodes["mark_failed_pre_injection"]["extra_data"] == {
    "maintenance_state": "failed",
    "maintenance_request_scope": "failure-recovery-pre-injection-failed",
}
assert recovery_nodes["mark_failed_recovery"]["extra_data"] == {
    "maintenance_state": "failed",
    "maintenance_request_scope": "failure-recovery-terminal-failed",
}
assert recovery_nodes["assert_recovery"]["extra_data"] == {"assert_profile": "recovery"}
assert recovery_nodes["assert_failed"]["unified_job_template"] == (
    "Lazarus E2E - assert injected failure"
)
assert recovery_nodes["reopen"]["unified_job_template"] == (
    "Lazarus E2E - reopen failed maintenance"
)
recovery_survey = {
    question["variable"]: question for question in recovery_workflow["survey_spec"]["spec"]
}
assert recovery_survey["recovery_justification"]["required"] is True
assert recovery_survey["recovery_justification"]["min"] == 1

admin_credential = "{{ awx_e2e_admin_credential }}"
templates_using_admin = [
    template["name"]
    for template in controller_templates
    if admin_credential in template["credentials"]
]
assert templates_using_admin == ["Lazarus E2E - reopen failed maintenance"]
assert next(
    template
    for template in controller_templates
    if template["name"] == "Lazarus E2E - reopen failed maintenance"
)["credentials"] == [admin_credential]

kubernetes_documents = []
for path in sorted((ROOT / "kubernetes").glob("*.yaml")):
    kubernetes_documents.extend(document for document in yaml.safe_load_all(path.read_text()) if document)
assert all(document.get("kind") != "Secret" for document in kubernetes_documents)
assert {document["metadata"]["name"] for document in kubernetes_documents if document.get("kind") == "Service"} == {
    "node-server",
    "dmgr-server",
}

flow_paths = [
    ROOT / "playbooks" / "discover.yml",
    ROOT / "playbooks" / "capture.yml",
    ROOT / "playbooks" / "assert.yml",
    ROOT / "playbooks" / "failure_assert.yml",
    ROOT / "playbooks" / "reopen.yml",
    ROOT / "playbooks" / "maintenance.yml",
    ROOT / "playbooks" / "target_batch.yml",
    ROOT / "playbooks" / "tasks" / "target_once.yml",
    ROOT / "workflow.yml",
    ROOT / "configure.yml",
    ROOT / "configure-awx.yml",
    ROOT / "README.md",
]
flow_text = "\n".join(path.read_text(encoding="utf-8") for path in flow_paths)
assert "LAZARUS_CAPTURE_CACHE_DIR" not in flow_text
assert "capture_cache" not in flow_text
for path in (
    ROOT / "playbooks" / "discover.yml",
    ROOT / "playbooks" / "capture.yml",
    ROOT / "playbooks" / "assert.yml",
    ROOT / "playbooks" / "failure_assert.yml",
):
    assert "awx_e2e_capture_document" in path.read_text(encoding="utf-8")

discover_plays = load(ROOT / "playbooks" / "discover.yml")
publish_task = discover_plays[-1]["tasks"][-1]
assert publish_task["ansible.builtin.set_stats"] == {
    "data": {"awx_e2e_capture_document": "{{ capture_document }}"},
    "per_host": False,
    "aggregate": False,
}

playbook_text = "\n".join(
    path.read_text(encoding="utf-8") for path in sorted((ROOT / "playbooks").rglob("*.yml"))
)
environment_lookups = re.findall(r"lookup\('env', '([^']+)'\)", playbook_text)
assert set(environment_lookups) == {"LAZARUS_TOKEN", "LAZARUS_ADMIN_TOKEN"}
reopen_text = (ROOT / "playbooks" / "reopen.yml").read_text(encoding="utf-8")
assert "lookup('env', 'LAZARUS_ADMIN_TOKEN')" in reopen_text
assert "lookup('env', 'LAZARUS_TOKEN')" not in reopen_text
for ordinary_api_playbook in (
    "assert.yml",
    "capture.yml",
    "create.yml",
    "failure_assert.yml",
    "maintenance.yml",
    "target_batch.yml",
):
    ordinary_text = (ROOT / "playbooks" / ordinary_api_playbook).read_text(
        encoding="utf-8"
    )
    assert "LAZARUS_ADMIN_TOKEN" not in ordinary_text

for delegated_playbook in ("target_batch.yml", "assert.yml", "failure_assert.yml"):
    delegated_play = load(ROOT / "playbooks" / delegated_playbook)[0]
    assert "connection" not in delegated_play

assert "item.version == (expected_version | int)" in (
    ROOT / "playbooks" / "assert.yml"
).read_text(encoding="utf-8")
assert "event_page.json.items" not in (
    ROOT / "playbooks" / "assert.yml"
).read_text(encoding="utf-8")
assert "reopened_events[0].payload.justification == recovery_justification" in (
    ROOT / "playbooks" / "assert.yml"
).read_text(encoding="utf-8")

target_batch = load(ROOT / "playbooks" / "target_batch.yml")[0]
assert target_batch["vars"]["fixture_failure_target_id"] == ""
batch_assertions = next(
    task["ansible.builtin.assert"]["that"]
    for task in target_batch["tasks"]
    if task["name"] == "Validate batch inputs"
)
assert (
    "fixture_failure_target_id | length == 0 or fixture_failure_target_id in target_ids"
    in batch_assertions
)
assert (
    "fixture_failure_target_id | length == 0 or fixture_group == 'stop_app_services'"
    in batch_assertions
)

target_once = load(ROOT / "playbooks" / "tasks" / "target_once.yml")
target_once_names = [task["name"] for task in target_once]
enter_index = target_once_names.index("Enter the target phase with its exact ETag")
command_index = target_once_names.index("Apply the idempotent fixture operation over SSH")
finish_index = target_once_names.index("Finish the target phase using the observation ETag")
assert enter_index < command_index < finish_index
fixture_command = target_once[command_index]
assert "awx-e2e-unknown-service" in fixture_command["vars"][
    "fixture_operation_service"
]
assert "target_id == fixture_failure_target_id" in fixture_command["vars"][
    "fixture_operation_service"
]

maintenance_text = (ROOT / "playbooks" / "maintenance.yml").read_text(
    encoding="utf-8"
)
assert "maintenance_request_scope: standard" in maintenance_text
assert "maintenance_request_scope ~ '|'" in maintenance_text
assert "recovery_justification | default('') | trim | length > 0" in reopen_text
assert "If-Match: \"{{ failed_maintenance.etag }}\"" in reopen_text
assert "awx-e2e-reopen-" in reopen_text
assert "reopened_maintenance.json.maintenance.version == 8" in reopen_text
assert "reopened_maintenance.etag == '\"8\"'" in reopen_text

failure_assert_text = (ROOT / "playbooks" / "failure_assert.yml").read_text(
    encoding="utf-8"
)
for expected_failure_contract in (
    "fixture_failure_expected.maintenance_version",
    "failed_capture.json.payload == awx_e2e_capture_document",
    "failed_terminal_events[0].from_version == 6",
    "failed_terminal_events[0].to_version == 7",
    "failed_terminal_events[0].aggregate_version == 7",
    "failed_terminal_events[0].role == 'operator'",
    "item.item.target_id == 'web-01'",
):
    assert expected_failure_contract in failure_assert_text

requirements = load(ROOT / "collections" / "requirements.yml")
assert requirements == {"collections": [{"name": "awx.awx", "version": "24.6.1"}]}

awx_bootstrap = load(ROOT / "configure-awx.yml")[0]
awx_tasks = awx_bootstrap["tasks"]
awx_task_modules = [
    next(key for key in task if key.startswith("awx.awx."))
    for task in awx_tasks
]
assert awx_task_modules == [
    "awx.awx.organization",
    "awx.awx.execution_environment",
    "awx.awx.project",
    "awx.awx.inventory",
    "awx.awx.inventory_source",
    "awx.awx.inventory_source_update",
    "awx.awx.credential_type",
    "awx.awx.credential",
    "awx.awx.credential_type",
    "awx.awx.credential",
    "awx.awx.credential",
    "awx.awx.job_template",
    "awx.awx.workflow_job_template",
    "awx.awx.workflow_job_template_node",
    "awx.awx.workflow_job_template_node",
]

awx_modules = {
    module_name: task["awx.awx." + module_name]
    for module_name in (
        "organization",
        "execution_environment",
        "project",
        "inventory",
        "inventory_source",
        "inventory_source_update",
        "job_template",
        "workflow_job_template",
    )
    for task in awx_tasks
    if "awx.awx." + module_name in task
}
assert awx_modules["inventory_source"]["source"] == "scm"
assert awx_modules["inventory_source"]["source_path"] == "tests/e2e/awx/inventory.example.yml"
assert awx_modules["inventory_source_update"]["wait"] is True
assert awx_modules["project"]["update_project"] is True
credential_type_tasks = [
    task["awx.awx.credential_type"]
    for task in awx_tasks
    if "awx.awx.credential_type" in task
]
assert len(credential_type_tasks) == 2
assert all(task["kind"] == "cloud" for task in credential_type_tasks)
assert [task["injectors"]["env"] for task in credential_type_tasks] == [
    {"LAZARUS_TOKEN": "{% raw %}{{ lazarus_token }}{% endraw %}"},
    {"LAZARUS_ADMIN_TOKEN": "{% raw %}{{ lazarus_admin_token }}{% endraw %}"},
]
assert awx_modules["job_template"]["execution_environment"] == (
    "{{ awx_e2e_awx_execution_environment }}"
)
assert awx_modules["job_template"]["ask_variables_on_launch"] is True
assert awx_modules["workflow_job_template"]["survey_enabled"] == (
    "{{ item.survey_enabled }}"
)
job_template_task = next(
    task["awx.awx.job_template"]
    for task in awx_tasks
    if "awx.awx.job_template" in task
)
workflow_template_task = next(
    task["awx.awx.workflow_job_template"]
    for task in awx_tasks
    if "awx.awx.workflow_job_template" in task
)
assert job_template_task["credentials"] == "{{ item.credentials }}"
assert next(
    task["loop"] for task in awx_tasks if "awx.awx.job_template" in task
) == "{{ controller_templates }}"
assert workflow_template_task["name"] == "{{ item.name }}"
assert next(
    task["loop"] for task in awx_tasks if "awx.awx.workflow_job_template" in task
) == "{{ controller_workflows }}"

awx_node_tasks = [task for task in awx_tasks if "awx.awx.workflow_job_template_node" in task]
assert len(awx_node_tasks) == 2
create_node = awx_node_tasks[0]["awx.awx.workflow_job_template_node"]
link_node = awx_node_tasks[1]["awx.awx.workflow_job_template_node"]
assert "unified_job_template" in create_node
assert not {"success_nodes", "failure_nodes", "always_nodes"} & create_node.keys()
assert create_node["workflow_job_template"] == "{{ item.0.name }}"
assert create_node["identifier"] == "{{ item.1.identifier }}"
assert link_node["success_nodes"] == "{{ item.1.success_nodes | default([]) }}"
assert link_node["failure_nodes"] == "{{ item.1.failure_nodes | default([]) }}"
assert link_node["always_nodes"] == []
assert all(task.get("no_log") is True for task in awx_bootstrap["pre_tasks"])
credential_tasks = [task for task in awx_tasks if "awx.awx.credential" in task]
assert len(credential_tasks) == 3
assert all(task.get("no_log") is True for task in credential_tasks)

dockerfile = (ROOT / "fixture-image" / "Dockerfile").read_text(encoding="utf-8")
assert dockerfile.startswith(
    "FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818\n"
)
assert "find /var/lib/apt/lists -mindepth 1 -delete" in dockerfile
assert not re.search(r"(^|\s)(/[^\s]*/)?rm(\s|$)", dockerfile)
assert "passwd --delete awx-fixture" in dockerfile
assert "passwd --lock awx-fixture" not in dockerfile

entrypoint = (ROOT / "fixture-image" / "fixture-entrypoint.sh").read_text(encoding="utf-8")
assert entrypoint.index("/usr/local/bin/fixture-service seed") < entrypoint.index(
    'chown -R awx-fixture:awx-fixture "$state_dir"'
)

sshd_config = (ROOT / "fixture-image" / "sshd_config").read_text(encoding="utf-8")
assert "PasswordAuthentication no" in sshd_config
assert "PermitEmptyPasswords no" in sshd_config

print("Deterministic AWX fixture contract is internally consistent.")
