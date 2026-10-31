#!/usr/bin/env python3
"""Bounded read-only probe; only Azure exec handshake throttling is retryable."""
import re
import shlex
import subprocess
import sys
import time

HANDSHAKE_429 = 'websocket._exceptions.WebSocketBadStatusException: Handshake status 429 Too Many Requests'
HTTP = re.compile(r'HTTP/\d(?:\.\d)?\s+\d{3}')


def probe(resource_group, gateway):
    deadline = time.monotonic() + 60
    command = shlex.join(['az', 'containerapp', 'exec', '--resource-group', resource_group,
                          '--name', gateway, '--command',
                          '/usr/bin/wget -S -O - http://localhost:3500/v1.0/invoke/notification-api/method/ready'])
    for attempt in range(3):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        result = subprocess.run(['timeout', f'{remaining:.3f}s', 'script', '-q', '-e', '-c',
                                 command, '/dev/null'], capture_output=True, text=True)
        output = (result.stdout + result.stderr).replace('\r', '')
        if (result.returncode == 0 and re.search(r'HTTP/1\.[01]\s+200\b', output)
                and re.search(r'"status"\s*:\s*"ready"', output)):
            print('Dapr readiness verified: HTTP 200, status ready')
            return 0
        # Any HTTP response is application/proxy evidence: never retry it here.
        if result.returncode not in (0, 124) and HANDSHAKE_429 in output and not HTTP.search(output):
            if attempt < 2:
                delay = 2 ** (attempt + 1)
                if deadline - time.monotonic() > delay:
                    print('Azure exec handshake throttled before HTTP; bounded retry', flush=True)
                    time.sleep(delay)
                    continue
            print('Azure exec handshake throttle retry budget exhausted', file=sys.stderr)
        else:
            print('Dapr readiness unverified: HTTP failure, timeout or unknown exec error; no retry', file=sys.stderr)
        return 1
    print('Dapr readiness total deadline exhausted', file=sys.stderr)
    return 1


if __name__ == '__main__':
    if len(sys.argv) != 3:
        raise SystemExit('usage: probe-dapr-readiness.py RESOURCE_GROUP GATEWAY_APP')
    raise SystemExit(probe(*sys.argv[1:]))
