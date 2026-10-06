"""Local pure-function/control-flow tests. Never contacts Docker, SSH or production."""
import io
import json
from pathlib import Path
import subprocess
import shutil
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import deploy_remote as d

OUT = Path(__file__).parent


class RolloutTests(unittest.TestCase):
    def test_release_code_digest_matches_node(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            p = Path(root)
            (p / 'src/nested').mkdir(parents=True)
            for name in ['package.json', 'package-lock.json', 'THIRD_PARTY_NOTICES.md', 'src/test.mjs', 'src/nested/test.mjs']:
                (p / name).write_text('value ' + name)
            code = d.NODE_DIGEST.replace("'/app'", repr(str(p)))
            actual = subprocess.check_output(['node', '-e', code, 'code'], text=True).strip()
            self.assertEqual(d.release_code_digest(p), actual)

    def test_configuration_preserves_environment(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            result = d.configuration_expected({'PRISM_HTTP_CACHE': 'true', 'PRISM_MEMORY_LIMIT_MIB': '256',
                                               'PRISM_MEMORY_RESERVE_MIB': '64', 'PRISM_PREWARM_CHAT': 'false'}, Path(root))
            self.assertTrue(result['effective']['enabled'])
            self.assertTrue(result['effective']['http_cache'])
            self.assertEqual(result['effective']['memory_limit_mib'], 256)
            self.assertEqual(result['effective']['memory_reserve_mib'], 64)
            self.assertFalse(result['effective']['prewarm_chat'])
            self.assertEqual(result['source'], 'environment')
            self.assertFalse(result['restart_required'])
            with self.assertRaises(RuntimeError):
                d.configuration_expected({'PRISM_MEMORY_LIMIT_MIB': '10'}, Path(root))

    def test_configuration_migrates_legacy_saved_switch(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            p = Path(root)
            legacy = {field: default for field, (_, default) in d.CONFIG_FIELDS.items() if field != 'enabled'}
            legacy['http_cache'] = True
            (p / 'runtime-config.json').write_text(json.dumps({'schema': 1, 'values': legacy}))
            actual = d.configuration_expected({'PRISM_BROWSER_ENABLED': 'false'}, p)
            self.assertFalse(actual['effective']['enabled'])
            self.assertTrue(actual['effective']['http_cache'])
            self.assertEqual(actual['effective'], {**legacy, 'enabled': False})
            self.assertEqual(actual['source'], 'saved')

    def test_configuration_rejects_partial_or_unknown_saved_switch(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            p = Path(root)
            legacy = {field: default for field, (_, default) in d.CONFIG_FIELDS.items() if field != 'enabled'}
            for values in [
                {**legacy, 'enabled': True, 'unknown': False},
                {key: value for key, value in legacy.items() if key != 'http_cache'},
            ]:
                (p / 'runtime-config.json').write_text(json.dumps({'schema': 1, 'values': values}))
                with self.assertRaises(RuntimeError):
                    d.configuration_expected({}, p)

    def test_configuration_saved_override(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            p = Path(root)
            expected = {field: default for field, (_, default) in d.CONFIG_FIELDS.items()}
            expected['http_cache'] = True
            (p / 'runtime-config.json').write_text(json.dumps({'schema': 1, 'values': expected}))
            actual = d.configuration_expected({'PRISM_HTTP_CACHE': 'false'}, p)
            self.assertEqual(actual['effective'], expected)
            self.assertEqual(actual['source'], 'saved')

    def test_checksum_entry_rejects_duplicate(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            p = Path(root) / 'checksums.txt'
            p.write_text('a' * 64 + '  archive.tar.gz\n')
            self.assertEqual(d.checksum_entry(p, 'archive.tar.gz'), 'a' * 64)
            p.write_text(p.read_text() * 2)
            with self.assertRaises(RuntimeError):
                d.checksum_entry(p, 'archive.tar.gz')

    def test_archive_rejects_traversal_links_duplicates(self):
        for names in [['../escape'], ['/absolute'], ['duplicate', 'duplicate'], ['link']]:
            with self.subTest(names=names), tempfile.TemporaryDirectory(dir=OUT) as root:
                p = Path(root)
                archive = p / 'archive.tar.gz'
                with tarfile.open(archive, 'w:gz') as tar:
                    for name in names:
                        info = tarfile.TarInfo(name)
                        info.size = 3
                        if name == 'link':
                            info.type = tarfile.SYMTYPE
                            info.linkname = '/outside'
                            info.size = 0
                        tar.addfile(info, None if name == 'link' else io.BytesIO(b'abc'))
                with self.assertRaises(RuntimeError):
                    d.safe_extract(archive, p / 'extracted')

    def test_valid_archive_extracts(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            p = Path(root)
            archive = p / 'archive.tar.gz'
            with tarfile.open(archive, 'w:gz') as tar:
                info = tarfile.TarInfo('source/sub2api')
                info.mode = 0o755
                info.size = 3
                tar.addfile(info, io.BytesIO(b'abc'))
            d.safe_extract(archive, p / 'extracted')
            self.assertEqual((p / 'extracted/source/sub2api').read_bytes(), b'abc')
            self.assertEqual((p / 'extracted/source/sub2api').stat().st_mode & 0o777, 0o755)

    def test_migration_verification_with_and_without_new_migrations(self):
        before = {'263.sql': 'baseline'}
        with patch.object(d, 'EXPECTED_MIGRATIONS', {}):
            self.assertEqual(d.verify_migrations(before, before), {})
            d.verify_migrations(before, before, rolled_back=True)
            with self.assertRaises(RuntimeError):
                d.verify_migrations(before, before | {'266_new.sql': 'x'})
            with self.assertRaises(RuntimeError):
                d.verify_migrations(before, {**before, '263.sql': 'changed'})
        wanted = {'266_new.sql': 'a' * 64}
        with patch.object(d, 'EXPECTED_MIGRATIONS', wanted):
            self.assertEqual(d.verify_migrations(before, before | wanted), wanted)
            with self.assertRaises(RuntimeError):
                d.verify_migrations(before, before)  # the new binary must have applied it
            with self.assertRaises(RuntimeError):
                d.verify_migrations(before, before | {'266_new.sql': 'b' * 64})  # wrong checksum

    def test_sidecar_command_override_is_explicit_and_nothing_else_changes(self):
        base = {'Cmd': ['node', 'src/server.mjs'], 'Entrypoint': None, 'User': '', 'WorkingDir': '/app',
                'ExposedPorts': {'8319/tcp': {}}, 'StopSignal': ''}
        old = {'Config': base}
        new = {'Config': base | {'Cmd': ['node', 'src/supervisor.mjs']}}
        with patch.object(d, 'SIDECAR_CMD_OVERRIDE', None):
            self.assertTrue(d.command_matches(d.SIDECAR, old, old))
            self.assertFalse(d.command_matches(d.SIDECAR, new, old), 'no override: a changed command is refused')
        with patch.object(d, 'SIDECAR_CMD_OVERRIDE', ['node', 'src/supervisor.mjs']):
            self.assertTrue(d.command_matches(d.SIDECAR, new, old))
            self.assertFalse(d.command_matches(d.SIDECAR, old, old), 'with an override the old command is wrong')
            self.assertFalse(d.command_matches(d.SIDECAR, new | {'Config': new['Config'] | {'User': 'root'}}, old))
            self.assertFalse(d.command_matches(d.APP, new, old), 'the app command may never change')

    def test_missing_ledger_is_never_statically_referenced(self):
        queries = []
        def fake_sql(query, **_kwargs):
            queries.append(query)
            return 'f' if len(queries) == 1 else '{}'
        with patch.object(d, 'sql', fake_sql):
            self.assertEqual(d.raw_facts_fingerprint(), {})
        self.assertEqual(len(queries), 2)
        self.assertNotIn('FROM probe_request_facts', queries[1])
        self.assertIn("'probe_facts', NULL", queries[1])

    def test_present_ledger_is_included(self):
        queries = []
        def fake_sql(query, **_kwargs):
            queries.append(query)
            return 't' if len(queries) == 1 else '{}'
        with patch.object(d, 'sql', fake_sql):
            d.raw_facts_fingerprint()
        self.assertIn('FROM probe_request_facts', queries[1])

    def test_interrupted_directory_swap_recovers_from_journal(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            p = Path(root)
            live = p / 'prism-browser-data'
            live.mkdir()
            (live / 'session.json').write_text('candidate')
            backup = p / 'backup'
            backup.mkdir()
            snapshot = backup / 'prism-data.snapshot'
            snapshot.mkdir()
            (snapshot / 'session.json').write_text('original')
            plan = {'prism_snapshot_ready': True, 'candidate_started': True, 'prism_data_sha': d.tree_digest(snapshot)}
            old_rename = Path.rename
            def injected_rename(path, target):
                if path.name.startswith('prism-browser-data.restore-'):
                    raise InterruptedError('injected after first directory rename')
                return old_rename(path, target)
            def fake_cp(args, **_kwargs):
                self.assertEqual(args[:4], ['cp', '-a', '--reflink=auto', '--'])
                shutil.copytree(args[4], args[5], symlinks=True)
                return b''
            with patch.object(d, 'PRISM_DATA', live), patch.object(d, 'run', fake_cp):
                with patch.object(Path, 'rename', injected_rename):
                    with self.assertRaises(InterruptedError):
                        d.restore_prism_data(backup, plan)
                self.assertFalse(live.exists())
                self.assertEqual(json.loads((backup / 'prism-data-restore-journal.json').read_text())['phase'], 'prepared')
                d.restore_prism_data(backup, plan)
            self.assertEqual((live / 'session.json').read_text(), 'original')
            self.assertEqual(d.tree_digest(live), plan['prism_data_sha'])
            self.assertEqual(json.loads((backup / 'prism-data-restore-journal.json').read_text())['phase'], 'restored')
            failed = list(p.glob('prism-browser-data.failed-*'))
            self.assertEqual(len(failed), 1)
            self.assertEqual((failed[0] / 'session.json').read_text(), 'candidate')

    def test_missing_data_without_journal_is_fail_closed(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            p = Path(root)
            backup = p / 'backup'
            backup.mkdir()
            snapshot = backup / 'prism-data.snapshot'
            snapshot.mkdir()
            plan = {'prism_snapshot_ready': True, 'candidate_started': True, 'prism_data_sha': d.tree_digest(snapshot)}
            with patch.object(d, 'PRISM_DATA', p / 'prism-browser-data'):
                with self.assertRaises(RuntimeError):
                    d.restore_prism_data(backup, plan)

    def test_safe_failure_before_candidate_no_data_restore(self):
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            d.restore_prism_data(Path(root), {'prism_snapshot_ready': False, 'candidate_started': False})
            with self.assertRaises(RuntimeError):
                d.restore_prism_data(Path(root), {'prism_snapshot_ready': False, 'candidate_started': True})


class ReleaseParameterTests(unittest.TestCase):
    def test_download_verifies_checksums_and_the_named_revision(self):
        version, revision = '9.9.9', 'a' * 40
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            stage = Path(root) / 'stage'
            archive = Path(root) / 'src.tar.gz'
            fake = Path(root) / 'sub2api'
            fake.write_text('#!/bin/sh\necho "Sub2API 9.9.9 (commit: ' + revision + ', built: x)"\n'); fake.chmod(0o755)
            with tarfile.open(archive, 'w:gz') as t: t.add(fake, arcname='sub2api')
            prism = Path(root) / 'prism.tar.gz'
            with tarfile.open(prism, 'w:gz') as t: t.add(fake, arcname='p/x')
            main_sha, prism_sha = d.file_sha(archive), d.file_sha(prism)
            def fake_run(args, **kw):
                if args[0] == 'curl':
                    name = args[-1].rsplit('/', 1)[1]
                    out = Path(args[args.index('--output') + 1])
                    # Same layout as a real release: checksums.txt lists the platform binaries only.
                    out.write_bytes({'checksums.txt': f'{main_sha}  sub2api_{version}_linux_amd64.tar.gz\n{"c" * 64}  sub2api_{version}_darwin_arm64.tar.gz\n'.encode(),
                                     f'sub2api_{version}_linux_amd64.tar.gz': archive.read_bytes(),
                                     f'prism-browser_{version}.tar.gz': prism.read_bytes(),
                                     f'prism-browser_{version}.tar.gz.sha256': f'{prism_sha}  prism-browser_{version}.tar.gz\n'.encode()}[name])
                    return b''
                return subprocess.run(args, capture_output=True, check=True).stdout
            with patch.object(d, 'run', fake_run):
                paths = d.download_release(version, revision, stage)
                self.assertEqual(len(paths), 4)
                with self.assertRaises(RuntimeError):  # the tag does not point at the revision that was approved
                    d.download_release(version, 'b' * 40, Path(root) / 'stage2')

    def test_prism_package_is_verified_by_its_own_sha256_not_checksums_txt(self):
        version, revision = '9.9.9', 'a' * 40
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            fake = Path(root) / 'sub2api'
            fake.write_text('#!/bin/sh\necho "Sub2API 9.9.9 (commit: ' + revision + ', built: x)"\n'); fake.chmod(0o755)
            main = Path(root) / 'main.tar.gz'
            with tarfile.open(main, 'w:gz') as t: t.add(fake, arcname='sub2api')
            prism = Path(root) / 'prism.tar.gz'
            with tarfile.open(prism, 'w:gz') as t: t.add(fake, arcname='p/x')
            sha = {'main': d.file_sha(main), 'prism': d.file_sha(prism)}
            served = {'checksums.txt': f'{sha["main"]}  sub2api_{version}_linux_amd64.tar.gz\n'.encode(),  # no prism entry
                      f'sub2api_{version}_linux_amd64.tar.gz': main.read_bytes(), f'prism-browser_{version}.tar.gz': prism.read_bytes(),
                      f'prism-browser_{version}.tar.gz.sha256': f'{sha["prism"]}  prism-browser_{version}.tar.gz\n'.encode()}
            def fake_run(args, **kw):
                if args[0] == 'curl':
                    Path(args[args.index('--output') + 1]).write_bytes(served[args[-1].rsplit('/', 1)[1]]); return b''
                return subprocess.run(args, capture_output=True, check=True).stdout
            with patch.object(d, 'run', fake_run):
                self.assertEqual(len(d.download_release(version, revision, Path(root) / 'ok')), 4)
                served[f'prism-browser_{version}.tar.gz.sha256'] = f'{"0" * 64}  prism-browser_{version}.tar.gz\n'.encode()
                with self.assertRaises(RuntimeError):  # a wrong .sha256 must still be caught
                    d.download_release(version, revision, Path(root) / 'bad')

    def test_download_rejects_a_tampered_archive(self):
        version, revision = '9.9.9', 'a' * 40
        with tempfile.TemporaryDirectory(dir=OUT) as root:
            def fake_run(args, **kw):
                name = args[-1].rsplit('/', 1)[1]
                Path(args[args.index('--output') + 1]).write_bytes(
                    b'0' * 64 + b'  x\n' if name == 'checksums.txt' else b'not what was published')
                return b''
            with patch.object(d, 'run', fake_run):
                with self.assertRaises(RuntimeError):
                    d.download_release(version, revision, Path(root) / 'stage')

    def test_wait_for_idle_waits_then_proceeds_and_gives_up(self):
        states = iter([True, True, False, False, False, False, False])
        clock = {'now': 0.0}
        with patch.object(d, 'sidecar_busy', lambda: next(states)), \
             patch.object(d.time, 'monotonic', lambda: clock['now']), \
             patch.object(d.time, 'sleep', lambda seconds: clock.__setitem__('now', clock['now'] + seconds)):
            d.wait_for_idle(600, quiet_seconds=10)
        with patch.object(d, 'sidecar_busy', lambda: True), \
             patch.object(d.time, 'monotonic', lambda: clock['now']), \
             patch.object(d.time, 'sleep', lambda seconds: clock.__setitem__('now', clock['now'] + seconds)):
            with self.assertRaises(RuntimeError):
                d.wait_for_idle(30)

    def test_sidecar_busy_counts_unfinished_turns_only(self):
        def logs(*events):
            lines = [f'2026-10-03T00:00:0{i}Z ' + json.dumps({'event': e}) for i, e in enumerate(events)]
            return ('\n'.join(lines)).encode()
        with patch.object(d, 'run', lambda *a, **k: logs('upstream_start', 'upstream_result')):
            self.assertFalse(d.sidecar_busy())
        with patch.object(d, 'run', lambda *a, **k: logs('upstream_start', 'upstream_start', 'upstream_result')):
            self.assertTrue(d.sidecar_busy())
        with patch.object(d, 'run', lambda *a, **k: logs('upstream_start', 'browser_ui_failure')):
            self.assertFalse(d.sidecar_busy())


if __name__ == '__main__':
    unittest.main(verbosity=2)
