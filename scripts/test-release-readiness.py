#!/usr/bin/env python3
"""Exercise candidate evidence and the workflow's actual recovery predicates."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('verify', ROOT / 'scripts/verify-aca-revision.py')
verify = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verify)
APP = 'notification-api' if 'notification' in (ROOT / 'go.mod').read_text() else 'engagement-api'
OLD, NEW = APP + '--old', APP + '--new'
IMAGE = f'alive.azurecr.io/alive/{APP}@sha256:' + 'a' * 64
CURRENT = {'latest': NEW, 'ready': NEW, 'ingress': None}
HEALTHY = {'image': IMAGE, 'active': True, 'health': 'Healthy', 'running': 'Running', 'provisioning': 'Provisioned'}


class ReadinessTests(unittest.TestCase):
    def run_check(self, responses):
        clock = [0]
        def sleep(_): clock[0] += 300
        def run(*args, **kwargs):
            item = responses.pop(0) if len(responses) > 1 else responses[0]
            if isinstance(item, Exception): raise item
            return subprocess.CompletedProcess([], 0, json.dumps(item), '')
        with tempfile.TemporaryDirectory() as folder:
            output = Path(folder) / 'output'
            with patch.object(verify.subprocess, 'run', side_effect=run), patch.object(verify.time, 'monotonic', side_effect=lambda: clock[0]), \
                 patch.object(verify.time, 'sleep', side_effect=sleep), patch.dict(os.environ, {'GITHUB_OUTPUT': str(output)}), \
                 contextlib.redirect_stdout(io.StringIO()) as stdout:
                status = verify.verify('alive', APP, IMAGE, OLD)
            return status, output.read_text(), stdout.getvalue()

    def test_new_immutable_healthy(self):
        code, output, _ = self.run_check([CURRENT, HEALTHY])
        self.assertEqual(code, 0)
        self.assertTrue(output.endswith(f'state=healthy\nrevision={NEW}\n'))

    def test_known_candidate_unhealthy(self):
        for field, value in [('health', 'Unhealthy'), ('provisioning', 'Failed'), ('running', 'Failed')]:
            self.assertEqual(self.run_check([CURRENT, HEALTHY | {field: value}])[0], 10)

    def test_read_errors_are_unavailable_and_private_output_not_logged(self):
        for failure in [subprocess.CalledProcessError(1, 'az', stderr='PRIVATE_MARKER'),
                        subprocess.TimeoutExpired('az', 30), ValueError('PRIVATE_MARKER')]:
            code, output, log = self.run_check([failure])
            self.assertEqual(code, 20)
            self.assertNotIn('PRIVATE_MARKER', output + log)
            self.assertNotIn('state=unhealthy', output)

    def test_stale_wrong_missing_and_changed_candidates_are_unavailable(self):
        cases = [[CURRENT | {'latest': OLD}], [{}], [CURRENT, HEALTHY | {'image': 'wrong'}],
                 [CURRENT, HEALTHY | {'active': False}, CURRENT | {'latest': APP + '--other'}]]
        for values in cases:
            self.assertEqual(self.run_check(values)[0], 20)

    def test_pending_candidate_is_not_claimed_unhealthy(self):
        for field, value in [('active', False), ('health', 'None'), ('running', 'Processing')]:
            self.assertEqual(self.run_check([CURRENT, HEALTHY | {field: value}, CURRENT,
                                            HEALTHY | {field: value}])[0], 20)

    def test_ingress_requires_all_traffic_to_candidate(self):
        good = [{'latestRevision': True, 'weight': 100}]
        code, _, _ = self.run_check([CURRENT | {'ingress': {'traffic': good}}, HEALTHY])
        self.assertEqual(code, 0)
        wrong = [{'revisionName': OLD, 'weight': 100}]
        values = [CURRENT | {'ingress': {'traffic': wrong}}, HEALTHY] * 2
        self.assertEqual(self.run_check(values)[0], 20)

    def test_api_negative_stops_before_worker_can_overwrite_state(self):
        if APP != 'notification-api':
            self.skipTest('Notification API/worker sequencing only')
        workflow = (ROOT / '.github/workflows/release.yml').read_text()
        block = workflow.split('      - name: Verify ACA revisions\n', 1)[1].split('\n      - ', 1)[0]
        command = '\n'.join(line[10:] for line in block.split('        run: |\n', 1)[1].splitlines())
        mock = r'''python3() {
          echo "$3" >> "$CALLS"
          if [ "$3" = notification-api ]; then
            echo "state=$API_STATE" >> "$GITHUB_OUTPUT"
            return "$API_CODE"
          fi
          echo state=verification_unavailable >> "$GITHUB_OUTPUT"
          return 20
        }
'''
        for code, state, calls in [(10, 'unhealthy', ['notification-api']),
                                   (0, 'healthy', ['notification-api', 'notification-worker'])]:
            with tempfile.TemporaryDirectory() as folder:
                output, seen = Path(folder) / 'output', Path(folder) / 'calls'
                env = os.environ | {'GITHUB_OUTPUT': str(output), 'CALLS': str(seen),
                                    'API_CODE': str(code), 'API_STATE': state, 'RESOURCE_GROUP': 'alive',
                                    'API_APP_NAME': 'notification-api', 'WORKER_APP_NAME': 'notification-worker',
                                    'IMAGE_REF': IMAGE, 'PREVIOUS_API_REVISION': OLD,
                                    'PREVIOUS_WORKER_REVISION': 'notification-worker--old'}
                outcome = subprocess.run(['bash', '-e', '-c', mock + command], env=env, capture_output=True)
                self.assertEqual(outcome.returncode, code if code else 20)
                self.assertEqual(seen.read_text().splitlines(), calls)
                self.assertEqual(output.read_text().splitlines()[-1], 'state=' + (state if code else 'verification_unavailable'))

    def test_actual_workflow_recovers_only_confirmed_unhealthy(self):
        workflow = (ROOT / '.github/workflows/release.yml').read_text()
        self.assertNotIn('az containerapp exec', workflow)
        marker = '      - name: Roll back runtime after failed deployment verification' if APP == 'notification-api' else '      - name: Roll back failed runtime'
        predicate = workflow.split(marker, 1)[1].split('        if: ', 1)[1].splitlines()[0]
        self.assertNotIn("outcome == 'failure'", predicate)
        self.assertIn("outputs.state == 'unhealthy'", predicate)
        # Evaluate the exact restricted expression, not an independent copy.
        expression = predicate.removeprefix('${{ ').removesuffix(' }}').replace('failure()', 'True').replace('&&', 'and').replace('||', 'or')
        for state, expected in [('healthy', False), ('unhealthy', True), ('verification_unavailable', False), ('', False)]:
            candidate = expression
            for key in ['steps.deploy_runtime.outputs.state', 'steps.smoke_runtime.outputs.state', 'steps.verify_runtime.outputs.state']:
                candidate = candidate.replace(key, repr(state))
            candidate = candidate.replace('steps.capture_runtime.outcome', repr('success'))
            self.assertEqual(eval(candidate, {'__builtins__': {}}), expected)
        self.assertIn('concurrency:', workflow)
        self.assertIn('cancel-in-progress: false', workflow)
        self.assertIn('python3 scripts/verify-aca-revision.py', workflow)


if __name__ == '__main__': unittest.main()
