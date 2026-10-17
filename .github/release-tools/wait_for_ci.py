#!/usr/bin/env python3
"""Reuse the latest branch/manual CI run for the exact release commit."""
import argparse
import json
import re
import subprocess
import time


def latest_run(runs, sha):
    eligible = [run for run in runs
                if run['head_sha'] == sha
                and run['event'] in ('push', 'workflow_dispatch')
                # Older tag-triggered CI must not be mistaken for branch CI.
                and not re.fullmatch(r'v\d+\.\d+\.\d+(?:-[\w.-]+)?', run['head_branch'] or '')]
    return max(eligible, key=lambda run: run['run_number'], default=None)


def fetch_runs(repo, sha):
    result = subprocess.check_output([
        'gh', 'api', '--method', 'GET',
        f'repos/{repo}/actions/workflows/backend-ci.yml/runs',
        '-f', f'head_sha={sha}', '-f', 'per_page=100',
    ], text=True)
    return json.loads(result)['workflow_runs']


def wait_for_ci(repo, sha, timeout=2700, poll=15):
    deadline = time.monotonic() + timeout
    missing_deadline = time.monotonic() + min(timeout, 60)
    previous = None
    while True:
        run = latest_run(fetch_runs(repo, sha), sha)
        if run is not None:
            state = (run['id'], run['status'], run['conclusion'])
            if state != previous:
                print(f"CI: {run['html_url']} ({run['status']}, {run['conclusion']})", flush=True)
                previous = state
            if run['status'] == 'completed':
                if run['conclusion'] != 'success':
                    raise RuntimeError('Latest CI did not pass; fix or rerun that CI before releasing.')
                print(f'Reusing successful CI for {sha}; no duplicate test run.', flush=True)
                return
        elif time.monotonic() >= missing_deadline:
            raise RuntimeError(
                'No branch CI found for this commit. Push the commit to a branch, '
                'or run backend-ci.yml manually on a branch pointing to it, then retry Release.')
        if time.monotonic() >= deadline:
            raise RuntimeError('Timed out waiting for CI; retry Release after that CI finishes.')
        time.sleep(poll)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo', required=True)
    parser.add_argument('--sha', required=True)
    parser.add_argument('--timeout', type=int, default=2700)
    args = parser.parse_args()
    if not re.fullmatch(r'[\w.-]+/[\w.-]+', args.repo) or not re.fullmatch(r'[0-9a-f]{40}', args.sha):
        parser.error('expected owner/repository and a full commit SHA')
    if args.timeout <= 0:
        parser.error('--timeout must be positive')
    wait_for_ci(args.repo, args.sha, args.timeout)


if __name__ == '__main__':
    main()
