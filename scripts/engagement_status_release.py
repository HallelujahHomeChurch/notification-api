#!/usr/bin/env python3
"""Plan and apply the exact Notification API status-listener transition."""
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

APP = "notification-api"
GROUP = "alive"
SECRET = "notification-engagement-status-token"
ENV = "NOTIFICATION_ENGAGEMENT_STATUS_TOKEN"
VAULT_URI = f"https://alive-notify-r56q6e5qwh5.vault.azure.net/secrets/{SECRET}/"
HOST = "notification-api.internal.gentleriver-81abd7bc.eastasia.azurecontainerapps.io"
INGRESS = {"external": False, "allowInsecure": False, "targetPort": 8082, "transport": "auto",
           "traffic": [{"latestRevision": True, "weight": 100}]}
PROBE = {"type": "Startup", "tcpSocket": {"port": 8082}, "initialDelaySeconds": 1,
         "periodSeconds": 1, "timeoutSeconds": 3, "failureThreshold": 240, "successThreshold": 1}
ENV_REF = {"name": ENV, "secretRef": SECRET}
IMAGE = re.compile(r"alive\.azurecr\.io/alive/notification-api@sha256:[0-9a-f]{64}")
VERSION = re.compile(r"[0-9a-f]{32}")
PLAIN_ENV = {
    "ENVIRONMENT", "PORT", "QUEUE_DRIVER", "SERVICEBUS_NAMESPACE", "SERVICEBUS_QUEUE_NAME",
    "SHUTDOWN_TIMEOUT_SECONDS", "DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
    "AZURE_CLIENT_ID", "NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID", "NOTIFICATION_ACTIVE_HASH_KEY_ID",
    "NOTIFICATION_ALLOWED_CALLERS", "NOTIFICATION_ALLOW_DEV_CALLER_HEADER",
    "NOTIFICATION_TEMPLATE_DAILY_LIMIT", "NOTIFICATIONS_DISABLED",
}


def command(args):
    result = subprocess.run(args, capture_output=True, check=False, timeout=600)
    if result.returncode:
        raise ValueError(f"{' '.join(args[:4])} failed (exit {result.returncode})")
    return result.stdout


def az(*args):
    return json.loads(command(["az", *args, "--only-show-errors", "-o", "json"]))


def app_state(name=APP):
    return az("containerapp", "show", "-g", GROUP, "-n", name)


def identity(app):
    identities = app["identity"]["userAssignedIdentities"]
    owner, = identities
    if app["identity"]["type"] != "UserAssigned" or not owner.endswith("/notification-api-identity"):
        raise ValueError("unexpected API identity")
    return owner


def container(app):
    item, = app["properties"]["template"]["containers"]
    if item["name"] != APP or not IMAGE.fullmatch(item["image"]):
        raise ValueError("unexpected API container or image")
    return item


def binding(app):
    cfg = app["properties"]["configuration"]
    secrets = [item for item in cfg.get("secrets", []) if item["name"] == SECRET]
    env = [item for item in container(app).get("env", []) if item["name"] == ENV]
    version = ""
    if secrets:
        entry, = secrets
        version = entry.get("keyVaultUrl", "").removeprefix(VAULT_URI)
        if not VERSION.fullmatch(version) or entry != {"name": SECRET, "identity": identity(app), "keyVaultUrl": VAULT_URI + version}:
            raise ValueError("unexpected status secret binding")
    ingress = cfg.get("ingress")
    if ingress:
        if (str(ingress.get("transport", "")).lower() != INGRESS["transport"]
                or any(ingress.get(key) != value for key, value in INGRESS.items() if key not in ("traffic", "transport"))):
            raise ValueError("unexpected Notification ingress")
        if (ingress.get("fqdn", HOST) != HOST or ingress.get("additionalPortMappings")
                or ingress.get("customDomains") or ingress.get("ipSecurityRestrictions")
                or len(ingress.get("traffic", [])) != 1
                or not ingress["traffic"][0].get("latestRevision") or ingress["traffic"][0].get("weight") != 100):
            raise ValueError("unexpected status ingress policy")
    startup = [item for item in container(app).get("probes", []) if item["type"] == "Startup"]
    if (bool(ingress) != bool(env) or (env and (env != [ENV_REF] or not version))
            or startup != ([PROBE] if env else [])):
        raise ValueError("inconsistent status ingress, environment binding, or startup probe")
    return bool(env), version


def runtime(app):
    props = app["properties"]
    cfg = props["configuration"]
    for secret in cfg.get("secrets", []):
        if set(secret) != {"name", "keyVaultUrl", "identity"} or not secret["keyVaultUrl"].startswith("https://"):
            raise ValueError("unexpected API secret configuration")
    owner = identity(app)
    desired = {
        "type": "Microsoft.App/containerApps", "apiVersion": "2025-01-01", "name": APP,
        "location": app["location"], "identity": {"type": "UserAssigned", "userAssignedIdentities": {owner: {}}},
        "properties": {
            "managedEnvironmentId": props["managedEnvironmentId"], "workloadProfileName": props["workloadProfileName"],
            "configuration": copy.deepcopy(cfg), "template": copy.deepcopy(props["template"]),
        },
    }
    if props.get("patchingMode", "Automatic") != "Automatic" or props.get("delegatedIdentities"):
        raise ValueError("unexpected platform identity or patch configuration")
    if app.get("tags"):
        desired["tags"] = copy.deepcopy(app["tags"])
    api = container(desired)
    for entry in api.get("env", []):
        if not ((set(entry) == {"name", "value"} and entry["name"] in PLAIN_ENV)
                or set(entry) == {"name", "secretRef"}):
            raise ValueError("unexpected API environment configuration")
    api.pop("imageType", None)
    if api.get("resources", {}).pop("ephemeralStorage", "1Gi") != "1Gi":
        raise ValueError("unexpected API ephemeral storage")
    deployment = desired["properties"]
    for key in ("targetLabel", "identitySettings"):
        if not deployment["configuration"].get(key):
            deployment["configuration"].pop(key, None)
    for registry in deployment["configuration"].get("registries", []):
        for key in ("username", "passwordSecretRef"):
            if not registry.get(key):
                registry.pop(key, None)
    for section in (deployment["configuration"], deployment["configuration"]["dapr"], deployment["template"]):
        for key in list(section):
            if section[key] is None and key != "ingress":
                section.pop(key)
    ingress = desired["properties"]["configuration"].get("ingress")
    if ingress:
        ingress.pop("fqdn", None)
    return desired


def build_transition(app, transition, requested_version, suffix):
    active, version = binding(app)
    if transition == "enable":
        if active or not VERSION.fullmatch(requested_version):
            raise ValueError("enable requires disabled status and an exact secret version")
        version = requested_version
    elif transition != "disable" or not active or requested_version:
        raise ValueError("disable requires enabled status and no supplied version")
    result = runtime(app)
    cfg = result["properties"]["configuration"]
    item = container(result)
    cfg["ingress"] = copy.deepcopy(INGRESS) if transition == "enable" else None
    cfg["secrets"] = [entry for entry in cfg.get("secrets", []) if entry["name"] != SECRET]
    cfg["secrets"].append({"name": SECRET, "identity": identity(app), "keyVaultUrl": VAULT_URI + version})
    item["env"] = [entry for entry in item.get("env", []) if entry["name"] != ENV]
    item["probes"] = [entry for entry in item.get("probes", []) if entry["type"] != "Startup"]
    if transition == "enable":
        item["env"].append(ENV_REF)
        item["probes"].append(PROBE)
    result["properties"]["template"]["revisionSuffix"] = suffix
    return result


def comparable(app):
    # ARM elides nulls and empty arrays in full-resource what-if responses.
    def compact(value):
        if isinstance(value, dict):
            return {key: compact(item) for key, item in value.items() if item not in (None, [], {})}
        if isinstance(value, list):
            return [compact(item) for item in value]
        return value
    return compact(runtime(app))


def require_preview(preview, live, candidate):
    if preview["status"] != "Succeeded":
        raise ValueError("provider what-if did not succeed")
    change, = [item for item in preview["changes"] if item["changeType"] not in ("Ignore", "NoChange")]
    before = comparable(change["before"])
    current = comparable(live)
    # Azure omits its automatically assigned numeric suffix in before payloads.
    suffix = current["properties"]["template"].get("revisionSuffix", "")
    if suffix == "" and "revisionSuffix" in current["properties"]["template"] and "revisionSuffix" not in before["properties"]["template"]:
        before["properties"]["template"]["revisionSuffix"] = suffix
    if (change["changeType"] != "Modify" or change["resourceId"].lower() != live["id"].lower()
            or identity(change["before"]) != identity(live) or identity(change["after"]) != identity(candidate)
            or before != current or comparable(change["after"]) != comparable(candidate)):
        raise ValueError("unexpected provider what-if runtime delta")


def require_engagement_on_dapr():
    revisions = az("containerapp", "revision", "list", "-g", GROUP, "-n", "engagement-api", "--all",
                   "--query", "[?properties.active].name")
    if not revisions:
        raise ValueError("Engagement active revisions unavailable")
    for revision in revisions:
        entries = az("containerapp", "revision", "show", "-g", GROUP, "-n", "engagement-api", "--revision", revision,
                     "--query", f"properties.template.containers[].env[?name=='{ENV}'].name")
        if entries != [[]]:
            raise ValueError("restore Engagement status to Dapr first")


def require_source(image, run_id, commit):
    repo = "HallelujahHomeChurch/notification-api"
    if os.environ["GITHUB_REPOSITORY"].lower() != repo.lower() or not re.fullmatch(r"[1-9][0-9]*", run_id):
        raise ValueError("expected exact owner release run ID")
    info = json.loads(command(["gh", "api", f"repos/{repo}/actions/runs/{run_id}"]))
    source = info["head_sha"]
    if (info["path"] != ".github/workflows/release.yml" or info["head_branch"] != "main"
            or info["event"] not in ("push", "workflow_dispatch") or info["conclusion"] != "success"
            or info["head_repository"]["full_name"].lower() != repo.lower()
            or not re.fullmatch(r"[0-9a-f]{40}", source)):
        raise ValueError("source is not a successful owner release")
    command(["git", "merge-base", "--is-ancestor", source, commit])
    command(["git", "cat-file", "-e", f"{source}:internal/httpapi/engagement_status.go"])
    with tempfile.TemporaryDirectory() as work:
        command(["gh", "run", "download", run_id, "-R", repo, "-n", f"notification-release-plan-{source}", "-D", work])
        if (Path(work) / "image-ref.txt").read_text().strip() != image:
            raise ValueError("owner release image does not match the serving digest")


def transition():
    requested = os.environ["STATUS_TRANSITION"]
    expected = os.environ["STATUS_EXPECTED_REVISION"]
    version = os.environ.get("STATUS_VERSION", "")
    source = os.environ["STATUS_SOURCE_RELEASE_RUN"]
    approved = os.environ.get("STATUS_REVIEWED_PLAN", "")
    commit = os.environ["GITHUB_SHA"]
    live = app_state()
    props = live["properties"]
    if (os.environ["GITHUB_REF"] != "refs/heads/main" or not re.fullmatch(r"[0-9a-f]{40}", commit)
            or not re.fullmatch(r"notification-api--[a-z0-9-]+", expected)
            or props["latestRevisionName"] != expected or props["latestReadyRevisionName"] != expected
            or props["configuration"]["activeRevisionsMode"] != "Single"
            or props["configuration"]["dapr"]["enabled"] is not True or props["configuration"]["dapr"]["appPort"] != 8081):
        raise ValueError("exact ready revision or production controls did not match")
    require_source(container(live)["image"], source, commit)
    if requested == "disable":
        require_engagement_on_dapr()
    fingerprint = hashlib.sha256(json.dumps([commit, source, expected, requested, version, comparable(live)], sort_keys=True).encode()).hexdigest()
    suffix = "status-" + fingerprint[:16]
    candidate = build_transition(live, requested, version, suffix)
    if requested == "enable":
        metadata = az("resource", "show", "--ids",
                      re.split(r"/providers/microsoft\.app/", live["id"], flags=re.IGNORECASE)[0]
                      + f"/providers/Microsoft.KeyVault/vaults/alive-notify-r56q6e5qwh5/secrets/{SECRET}",
                      "--api-version", "2026-02-01", "--query",
                      "{enabled:properties.attributes.enabled,uri:properties.secretUriWithVersion}")
        if metadata != {"enabled": True, "uri": VAULT_URI + version}:
            raise ValueError("pinned latest Key Vault version is unavailable")
    plan_transition(live, candidate, suffix)
    print(f"Validated {requested} for {expected}, {container(live)['image']}, candidate {APP}--{suffix}", flush=True)
    print(f"Provider scope: only {APP} ingress 8082, status secret/env, startup probe and revision suffix; Key Vault version {binding(candidate)[1]}", flush=True)
    print(f"Reviewed plan fingerprint: {fingerprint}", flush=True)
    if not approved:
        return
    if approved != fingerprint:
        raise ValueError("reviewed plan fingerprint changed")
    if requested == "disable":
        require_engagement_on_dapr()
    before_apply = app_state()
    if (comparable(before_apply) != comparable(live)
            or before_apply["properties"]["latestReadyRevisionName"] != expected
            or before_apply["properties"]["latestRevisionName"] != expected):
        raise ValueError("current ready revision changed")
    plan_transition(live, candidate, suffix, apply=True)
    image = container(live)["image"]
    check = subprocess.run([sys.executable, "scripts/verify-aca-revision.py", GROUP, APP, image, expected], check=False)
    current = app_state()
    new_revision = APP + "--" + suffix
    if check.returncode == 10 and current["properties"]["latestRevisionName"] == new_revision and comparable(current) == comparable(candidate):
        if requested == "enable":
            require_engagement_on_dapr()
        restore = runtime(live)
        rollback_suffix = "status-rollback-" + fingerprint[:16]
        restore["properties"]["template"]["revisionSuffix"] = rollback_suffix
        # Keep a newly referenced credential available until retired replicas drain.
        restore["properties"]["configuration"]["secrets"] = candidate["properties"]["configuration"]["secrets"]
        plan_transition(current, restore, rollback_suffix, apply=True)
        command([sys.executable, "scripts/verify-aca-revision.py", GROUP, APP, image, new_revision])
        raise ValueError("confirmed unhealthy status revision rolled back")
    if check.returncode or current["properties"]["latestReadyRevisionName"] != new_revision or binding(current) != binding(candidate):
        raise ValueError("candidate ready revision or status binding did not match")


def plan_transition(live, candidate, suffix, apply=False):
    with tempfile.TemporaryDirectory() as work:
        template = Path(work) / "runtime.json"
        template.write_text(json.dumps({"$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
                                        "contentVersion": "1.0.0.0", "resources": [candidate]}))
        args = ["-g", GROUP, "--name", suffix, "--template-file", str(template)]
        preview = az("deployment", "group", "what-if", *args, "--validation-level", "Provider", "--result-format", "FullResourcePayloads", "--no-pretty-print")
        require_preview(preview, live, candidate)
        if apply:
            command(["az", "deployment", "group", "create", *args, "--only-show-errors", "-o", "none"])


def main():
    if sys.argv[1:] == ["capture"]:
        live = app_state()
        if live["properties"]["latestRevisionName"] != live["properties"]["latestReadyRevisionName"]:
            raise ValueError("Notification status requires an exact settled revision")
        enabled, version = binding(live)
        with open(os.environ["GITHUB_ENV"], "a") as output:
            output.write(f"ENGAGEMENT_STATUS_ENABLED={str(enabled).lower()}\nENGAGEMENT_STATUS_VERSION={version}\n")
    elif sys.argv[1:] == ["transition"]:
        transition()
    else:
        raise ValueError("usage: engagement_status_release.py capture|transition")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError):
        print("Notification status release rejected by identity, state, or provider verification", file=sys.stderr)
        sys.exit(1)
