#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('probe', Path(__file__).with_name('probe-dapr-readiness.py'))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)
ACTUAL_THROTTLE = Path(__file__).with_name('testdata').joinpath('exec-handshake-429.txt').read_text()
THROTTLED = ACTUAL_THROTTLE.replace("{'retry-after': '600'}\n", '')
READY = 'HTTP/1.1 200 OK\n{"status":"ready"}'


class ProbeTests(unittest.TestCase):
    def run_probe(self, results, times=None):
        with patch.object(probe.subprocess, 'run', side_effect=results) as run, \
             patch.object(probe.time, 'sleep') as sleep, \
             patch.object(probe.time, 'monotonic', side_effect=times, return_value=0):
            status = probe.probe('alive', 'api-gateway')
        return status, run, sleep

    def test_throttle_then_success(self):
        status, run, sleep = self.run_probe([subprocess.CompletedProcess([], 1, '', THROTTLED),
                                          subprocess.CompletedProcess([], 0, READY, '')])
        self.assertEqual(status, 0)
        self.assertEqual(run.call_count, 2)
        sleep.assert_called_once_with(2)
        self.assertEqual(run.call_args.args[0][:3], ['timeout', '60.000s', 'script'])
        self.assertIn('/invoke/notification-api/method/ready', run.call_args.args[0][-2])

    def test_exhausted(self):
        status, run, sleep = self.run_probe([subprocess.CompletedProcess([], 1, '', THROTTLED)] * 3)
        self.assertEqual(status, 1)
        self.assertEqual(run.call_count, 3)
        self.assertEqual([call.args[0] for call in sleep.call_args_list], [2, 4])

    def test_http_failure_and_unknown_never_retry(self):
        for output, code in [('HTTP/1.1 503 Unavailable', 1), ('HTTP/1.1 429 Too Many Requests', 1),
                             ('HTTP/1.1 503\n' + THROTTLED, 1), ('HTTP/2 429\n' + THROTTLED, 1),
                             ('unknown exec error', 1), ('HTTP/1.1 200 OK\n{}', 0),
                             (THROTTLED, 124), (READY, 1)]:
            with self.subTest(output=output):
                status, run, sleep = self.run_probe([subprocess.CompletedProcess([], code, output, '')])
                self.assertEqual(status, 1)
                self.assertEqual(run.call_count, 1)
                sleep.assert_not_called()

    def test_actual_server_cooldown_exceeds_budget(self):
        status, run, sleep = self.run_probe([subprocess.CompletedProcess([], 1, '', ACTUAL_THROTTLE)])
        self.assertEqual(status, 1)
        self.assertEqual(run.call_count, 1)
        sleep.assert_not_called()

    def test_short_retry_after_is_respected(self):
        status, run, sleep = self.run_probe([subprocess.CompletedProcess([], 1, '', THROTTLED + "{'retry-after': '5'}"),
                                          subprocess.CompletedProcess([], 0, READY, '')])
        self.assertEqual(status, 0)
        sleep.assert_called_once_with(5)

    def test_invalid_retry_after_fails_closed(self):
        for header in ["{'retry-after': 'unknown'}", 'retry-after malformed']:
            status, run, sleep = self.run_probe([subprocess.CompletedProcess([], 1, '', THROTTLED + header)])
            self.assertEqual(status, 1)
            self.assertEqual(run.call_count, 1)
            sleep.assert_not_called()

    def test_http_date_retry_after(self):
        with patch.object(probe.time, 'time', return_value=0):
            self.assertEqual(probe.retry_after("{'retry-after': 'Thu, 01 Jan 1970 00:00:05 GMT'}"), 5)

    def test_shared_deadline_not_reset(self):
        status, run, sleep = self.run_probe([subprocess.CompletedProcess([], 1, '', THROTTLED)], [0, 58, 59])
        self.assertEqual(status, 1)
        self.assertEqual(run.call_args.args[0][1], '2.000s')
        sleep.assert_not_called()


if __name__ == '__main__':
    unittest.main()
