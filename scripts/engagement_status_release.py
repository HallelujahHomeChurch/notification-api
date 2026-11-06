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
import time

APP = "notification-api"
GROUP = "alive"
SECRET = "notification-engagement-status-token"
ENV = "NOTIFICATION_ENGAGEMENT_STATUS_TOKEN"
VAULT_SECRETS_URI = "https://alive-notify-r56q6e5qwh5.vault.azure.net/secrets/"
VAULT_URI = VAULT_SECRETS_URI + SECRET + "/"
HOST = "notification-api.internal.gentleriver-81abd7bc.eastasia.azurecontainerapps.io"
INGRESS = {"external": False, "allowInsecure": False, "targetPort": 8082, "transport": "auto",
           "traffic": [{"latestRevision": True, "weight": 100}]}
PROBE = {"type": "Startup", "tcpSocket": {"port": 8082}, "initialDelaySeconds": 1,
         "periodSeconds": 1, "timeoutSeconds": 3, "failureThreshold": 240, "successThreshold": 1}
ENV_REF = {"name": ENV, "secretRef": SECRET}
IMAGE = re.compile(r"alive\.azurecr\.io/alive/notification-api@sha256:[0-9a-f]{64}")
VERSION = re.compile(r"[0-9a-f]{32}")
WRITE_SCOPES = (
    ("NOTIFICATION_ACCOUNT_PRIVATE_TOKEN", "notification-account-private-token", "NOTIFICATION_ACCOUNT_PRIVATE_SECRET_VERSION"),
    ("NOTIFICATION_ENGAGEMENT_SEND_TOKEN", "notification-engagement-send-token", "NOTIFICATION_ENGAGEMENT_SEND_SECRET_VERSION"),
)
DAPR_CONFIG = {"enabled": True, "appId": APP, "appPort": 8081, "appProtocol": "http",
               "logLevel": "warn", "enableApiLogging": False}
PLAIN_ENV = {
    "ENVIRONMENT", "PORT", "QUEUE_DRIVER", "SERVICEBUS_NAMESPACE", "SERVICEBUS_QUEUE_NAME",
    "SHUTDOWN_TIMEOUT_SECONDS", "DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
    "AZURE_CLIENT_ID", "NOTIFICATION_ACTIVE_ENCRYPTION_KEY_ID", "NOTIFICATION_ACTIVE_HASH_KEY_ID",
    "NOTIFICATION_ALLOWED_CALLERS", "NOTIFICATION_ALLOW_DEV_CALLER_HEADER",
    "NOTIFICATION_TEMPLATE_DAILY_LIMIT", "NOTIFICATIONS_DISABLED",
}


class RetirementReadbackError(ValueError):
    """A fixed, safe-to-log retirement verification error."""


class ServingPending(RetirementReadbackError):
    """The exact revision is still settling."""


def changed_snapshot_paths(before, after, prefix=""):
    if before == after:
        return set()
    if not isinstance(before, dict) or not isinstance(after, dict):
        return {prefix}
    safe_fields = {
        "type", "apiVersion", "name", "location", "identity", "userAssignedIdentities", "tags",
        "properties", "managedEnvironmentId", "workloadProfileName", "configuration", "template",
        "ingress", "secrets", "registries", "activeRevisionsMode", "identitySettings",
        "scale", "minReplicas", "maxReplicas", "rules", "pollingInterval", "cooldownPeriod",
        "containers", "initContainers", "volumes", "revisionSuffix",
    }
    paths = set()
    missing = object()
    for key in before.keys() | after.keys():
        old, new = before.get(key, missing), after.get(key, missing)
        if old == new:
            continue
        if key not in safe_fields:
            paths.add(prefix + ".*")
            continue
        path = f"{prefix}.{key}" if prefix else key
        paths.update(changed_snapshot_paths(old, new, path))
    return paths


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


def same_secret_binding(entry, expected):
    return (isinstance(entry.get("identity"), str)
            and dict(entry, identity=entry["identity"].lower()) == dict(expected, identity=expected["identity"].lower()))


def binding(app):
    cfg = app["properties"]["configuration"]
    secrets = [item for item in cfg.get("secrets", []) if item["name"] == SECRET]
    env = [item for item in container(app).get("env", []) if item["name"] == ENV]
    version = ""
    if secrets:
        entry, = secrets
        version = entry.get("keyVaultUrl", "").removeprefix(VAULT_URI)
        if not VERSION.fullmatch(version) or not same_secret_binding(entry, {"name": SECRET, "identity": identity(app), "keyVaultUrl": VAULT_URI + version}):
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


def write_binding(app, status_enabled, *, deployed=False):
    env = container(app).get("env", [])
    secrets = app["properties"]["configuration"].get("secrets", [])
    for env_name, secret_name, version_var in WRITE_SCOPES:
        requested = os.environ.get(version_var, "")
        if requested and (not status_enabled or not VERSION.fullmatch(requested)):
            raise ValueError("private writes require the active HTTPS listener and a pinned secret version")
        entries = [entry for entry in env if entry["name"] == env_name]
        bindings = [entry for entry in secrets if entry["name"] == secret_name]
        if entries or bindings or (deployed and requested):
            expected = {"name": secret_name, "identity": identity(app),
                        "keyVaultUrl": VAULT_SECRETS_URI + secret_name + "/" + requested}
            expected_env = [{"name": env_name, "secretRef": secret_name}]
            if (not requested or len(bindings) != 1 or not same_secret_binding(bindings[0], expected)
                    or ((entries or deployed) and entries != expected_env)):
                raise ValueError("unexpected Notification private write binding")


def require_private_transport(app):
    enabled, status_version = binding(app)
    write_binding(app, enabled, deployed=True)
    if (not enabled or app["properties"]["configuration"]["ingress"].get("fqdn") != HOST
            or any(not os.environ.get(var) for _, _, var in WRITE_SCOPES)):
        raise ValueError("retirement requires every scoped HTTPS binding")
    writes = [(env, secret, VAULT_SECRETS_URI + secret + "/" + os.environ[var])
              for env, secret, var in WRITE_SCOPES]
    return writes + [(ENV, SECRET, VAULT_URI + status_version)]


def require_caller_pins(app, name, pins):
    item, = app["properties"]["template"]["containers"]
    if item["name"] != name:
        raise ValueError("unexpected Notification consumer container")
    identities = {key.lower() for key in app["identity"]["userAssignedIdentities"]}
    for env_name, secret_name, uri in pins:
        env, = [entry for entry in item["env"] if entry["name"] == env_name]
        secret, = [entry for entry in app["properties"]["configuration"]["secrets"] if entry["name"] == secret_name]
        if (env.get("secretRef") != secret_name or env.get("value") is not None
                or secret.get("keyVaultUrl") != uri or secret["identity"].lower() not in identities):
            raise ValueError("Notification consumer credential pin does not match")


def require_serving(app, name, expected):
    props = app["properties"]
    item, = props["template"]["containers"]
    image = item["image"]
    ready = props.get("latestReadyRevisionName")
    if (not re.fullmatch(re.escape(name) + r"--[a-z0-9-]+", expected)
            or props["latestRevisionName"] != expected or ready not in (None, "", expected)
            or item["name"] != name
            or not re.fullmatch(r"alive\.azurecr\.io/alive/" + re.escape(name) + r"@sha256:[0-9a-f]{64}", image)):
        raise RetirementReadbackError("exact Notification serving revision or image did not match")
    ingress = props["configuration"].get("ingress")
    if ingress:
        traffic, = ingress["traffic"]
        if traffic.get("weight") != 100 or not (traffic.get("latestRevision") is True or traffic.get("revisionName") == expected):
            raise RetirementReadbackError("exact Notification revision does not hold all traffic")
    revision = az("containerapp", "revision", "show", "-g", GROUP, "-n", name, "--revision", expected,
                  "--query", "{name:name,active:properties.active,health:properties.healthState,running:properties.runningState,provisioned:properties.provisioningState,images:properties.template.containers[].image}")
    if revision.get("name") != expected or revision.get("images") != [image]:
        raise RetirementReadbackError("exact Notification revision readback identity did not match")
    if (ready != expected or revision != {"name": expected, "active": True, "health": "Healthy", "running": "Running",
                                           "provisioned": "Provisioned", "images": [image]}):
        raise ServingPending("exact Notification serving revision is not healthy yet")
    revisions = az("containerapp", "revision", "list", "-g", GROUP, "-n", name, "--all",
                   "--query", "[?properties.active || properties.replicas > `0`].name")
    if revisions != [expected]:
        raise ServingPending("prior Notification callers or API replicas have not drained")
    return image


def dapr_enabled(app):
    config = app["properties"]["configuration"]["dapr"]
    enabled = config["enabled"]
    if type(enabled) is not bool or (enabled and {key: value for key, value in config.items() if value is not None} != DAPR_CONFIG):
        raise ValueError("unexpected Notification API Dapr configuration")
    return enabled


def retire_dapr():
    if os.environ["GITHUB_REF"] != "refs/heads/main" or os.environ["GITHUB_REPOSITORY"] != "HallelujahHomeChurch/notification-api":
        raise ValueError("retirement requires the production owner workflow")
    expected, account_revision, engagement_revision = os.environ["RETIRE_DAPR_REVISIONS"].split()
    live = app_state()
    enabled = dapr_enabled(live)
    pins = require_private_transport(live)
    account_pin, engagement_pin, status_pin = pins
    image = require_serving(live, APP, expected)
    worker = app_state("notification-worker")["properties"]
    worker_revision = worker["latestReadyRevisionName"]
    if (worker["configuration"]["dapr"]["enabled"] is not False
            or not re.fullmatch(r"notification-worker--[a-z0-9-]+", worker_revision or "")
            or worker_revision != worker["latestRevisionName"]):
        raise ValueError("Notification Worker must remain healthy with Dapr disabled")

    def verify_consumers():
        for name, revision, scopes in (
                ("account-api", account_revision, (account_pin,)),
                ("engagement-api", engagement_revision, (engagement_pin, status_pin))):
            caller = app_state(name)
            require_serving(caller, name, revision)
            require_caller_pins(caller, name, scopes)

    def without_dapr(app):
        snapshot = comparable(app)
        snapshot["properties"]["configuration"].pop("dapr")
        return snapshot

    verify_consumers()
    if enabled:
        if app_state()["properties"]["latestRevisionName"] != expected:
            raise ValueError("Notification API changed before retirement")
        expected_snapshot = without_dapr(live)
        try:
            command(["az", "containerapp", "dapr", "disable", "-g", GROUP, "-n", APP, "--only-show-errors", "-o", "none"])
            # ACA can restart the same revision after an application-level Dapr change.
            max_attempts = 12
            for attempt in range(1, max_attempts + 1):
                current = app_state()
                try:
                    props = current["properties"]
                    if props["latestRevisionName"] != expected or container(current)["image"] != image:
                        raise RetirementReadbackError("Notification API retirement identity readback did not match")
                    provisioning = props.get("provisioningState")
                    if provisioning == "InProgress":
                        raise ServingPending("Notification API retirement provisioning is still in progress")
                    if provisioning != "Succeeded":
                        raise RetirementReadbackError("Notification API retirement provisioning did not succeed")
                    current_snapshot = without_dapr(current)
                    if current_snapshot != expected_snapshot:
                        paths = sorted(changed_snapshot_paths(expected_snapshot, current_snapshot))
                        raise RetirementReadbackError(
                            f"Notification API retirement configuration readback differs at {len(paths)} path(s): "
                            + ", ".join(paths[:8]))
                    try:
                        current_pins = require_private_transport(current)
                    except (ValueError, KeyError, TypeError) as error:
                        raise RetirementReadbackError("Notification API private HTTPS readback did not match") from error
                    if current_pins != pins:
                        raise RetirementReadbackError("Notification API private HTTPS readback did not match")
                    if dapr_enabled(current):
                        raise ServingPending("Notification API Dapr mode has not settled to disabled")
                    if require_serving(current, APP, expected) != image:
                        raise RetirementReadbackError("Notification API retirement image readback did not match")
                    break
                except ServingPending:
                    if attempt == max_attempts:
                        raise
                    time.sleep(5)
            verify_consumers()
            current_worker = app_state("notification-worker")["properties"]
            if (current_worker["latestRevisionName"] != worker_revision
                    or current_worker["latestReadyRevisionName"] != worker_revision
                    or current_worker["configuration"]["dapr"]["enabled"] is not False):
                raise ValueError("Notification Worker changed during API-only retirement")
        except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
            current = app_state()
            if (current["properties"]["latestRevisionName"] == expected
                    and container(current)["image"] == image and not dapr_enabled(current)):
                command(["az", "containerapp", "dapr", "enable", "-g", GROUP, "-n", APP,
                         "--dapr-app-id", APP, "--dapr-app-port", "8081", "--dapr-app-protocol", "http",
                         "--dapr-log-level", "warn", "--only-show-errors", "-o", "none"])
                if not dapr_enabled(app_state()):
                    raise RetirementReadbackError("Notification API Dapr rollback could not be verified")
            raise
    print(f"Notification API sidecar disabled on {expected} ({image}); Account and Engagement HTTPS pins verified")


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
    elif any(entry["name"] in {scope[0] for scope in WRITE_SCOPES} for entry in container(app).get("env", [])):
        raise ValueError("retire private write bindings before disabling HTTPS ingress")
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
    result = runtime(app)
    owner, = result["identity"]["userAssignedIdentities"]
    result["identity"]["userAssignedIdentities"] = {owner.lower(): {}}
    configuration = result["properties"]["configuration"]
    for item in configuration.get("secrets", []) + configuration.get("registries", []):
        if isinstance(item.get("identity"), str):
            item["identity"] = item["identity"].lower()
    return compact(result)


def require_preview(preview, live, candidate):
    if preview["status"] != "Succeeded":
        raise ValueError("provider what-if did not succeed")
    change, = [item for item in preview["changes"] if item["changeType"] not in ("Ignore", "NoChange")]
    before = comparable(change["before"])
    current = comparable(live)
    after = comparable(change["after"])
    after_cfg = after["properties"]["configuration"]
    provider_ingress = change["after"]["properties"]["configuration"].get("ingress")
    # Provider what-if retains only these defaults when the API request sets ingress to null.
    if (live["properties"]["configuration"].get("ingress")
            and candidate["properties"]["configuration"].get("ingress") is None
            and provider_ingress == {"allowInsecure": False, "external": False, "transport": "Auto"}):
        after_cfg.pop("ingress")
    # Azure omits its automatically assigned numeric suffix in before payloads.
    suffix = current["properties"]["template"].get("revisionSuffix", "")
    if suffix == "" and "revisionSuffix" in current["properties"]["template"] and "revisionSuffix" not in before["properties"]["template"]:
        before["properties"]["template"]["revisionSuffix"] = suffix
    if (change["changeType"] != "Modify" or change["resourceId"].lower() != live["id"].lower()
            or identity(change["before"]).lower() != identity(live).lower()
            or identity(change["after"]).lower() != identity(candidate).lower()
            or before != current or after != comparable(candidate)):
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


def resolve_release_source():
    repo = "HallelujahHomeChurch/notification-api"
    commit = os.environ["GITHUB_SHA"]
    run_id = os.environ["GITHUB_RUN_ID"]
    attempt = os.environ["GITHUB_RUN_ATTEMPT"]
    recovery = os.environ.get("RECOVERY_RUN", "")
    digest = os.environ.get("RECOVERY_IMAGE_DIGEST", "")
    if recovery or digest:
        if (os.environ["GITHUB_REPOSITORY"] != repo or os.environ["GITHUB_REF"] != "refs/heads/main"
                or os.environ["GITHUB_EVENT_NAME"] != "workflow_dispatch" or recovery == run_id
                or not re.fullmatch(r"[1-9][0-9]*", recovery) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest)):
            raise ValueError("recovery requires a previous owner run and exact published digest")
        api = f"repos/{repo}/actions/runs/{recovery}"
        info = json.loads(command(["gh", "api", api]))
        source = info["head_sha"]
        if (info["path"] != ".github/workflows/release.yml" or info["head_branch"] != "main"
                or info["event"] not in ("push", "workflow_dispatch") or info["conclusion"] != "failure"
                or info["status"] != "completed" or info["head_repository"]["full_name"] != repo
                or not re.fullmatch(r"[0-9a-f]{40}", source) or type(info["run_attempt"]) is not int or info["run_attempt"] < 1):
            raise ValueError("recovery source is not an exact failed production owner release")
        attempt = str(info["run_attempt"])
        jobs = json.loads(command(["gh", "api", f"{api}/attempts/{attempt}/jobs?per_page=100"]))["jobs"]
        by_name = {job["name"]: job for job in jobs}
        plan = by_name["Build and plan"]
        steps = {step["name"]: step["conclusion"] for step in plan["steps"]}
        if (len(by_name) != len(jobs) or by_name["Verify"]["conclusion"] != "success"
                or plan["conclusion"] != "failure" or steps.get("Verify published image identity") != "success"
                or steps.get("Read current release state") != "failure" or by_name["Migrate and deploy"]["conclusion"] != "skipped"):
            raise ValueError("original run did not stop after verified image publication and before deployment")
        command(["git", "merge-base", "--is-ancestor", source, commit])
        changed = command(["git", "diff", "--name-only", source, commit, "--"]).decode().splitlines()
        if set(changed) - {".github/workflows/release.yml", "scripts/engagement_status_release.py",
                           "scripts/test-engagement-status-release.py", "scripts/release-static.test.sh"}:
            raise ValueError("runtime or deployment source changed after the recovered image was built")
        commit, run_id = source, recovery
    if (not re.fullmatch(r"[0-9a-f]{40}", commit) or not re.fullmatch(r"[1-9][0-9]*", run_id)
            or not re.fullmatch(r"[1-9][0-9]*", attempt)):
        raise ValueError("invalid release provenance")
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        output.write(f"commit={commit}\nrun={run_id}\nimage_artifact=notification-image-{commit}-{attempt}\n")


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
    if sys.argv[1:] in (["capture"], ["verify-writes"]):
        live = app_state()
        if live["properties"]["latestRevisionName"] != live["properties"]["latestReadyRevisionName"]:
            raise ValueError("Notification status requires an exact settled revision")
        enabled, version = binding(live)
        api_dapr = dapr_enabled(live)
        deployed = sys.argv[1:] == ["verify-writes"]
        if api_dapr:
            write_binding(live, enabled, deployed=deployed)
        else:
            require_private_transport(live)
        if deployed:
            return
        with open(os.environ["GITHUB_ENV"], "a") as output:
            output.write(f"ENGAGEMENT_STATUS_ENABLED={str(enabled).lower()}\nENGAGEMENT_STATUS_VERSION={version}\n")
            output.write(f"API_DAPR_ENABLED={str(api_dapr).lower()}\n")
    elif sys.argv[1:] == ["transition"]:
        transition()
    elif sys.argv[1:] == ["retire-dapr"]:
        retire_dapr()
    elif sys.argv[1:] == ["release-source"]:
        resolve_release_source()
    else:
        raise ValueError("usage: engagement_status_release.py capture|verify-writes|transition|retire-dapr|release-source")


if __name__ == "__main__":
    try:
        main()
    except RetirementReadbackError as error:
        print(f"Notification API retirement verification failed: {error}", file=sys.stderr)
        sys.exit(1)
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
        print("Notification status release rejected by identity, state, or provider verification", file=sys.stderr)
        sys.exit(1)
