#!/usr/bin/env python3
"""Execute the workflow's actual readiness function against a scripted CLI."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
lines = (ROOT / '.github/workflows/release.yml').read_text().splitlines()
start = lines.index('          wait_for_revision() {')
end = lines.index('          }', start + 1)
FUNCTION = '\n'.join(line[10:] for line in lines[start:end + 1])

FIXTURE = r'''
set -euo pipefail
SECONDS=0
RESOURCE_GROUP=fixture
image_ref=expected-digest
sleep() { SECONDS=$((SECONDS + ${CLOCK_STEP:-$1})); }
timeout() { shift; "$@"; }
az() {
  local kind n
  case "$1 $2 $3" in
    'containerapp show --resource-group') kind=app ;;
    'containerapp revision show') kind=image ;;
    *) echo 'unexpected CLI command' >&2; return 99 ;;
  esac
  n=$(cat "$COUNTERS/$kind" 2>/dev/null || true)
  n=$((${n:-0} + 1))
  echo "$n" > "$COUNTERS/$kind"
  if [[ "$SCENARIO" == auth ]]; then
    echo 'ERROR: AuthorizationFailed; PRIVATE_MARKER' >&2
    return 1
  fi
  if [[ "$SCENARIO" == aadsts ]]; then
    echo 'ERROR: AADSTS700213; PRIVATE_MARKER' >&2
    return 1
  fi
  if [[ "$SCENARIO" == token ]]; then
    echo 'ERROR: InvalidAuthenticationToken; PRIVATE_MARKER' >&2
    return 1
  fi
  if [[ "$SCENARIO" == config ]]; then
    echo 'ERROR: unrecognized arguments; PRIVATE_MARKER' >&2
    return 1
  fi
  if [[ "$SCENARIO" == permanent-app || ( "$SCENARIO" == first-app && "$kind" == app && "$n" == 1 ) ||
        ( "$SCENARIO" == permanent-image && "$kind" == image ) ||
        ( "$SCENARIO" == first-image && "$kind" == image && "$n" == 1 ) ]]; then
    echo 'ERROR: Expecting property name enclosed in double quotes; PRIVATE_MARKER' >&2
    return 1
  fi
  if [[ "$kind" == app ]]; then
    if [[ "$SCENARIO" == different-revision ]]; then echo 'candidate|previous'; else echo 'candidate|candidate'; fi
  elif [[ "$SCENARIO" == different-image || "$SCENARIO" == deadline ]]; then echo wrong-digest
  else echo expected-digest
  fi
}
'''


class ReadinessTests(unittest.TestCase):
    def run_poll(self, scenario, clock_step=10):
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run(['bash', '-c', FIXTURE + '\n' + FUNCTION + '\nwait_for_revision fixture'],
                                    env=os.environ | {'SCENARIO': scenario, 'COUNTERS': directory,
                                                      'CLOCK_STEP': str(clock_step)},
                                    text=True, capture_output=True, timeout=5)
            counts = {name: int((Path(directory) / name).read_text()) if (Path(directory) / name).exists() else 0
                      for name in ('app', 'image')}
            return result, counts

    def test_exact_ready(self):
        result, counts = self.run_poll('ready')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(counts, {'app': 1, 'image': 1})

    def test_read_error_then_verified_readiness(self):
        for scenario in ('first-app', 'first-image'):
            with self.subTest(scenario=scenario):
                result, counts = self.run_poll(scenario)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(counts['app'], 2)
                self.assertNotIn('PRIVATE_MARKER', result.stdout + result.stderr)

    def test_persistent_errors_stop_after_three_reads(self):
        for scenario, kind in (('permanent-app', 'app'), ('permanent-image', 'image')):
            with self.subTest(scenario=scenario):
                result, counts = self.run_poll(scenario)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(counts[kind], 3)
                self.assertNotIn('PRIVATE_MARKER', result.stdout + result.stderr)

    def test_permission_and_input_errors_fail_fast(self):
        for scenario in ('auth', 'aadsts', 'token', 'config'):
            result, counts = self.run_poll(scenario)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(counts, {'app': 1, 'image': 0})
            self.assertNotIn('PRIVATE_MARKER', result.stdout + result.stderr)

    def test_mismatched_identity_never_becomes_ready(self):
        for scenario in ('different-image', 'different-revision'):
            result, counts = self.run_poll(scenario)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(counts['app'], 60)

    def test_deadline_is_not_reset_between_observations(self):
        result, counts = self.run_poll('deadline', clock_step=300)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(counts['app'], 2)


if __name__ == '__main__':
    unittest.main()
