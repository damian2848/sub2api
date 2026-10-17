import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('wait_for_ci', Path(__file__).with_name('wait_for_ci.py'))
ci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ci)
SHA = 'a' * 40


def run(number=1, status='completed', conclusion='success', **kwargs):
    return dict(id=number, run_number=number, status=status, conclusion=conclusion,
                html_url=f'https://example.test/runs/{number}', head_sha=SHA,
                event='push', head_branch='main') | kwargs


class WaitForCITest(unittest.TestCase):
    def test_requires_exact_commit_and_branch_ci(self):
        self.assertIsNone(ci.latest_run([
            run(head_sha='b' * 40), run(event='pull_request'),
            run(head_branch='v1.2.3'), run(head_branch='v1.2.3-rc.1'),
        ], SHA))
        manual = run(event='workflow_dispatch')
        self.assertEqual(ci.latest_run([manual], SHA), manual)

    def test_reuses_success_without_waiting(self):
        with patch.object(ci, 'fetch_runs', return_value=[run()]), patch.object(ci.time, 'sleep') as sleep:
            ci.wait_for_ci('owner/repo', SHA)
            sleep.assert_not_called()

    def test_newer_failure_blocks_even_if_old_run_passed(self):
        with patch.object(ci, 'fetch_runs', return_value=[run(), run(2, conclusion='failure')]):
            with self.assertRaisesRegex(RuntimeError, 'did not pass'):
                ci.wait_for_ci('owner/repo', SHA)

    def test_waits_for_registration_and_completion(self):
        with patch.object(ci, 'fetch_runs', side_effect=[[], [run(status='in_progress', conclusion=None)], [run()]]), patch.object(ci.time, 'sleep') as sleep:
            ci.wait_for_ci('owner/repo', SHA)
            self.assertEqual(sleep.call_count, 2)

    def test_missing_ci_and_timeout_never_allow_release(self):
        for runs, message in [([], 'No branch CI'), ([run(status='queued', conclusion=None)], 'Timed out')]:
            with self.subTest(message=message), patch.object(ci, 'fetch_runs', return_value=runs), patch.object(ci.time, 'monotonic', side_effect=[0, 0, 100, 100]):
                with self.assertRaisesRegex(RuntimeError, message):
                    ci.wait_for_ci('owner/repo', SHA, timeout=60)

    def test_api_errors_never_allow_release(self):
        with patch.object(ci, 'fetch_runs', side_effect=RuntimeError('API failure')):
            with self.assertRaisesRegex(RuntimeError, 'API failure'):
                ci.wait_for_ci('owner/repo', SHA)


if __name__ == '__main__':
    unittest.main()
