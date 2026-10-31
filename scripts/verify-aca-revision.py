#!/usr/bin/env python3
"""Verify only a new immutable ACA revision; unavailable evidence is not unhealthy."""
import json
import os
import re
import subprocess
import sys
import time

APP_QUERY = '{latest:properties.latestRevisionName,ready:properties.latestReadyRevisionName,ingress:properties.configuration.ingress}'
REVISION_QUERY = '{image:properties.template.containers[0].image,active:properties.active,health:properties.healthState,running:properties.runningState,provisioning:properties.provisioningState}'


def result(state, revision=''):
    # Initial unavailable output also covers termination before the final result.
    if os.environ.get('GITHUB_OUTPUT'):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            output.write(f'state={state}\nrevision={revision}\n')
    print(f'ACA verification: {state}' + (f' ({revision})' if revision else ''), flush=True)
    return {'healthy': 0, 'unhealthy': 10, 'verification_unavailable': 20}[state]


def read_azure(args, query, deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError()
    response = subprocess.run(['az', 'containerapp', *args, '--query', query, '-o', 'json',
                               '--only-show-errors'], capture_output=True, text=True,
                              timeout=min(30, remaining), check=True)
    value = json.loads(response.stdout)
    if not isinstance(value, dict):
        raise ValueError('missing metadata')
    return value


def verify(group, app, image, previous):
    result('verification_unavailable')
    if not re.fullmatch(r'[a-z0-9-]+', app) or not previous.startswith(app + '--'):
        return 20
    if not re.fullmatch(r'alive\.azurecr\.io/alive/[a-z0-9-]+@sha256:[0-9a-f]{64}', image):
        return 20
    deadline = time.monotonic() + 600
    expected = None
    try:
        while time.monotonic() < deadline:
            current = read_azure(['show', '-g', group, '-n', app], APP_QUERY, deadline)
            if not {'latest', 'ready', 'ingress'} <= current.keys():
                return 20
            latest = current.get('latest')
            if not isinstance(latest, str) or not re.fullmatch(re.escape(app) + r'--[a-z0-9-]+', latest):
                return 20
            if latest == previous:
                time.sleep(min(5, max(0, deadline - time.monotonic())))
                continue
            if expected is not None and latest != expected:
                return result('verification_unavailable', expected)
            revision = read_azure(['revision', 'show', '-g', group, '-n', app, '--revision', latest], REVISION_QUERY, deadline)
            if not {'image', 'active', 'health', 'running', 'provisioning'} <= revision.keys():
                return 20
            if revision.get('image') != image:
                return result('verification_unavailable')
            expected = latest
            if (revision.get('health') == 'Unhealthy' or revision.get('provisioning') == 'Failed'
                    or revision.get('running') == 'Failed'):
                return result('unhealthy', expected)
            ingress = current.get('ingress')
            traffic_ready = ingress is None
            if isinstance(ingress, dict):
                traffic = ingress.get('traffic')
                if not isinstance(traffic, list):
                    return result('verification_unavailable', expected)
                traffic_ready = bool(traffic) and all(
                    isinstance(route, dict) and isinstance(route.get('weight'), int)
                    and route['weight'] >= 0 for route in traffic)
                if traffic_ready:
                    traffic_ready = sum(route['weight'] for route in traffic) == 100 and all(
                        route['weight'] == 0 or route.get('revisionName') == expected
                        or (route.get('latestRevision') is True and route.get('revisionName') in (None, expected)) for route in traffic)
            if (current.get('ready') == expected and revision.get('active') is True
                    and revision.get('health') == 'Healthy' and revision.get('running') == 'Running'
                    and revision.get('provisioning') == 'Provisioned' and traffic_ready):
                return result('healthy', expected)
            time.sleep(min(5, max(0, deadline - time.monotonic())))
    except (subprocess.SubprocessError, OSError, ValueError, TimeoutError):
        pass
    return result('verification_unavailable', expected or '')


if __name__ == '__main__':
    if len(sys.argv) != 5:
        raise SystemExit('usage: verify-aca-revision.py RESOURCE_GROUP APP IMAGE PREVIOUS_REVISION')
    raise SystemExit(verify(*sys.argv[1:]))
