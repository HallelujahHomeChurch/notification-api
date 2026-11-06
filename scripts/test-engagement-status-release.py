#!/usr/bin/env python3
import copy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from engagement_status_release import DAPR_CONFIG, HOST, INGRESS, PROBE, SECRET, VAULT_SECRETS_URI, WRITE_SCOPES, build_transition, binding, require_preview, require_private_transport, require_caller_pins, require_serving, resolve_release_source, retire_dapr, write_binding


APP_ID = "/subscriptions/sub/resourceGroups/alive/providers/Microsoft.App/containerApps/notification-api"
IDENTITY = "/subscriptions/sub/resourceGroups/alive/providers/Microsoft.ManagedIdentity/userAssignedIdentities/notification-api-identity"
REVISION = "notification-api--current"
VERSION = "a" * 32


def fixture():
    return {
        "id": APP_ID, "name": "notification-api", "location": "eastasia",
        "identity": {"type": "UserAssigned", "userAssignedIdentities": {IDENTITY: {}}},
        "properties": {
            "latestRevisionName": REVISION, "latestReadyRevisionName": REVISION,
            "managedEnvironmentId": "/alive-env", "workloadProfileName": "Consumption",
            "configuration": {
                "ingress": None, "secrets": [], "activeRevisionsMode": "Single",
                "dapr": {"enabled": True, "appId": "notification-api", "appPort": 8081, "appProtocol": "http"},
            },
            "template": {
                "revisionSuffix": "current",
                "containers": [{"name": "notification-api", "image": "alive.azurecr.io/alive/notification-api@sha256:" + "b" * 64,
                                "env": [], "probes": [{"type": "Readiness"}]}],
                "scale": {"minReplicas": 1},
            },
        },
    }


class StatusReleaseTests(unittest.TestCase):
    def test_enable_and_disable_keep_application_configuration(self):
        app = fixture()
        original = copy.deepcopy(app)
        enabled = build_transition(app, "enable", VERSION, "status-enable")
        self.assertEqual(app, original)
        self.assertEqual(enabled["properties"]["configuration"]["ingress"], INGRESS)
        self.assertEqual(enabled["properties"]["template"]["containers"][0]["probes"][-1], PROBE)
        self.assertEqual(binding(enabled), (True, VERSION))
        enabled["properties"]["configuration"]["ingress"]["transport"] = "Auto"
        self.assertEqual(binding(enabled), (True, VERSION))
        self.assertEqual(enabled["properties"]["configuration"]["dapr"], app["properties"]["configuration"]["dapr"])
        disabled = build_transition(enabled, "disable", "", "status-disable")
        self.assertEqual(binding(disabled), (False, VERSION))
        self.assertIsNone(disabled["properties"]["configuration"]["ingress"])
        self.assertEqual(disabled["properties"]["template"]["containers"][0]["probes"], [{"type": "Readiness"}])

    def test_private_writes_require_existing_ingress_and_exact_credential_binding(self):
        app = build_transition(fixture(), "enable", VERSION, "status-enable")
        env_name, secret_name, version_var = WRITE_SCOPES[0]
        with patch.dict(os.environ, {version_var: VERSION}):
            write_binding(app, True)
            with self.assertRaises(ValueError):
                write_binding(app, False)
            with self.assertRaises(ValueError):
                write_binding(app, True, deployed=True)
            app["properties"]["template"]["containers"][0]["env"].append({"name": env_name, "secretRef": secret_name})
            secret = {"name": secret_name, "identity": IDENTITY,
                      "keyVaultUrl": VAULT_SECRETS_URI + secret_name + "/" + VERSION}
            app["properties"]["configuration"]["secrets"].append(secret)
            write_binding(app, True, deployed=True)
            with self.assertRaises(ValueError):
                build_transition(app, "disable", "", "status-disable")
            secret["identity"] = "/wrong-identity"
            with self.assertRaises(ValueError):
                write_binding(app, True)
        with self.assertRaises(ValueError):
            write_binding(app, True)

    def test_binding_and_snapshot_fail_closed(self):
        app = fixture()
        app["properties"]["configuration"]["secrets"] = [{"name": SECRET, "value": "plaintext"}]
        with self.assertRaises(ValueError):
            build_transition(app, "enable", VERSION, "status-enable")
        app = fixture()
        app["properties"]["template"]["containers"][0]["env"] = [{"name": "NEW_TOKEN", "value": "plaintext"}]
        with self.assertRaises(ValueError):
            build_transition(app, "enable", VERSION, "status-enable")
        app = fixture()
        app["properties"]["configuration"]["ingress"] = {"external": True, "targetPort": 8081}
        with self.assertRaises(ValueError):
            binding(app)

    def test_binding_accepts_arm_identity_casing_but_rejects_other_fields(self):
        app = build_transition(fixture(), "enable", VERSION, "status-enable")
        secrets = app["properties"]["configuration"]["secrets"]
        secrets[0]["identity"] = IDENTITY.lower()
        self.assertEqual(binding(app), (True, VERSION))
        secrets[0]["value"] = None
        with self.assertRaises(ValueError):
            binding(app)

    def test_retire_requires_every_private_scope_and_matching_consumer_pins(self):
        app = build_transition(fixture(), "enable", VERSION, "status-enable")
        app["properties"]["configuration"]["ingress"]["fqdn"] = HOST
        with self.assertRaises(ValueError):
            require_private_transport(app)
        with patch.dict(os.environ, {scope[2]: VERSION for scope in WRITE_SCOPES}):
            cfg = app["properties"]["configuration"]
            env = app["properties"]["template"]["containers"][0]["env"]
            for env_name, secret_name, _ in WRITE_SCOPES:
                env.append({"name": env_name, "secretRef": secret_name})
                cfg["secrets"].append({"name": secret_name, "identity": IDENTITY,
                                       "keyVaultUrl": VAULT_SECRETS_URI + secret_name + "/" + VERSION})
            pins = require_private_transport(app)
            self.assertEqual(len(pins), 3)
            consumer = copy.deepcopy(app)
            consumer["name"] = "account-api"
            consumer["properties"]["template"]["containers"][0]["name"] = "account-api"
            require_caller_pins(consumer, "account-api", pins[:1])
            next(entry for entry in consumer["properties"]["configuration"]["secrets"] if entry["name"] == pins[0][1])["keyVaultUrl"] += "wrong"
            with self.assertRaises(ValueError):
                require_caller_pins(consumer, "account-api", pins[:1])

    def test_retire_checks_exact_replica_drain(self):
        app = build_transition(fixture(), "enable", VERSION, "status-enable")
        app["properties"].update(latestRevisionName=REVISION, latestReadyRevisionName=REVISION)
        image = app["properties"]["template"]["containers"][0]["image"]
        ready = {"name": REVISION, "active": True, "health": "Healthy", "running": "Running",
                 "provisioned": "Provisioned", "images": [image]}
        with patch("engagement_status_release.az", side_effect=[ready, [REVISION]]):
            self.assertEqual(require_serving(app, "notification-api", REVISION), image)
        with patch("engagement_status_release.az", side_effect=[ready, [REVISION, "notification-api--old"]]):
            with self.assertRaisesRegex(ValueError, "not drained"):
                require_serving(app, "notification-api", REVISION)

    def test_retire_changes_only_api_and_restores_exact_dapr_after_failed_readback(self):
        original = build_transition(fixture(), "enable", VERSION, "status-enable")
        original["properties"].update(latestRevisionName=REVISION, latestReadyRevisionName=REVISION)
        cfg = original["properties"]["configuration"]
        cfg["ingress"]["fqdn"] = HOST
        cfg["dapr"] = copy.deepcopy(DAPR_CONFIG)
        env = original["properties"]["template"]["containers"][0]["env"]
        image = original["properties"]["template"]["containers"][0]["image"]
        versions = {scope[2]: VERSION for scope in WRITE_SCOPES}
        for env_name, secret_name, _ in WRITE_SCOPES:
            env.append({"name": env_name, "secretRef": secret_name})
            cfg["secrets"].append({"name": secret_name, "identity": IDENTITY,
                                   "keyVaultUrl": VAULT_SECRETS_URI + secret_name + "/" + VERSION})
        worker = {"properties": {"latestRevisionName": "notification-worker--ready", "latestReadyRevisionName": "notification-worker--ready",
                                 "configuration": {"dapr": {"enabled": False}}}}
        inputs = dict(versions, GITHUB_REF="refs/heads/main", GITHUB_REPOSITORY="HallelujahHomeChurch/notification-api",
                      RETIRE_DAPR_REVISIONS=REVISION + " account-api--current engagement-api--current")
        for bad_readback in (False, True):
            with self.subTest(bad_readback=bad_readback):
                api = copy.deepcopy(original)
                commands = []

                def state(name="notification-api"):
                    return copy.deepcopy(worker if name == "notification-worker" else api)

                def command(args):
                    self.assertEqual(args[0:3], ["az", "containerapp", "dapr"])
                    self.assertEqual(args[args.index("-n") + 1], "notification-api")
                    action = args[3]
                    commands.append(action)
                    if action == "disable":
                        api["properties"]["configuration"]["dapr"] = {"enabled": False}
                        if bad_readback:
                            api["properties"]["template"]["scale"]["minReplicas"] = 2
                    else:
                        self.assertEqual(args[args.index("--dapr-app-port") + 1], "8081")
                        api["properties"]["configuration"]["dapr"] = copy.deepcopy(DAPR_CONFIG)

                with (patch.dict(os.environ, inputs), patch("engagement_status_release.app_state", side_effect=state),
                      patch("engagement_status_release.require_serving", return_value=image),
                      patch("engagement_status_release.require_caller_pins"), patch("engagement_status_release.command", side_effect=command)):
                    if bad_readback:
                        with self.assertRaisesRegex(ValueError, "readback"):
                            retire_dapr()
                    else:
                        retire_dapr()
                self.assertEqual(commands, ["disable", "enable"] if bad_readback else ["disable"])
                self.assertEqual(api["properties"]["configuration"]["dapr"]["enabled"], bad_readback)

    def test_disable_preview_accepts_only_exact_provider_ingress_placeholder(self):
        live = build_transition(fixture(), "enable", VERSION, "status-enable")
        live["id"] = APP_ID
        desired = build_transition(live, "disable", "", "status-disable")
        after = copy.deepcopy(desired)
        placeholder = {"allowInsecure": False, "external": False, "transport": "Auto"}
        after["properties"]["configuration"]["ingress"] = placeholder
        preview = {"status": "Succeeded", "changes": [{
            "changeType": "Modify", "resourceId": APP_ID, "before": live, "after": after,
        }]}
        require_preview(preview, live, desired)
        for key, value in [("targetPort", 8081), ("targetPort", None), ("traffic", [{"weight": 100}]), ("traffic", [])]:
            placeholder[key] = value
            with self.assertRaises(ValueError):
                require_preview(preview, live, desired)
            del placeholder[key]

    def test_status_controls_are_excluded_after_runtime_script_glob(self):
        workflow = (Path(__file__).resolve().parents[1] / ".github/workflows/release.yml").read_text()
        push = workflow.split("  workflow_dispatch:", 1)[0]
        self.assertNotIn("- .github/workflows/release.yml", push)
        runtime_scripts = push.index("- scripts/**")
        for path in ("engagement_status_release.py", "test-engagement-status-release.py", "release-static.test.sh"):
            self.assertGreater(push.index(f"- '!scripts/{path}'"), runtime_scripts)
        for path in ("cmd/**", "internal/**", "infra/**"):
            self.assertIn(f"- {path}", push)
        self.assertIn("inputs.status_transition == '' && inputs.retire_dapr_revisions == ''", workflow)
        self.assertIn("python3 scripts/engagement_status_release.py retire-dapr", workflow)
        self.assertEqual(workflow.count('apiDaprEnabled="${API_DAPR_ENABLED}"'), 3)

    def test_recovery_requires_exact_predeployment_failure_and_tooling_only_diff(self):
        repo = "HallelujahHomeChurch/notification-api"
        source, current = "a" * 40, "b" * 40
        run = dict(head_sha=source, run_attempt=1, head_branch="main", event="push", status="completed",
                   conclusion="failure", path=".github/workflows/release.yml", head_repository={"full_name": repo})
        jobs = {"jobs": [{"name": "Verify", "conclusion": "success"},
                         {"name": "Build and plan", "conclusion": "failure", "steps": [
                             {"name": "Verify published image identity", "conclusion": "success"},
                             {"name": "Read current release state", "conclusion": "failure"}]},
                         {"name": "Migrate and deploy", "conclusion": "skipped"}]}
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            env = dict(GITHUB_REPOSITORY=repo, GITHUB_REF="refs/heads/main", GITHUB_EVENT_NAME="workflow_dispatch",
                       GITHUB_SHA=current, GITHUB_RUN_ID="2", GITHUB_RUN_ATTEMPT="1", GITHUB_OUTPUT=str(output),
                       RECOVERY_RUN="1", RECOVERY_IMAGE_DIGEST="sha256:" + "c" * 64)
            evidence = [json.dumps(run).encode(), json.dumps(jobs).encode()]
            with (patch.dict(os.environ, env),
                  patch("engagement_status_release.command", side_effect=evidence + [b"", b".github/workflows/release.yml\n"])):
                resolve_release_source()
            self.assertEqual(output.read_text(), f"commit={source}\nrun=1\nimage_artifact=notification-image-{source}-1\n")
            with (patch.dict(os.environ, env),
                  patch("engagement_status_release.command", side_effect=evidence + [b"", b"cmd/notification/main.go\n"])):
                with self.assertRaisesRegex(ValueError, "source changed"):
                    resolve_release_source()
            jobs["jobs"][2]["conclusion"] = "failure"
            with (patch.dict(os.environ, env),
                  patch("engagement_status_release.command", side_effect=[evidence[0], json.dumps(jobs).encode()])):
                with self.assertRaisesRegex(ValueError, "before deployment"):
                    resolve_release_source()

    def test_preview_rejects_other_scope_and_unrelated_drift(self):
        app = fixture()
        candidate = build_transition(app, "enable", VERSION, "status-enable")
        change = {"changeType": "Modify", "resourceId": APP_ID, "before": app, "after": candidate}
        preview = {"status": "Succeeded", "changes": [change]}
        require_preview(preview, app, candidate)
        change["after"] = copy.deepcopy(candidate)
        change["after"]["properties"]["template"]["scale"]["minReplicas"] = 2
        with self.assertRaises(ValueError):
            require_preview(preview, app, candidate)
        change["after"] = candidate
        preview["changes"].append({"changeType": "Modify", "resourceId": "worker"})
        with self.assertRaises(ValueError):
            require_preview(preview, app, candidate)


if __name__ == "__main__":
    unittest.main()
