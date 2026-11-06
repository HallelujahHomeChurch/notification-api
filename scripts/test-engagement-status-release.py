#!/usr/bin/env python3
import copy
import unittest

from engagement_status_release import INGRESS, PROBE, SECRET, build_transition, binding, require_preview


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
