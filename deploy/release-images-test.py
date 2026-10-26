import argparse
import contextlib
import copy
import importlib.util
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('release_images', pathlib.Path(__file__).with_name('release-images.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
REG = 'ghcr.io/example'
PREFIX = 'deck-deploy'
SHA = 'a' * 40


def document(sha=SHA):
    return dict(schema=1, sha=sha, uid=1003, gid=1003,
                playwright='1.64.1', mempalace='3.10.0', go='1.26.10',
                font_files={'Euclid.woff2': 'b' * 64},
                images={role: REG + '/' + PREFIX + '-' + role + '@sha256:' + str(i) * 64
                        for i, role in enumerate(('free', 'licensed', 'browser'), 1)})


class ReleaseImagesTest(unittest.TestCase):
    def test_compare_api_failure_names_the_failed_source_comparison(self):
        path = 'repos/example/deck/compare/' + SHA + '...main'
        failure = module.urllib.error.HTTPError('https://api.github.com/' + path, 404, 'not found', {}, None)
        with patch.dict(os.environ, GH_TOKEN='test-only'), \
             patch.object(module.urllib.request, 'urlopen', side_effect=failure), \
             self.assertRaisesRegex(RuntimeError, 'GET ' + path.replace('.', r'\.') + ' returned HTTP 404'):
            module.can_promote('example/deck', SHA)

    def test_publish_does_not_move_catalog_pointers_when_promotion_is_refused(self):
        repo = 'example/deck'
        old = (document('c' * 40), REG + '/' + PREFIX + '-catalog@sha256:' + '9' * 64)
        args = argparse.Namespace(registry=REG, prefix=PREFIX, sha=SHA, uid=1003, gid=1003,
                                  font_context='', require_licensed=False)
        def push(ref):
            return ref.rsplit(':', 1)[0] + '@sha256:' + '8' * 64
        def command(*args, **kwargs):
            return SHA if args == ('git', 'rev-parse', 'HEAD') else ''
        for allowed in (False, True):
            with self.subTest(allowed=allowed), contextlib.chdir(pathlib.Path(__file__).resolve().parent.parent), \
                 patch.dict(os.environ, GITHUB_REPOSITORY=repo), patch.object(module, 'private_package', return_value={'visibility': 'private'}), \
                 patch.object(module, 'catalog', side_effect=[old, None]), patch.object(module, 'probe'), \
                 patch.object(module.subprocess, 'run'), patch.object(module, 'run', side_effect=command) as run, \
                 patch.object(module, 'push', side_effect=push) as pushed, patch.object(module, 'prune_registry') as prune, \
                 patch.object(module, 'can_promote', return_value=allowed) as promote:
                module.publish(args)
                promote.assert_called_once_with(repo, SHA, old[0]['sha'])
                tags = [call.args[-1] for call in run.call_args_list if call.args[:2] == ('docker', 'tag')]
                pointers = [call.args[0] for call in pushed.call_args_list if call.args[0].endswith((':latest', ':previous'))]
                expected = [REG + '/' + PREFIX + '-catalog:' + tag for tag in ('previous', 'latest')] if allowed else []
                self.assertEqual(tags, expected)
                self.assertEqual(pointers, expected)
                self.assertEqual(prune.called, allowed)

    def test_completed_accepted_build_survives_main_advancing(self):
        repo = 'example/deck'
        previous = 'c' * 40
        with patch.object(module, 'api', side_effect=[{'status': 'ahead'}, {'status': 'ahead'}]) as api:
            self.assertTrue(module.can_promote(repo, SHA, previous))
        self.assertEqual([call.args[0] for call in api.call_args_list], [
            'repos/' + repo + '/compare/' + SHA + '...main',
            'repos/' + repo + '/compare/' + previous + '...' + SHA])
        with patch.object(module, 'api', return_value={'status': 'ahead'}):
            self.assertTrue(module.can_promote(repo, SHA))
        with patch.object(module, 'api', return_value={'status': 'identical'}):
            self.assertTrue(module.can_promote(repo, SHA, SHA))

    def test_promotion_refuses_divergence_and_catalog_downgrade(self):
        for statuses in [('behind',), ('diverged',), ('ahead', 'behind'), ('ahead', 'diverged')]:
            with self.subTest(statuses=statuses), patch.object(module, 'api', side_effect=[{'status': s} for s in statuses]):
                self.assertFalse(module.can_promote('example/deck', SHA, 'c' * 40))
        with patch.object(module, 'api', side_effect=RuntimeError('API unavailable')), self.assertRaises(RuntimeError):
            module.can_promote('example/deck', SHA)

    def test_admission_coalesces_on_latest_successful_owned_main(self):
        repo = 'example/deck'
        def run(sha, owner=repo, branch='main', conclusion='success',
                name='Release acceptance', path='.github/workflows/release-acceptance.yaml'):
            return dict(head_sha=sha, head_repository={'full_name': owner}, head_branch=branch,
                        conclusion=conclusion, name=name, path=path)
        repairs = [run('b' * 40, name='CI repair acceptance'),
                   run('b' * 40, path='.github/workflows/ci-heal-acceptance.yaml')]
        runs = repairs + [run('b' * 40, owner='other/deck'), run('b' * 40, branch='feature'),
                run('b' * 40, conclusion='failure'), run(SHA), run('c' * 40)]
        workflow = pathlib.Path(__file__).resolve().parent.parent / '.github/workflows/release-images.yaml'
        script = textwrap.dedent(workflow.read_text().split("python3 - <<'PY'\n", 1)[1].split('\n          PY', 1)[0])
        for selected, records, status, admitted, error in [
                (SHA, runs, 'ahead', True, None),
                (SHA, runs, 'identical', True, None),
                ('c' * 40, runs, 'ahead', False, 0),
                (SHA, [], 'ahead', False, 'No successful exact-main acceptance'),
                (SHA, repairs, 'ahead', False, 'No successful exact-main acceptance'),
                (SHA, runs, 'diverged', False, 'Accepted source is no longer on current mainline'),
                ('bad', runs, 'ahead', False, 'Invalid release SHA')]:
            with self.subTest(selected=selected, status=status, error=error), tempfile.TemporaryDirectory() as directory:
                output = pathlib.Path(directory, 'output')
                def api(command, **kwargs):
                    if '/actions/workflows/' in command[-1]:
                        return json.dumps({'workflow_runs': records})
                    self.assertEqual(command[-1], 'repos/' + repo + '/compare/' + selected + '...main')
                    return json.dumps({'status': status})
                with patch.dict(os.environ, RELEASE_SHA=selected, GITHUB_REPOSITORY=repo, GITHUB_OUTPUT=str(output)), \
                     patch.object(subprocess, 'check_output', side_effect=api):
                    if error is None:
                        exec(compile(script, str(workflow), 'exec'), {})
                    else:
                        with self.assertRaises(SystemExit) as caught:
                            exec(compile(script, str(workflow), 'exec'), {})
                        self.assertEqual(caught.exception.code, error)
                self.assertEqual(output.exists(), admitted)
                if admitted:
                    self.assertEqual(output.read_text(), 'accepted=true\n')

    @unittest.skipUnless(shutil.which('docker'), 'Docker CLI is not installed')
    def test_real_compose_build_graphs_need_no_operator_configuration(self):
        repo = pathlib.Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            envfile = module.write_build_env(root)
            for role in ('free', 'licensed'):
                with self.subTest(role=role):
                    env = {'PATH': os.environ['PATH'], 'HOME': str(root),
                           'NOISEDECK_FONTS': role, 'NOISEDECK_BUILD_VERSION': SHA,
                           'NOISEDECK_RUNTIME_UID': '1003', 'NOISEDECK_RUNTIME_GID': '1003'}
                    files = list(module.BUILD_COMPOSE_FILES)
                    if role == 'licensed':
                        env['NOISEDECK_LICENSED_FONTS_DIR'] = 'docker-image://' + REG + '/fonts@sha256:' + '1' * 64
                        files += module.LICENSED_COMPOSE_FILES
                    command = ['docker', 'compose', '--project-name', 'deck-release-images-test',
                               '--env-file', str(envfile)]
                    for file in files:
                        command += ['--file', file]
                    doc = json.loads(subprocess.check_output(command + ['config', '--format', 'json'],
                                                             cwd=repo, env=env, text=True))
                    self.assertEqual(doc['services']['go-base']['build']['args']['NOISEDECK_FONTS'], role)
                    self.assertTrue(any(v.get('source') == str(root / 'unused-mempalace')
                                        for v in doc['services']['app']['volumes']))
                    self.assertFalse((root / 'unused-mempalace').exists())

    def test_build_env_satisfies_runtime_mount_interpolation_without_data(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            envfile = module.write_build_env(root)
            self.assertEqual(envfile.read_text().splitlines(), [
                'NOISEDECK_IMAGE_TAG=release-image-build',
                'NOISEDECK_MEMPALACE_HOST_PATH=' + str(root / 'unused-mempalace')])
            self.assertFalse((root / 'unused-mempalace').exists())

    def test_manifest_rejects_unsafe_or_partial_sets(self):
        module.validate(document(), REG, PREFIX, True)
        for key, value in [('sha', 'main'), ('uid', True), ('gid', 0),
                           ('playwright', '^1.64.1'), ('font_files', {}),
                           ('images', {'free': document()['images']['free']})]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                module.validate(dict(document(), **{key: value}), REG, PREFIX, True)
        for ref in ['evil.example/a@sha256:' + '1' * 64,
                    REG + '/' + PREFIX + '-free:latest',
                    REG + '/other-free@sha256:' + '1' * 64]:
            doc = document()
            doc['images']['free'] = ref
            with self.assertRaises(ValueError):
                module.validate(doc, REG, PREFIX)

    def test_registry_errors_are_not_absence(self):
        for text in ['unauthorized', 'connection refused', 'denied: requested access']:
            failure = subprocess.CalledProcessError(1, ['docker'], stderr=text)
            with patch.object(module.subprocess, 'run', side_effect=failure), self.assertRaises(subprocess.CalledProcessError):
                module.catalog(REG, PREFIX, 'previous')
        failure = subprocess.CalledProcessError(1, ['docker'], stderr='manifest unknown')
        with patch.object(module.subprocess, 'run', side_effect=failure):
            self.assertIsNone(module.catalog(REG, PREFIX, 'previous'))
        failure = subprocess.CalledProcessError(1, ['docker'], stderr=REG + '/' + PREFIX + '-catalog:previous: not found')
        with patch.object(module.subprocess, 'run', side_effect=failure):
            self.assertIsNone(module.catalog(REG, PREFIX, 'previous'))

    def test_manifest_graph_retains_children_and_attestations(self):
        doc = document()
        child = 'sha256:' + '9' * 64
        def raw(*args):
            return json.dumps({'manifests': [{'digest': child}]} if args[-1] == doc['images']['free'] else {})
        with patch.object(module, 'run', side_effect=raw):
            keep = module.retained_versions([doc], [], REG, PREFIX)
        self.assertEqual(keep['free'], {'sha256:' + '1' * 64, child})
        self.assertEqual(keep['licensed'], {'sha256:' + '2' * 64})

    def test_partial_pull_cannot_replace_host_state(self):
        with tempfile.TemporaryDirectory() as directory:
            target = pathlib.Path(directory, 'sets.json')
            old = {'sets': [document('c' * 40)]}
            target.write_text(json.dumps(old))
            args = argparse.Namespace(state_dir=directory, registry=REG, prefix=PREFIX, require_licensed=True)
            with patch.object(module, 'catalog', side_effect=[(document(), 'catalog'), None]), \
                 patch.object(module, 'run', side_effect=subprocess.CalledProcessError(1, ['docker'])):
                with self.assertRaises(subprocess.CalledProcessError):
                    module.prefetch(args)
            self.assertEqual(json.loads(target.read_text()), old)

    def test_corrupt_old_state_is_rejected_before_registry_access(self):
        with tempfile.TemporaryDirectory() as directory:
            target = pathlib.Path(directory, 'sets.json')
            target.write_text(json.dumps({'sets': [{'schema': 99}]}))
            args = argparse.Namespace(state_dir=directory, registry=REG, prefix=PREFIX, require_licensed=True)
            with patch.object(module, 'catalog') as catalog, self.assertRaises(ValueError):
                module.prefetch(args)
            catalog.assert_not_called()

    def test_unchanged_cache_does_not_run_probe_containers(self):
        with tempfile.TemporaryDirectory() as directory:
            pathlib.Path(directory, 'sets.json').write_text(json.dumps({'sets': [document()]}))
            args = argparse.Namespace(state_dir=directory, registry=REG, prefix=PREFIX, require_licensed=True)
            with patch.object(module, 'catalog', side_effect=[(document(), 'catalog'), None]), \
                 patch.object(module, 'inspect', return_value={}), patch.object(module, 'probe') as probe, \
                 patch.object(module, 'evict', return_value=[]):
                module.prefetch(args)
            probe.assert_not_called()

    def test_selection_admits_exact_source_identity_and_fonts_only(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            compose = root / 'compose.json'
            rendered = {'services': {'app': {'build': {'args': {'VERSION': SHA, 'NOISEDECK_FONTS': 'licensed'}},
                                             'user': '1003:1003', 'image': 'deck:test'},
                                      'chromium': {'image': 'browser:test'}}}
            pathlib.Path(directory, 'sets.json').write_text(json.dumps({'sets': [document()]}))
            args = argparse.Namespace(state_dir=directory, registry=REG, prefix=PREFIX, compose=str(compose), font_dir=directory, checkout=directory)
            for changed in ('sha', 'identity', 'fonts'):
                config = copy.deepcopy(rendered)
                if changed == 'sha': config['services']['app']['build']['args']['VERSION'] = 'c' * 40
                if changed == 'identity': config['services']['app']['user'] = '1000:1000'
                compose.write_text(json.dumps(config))
                with patch.object(module, 'font_files', return_value={} if changed == 'fonts' else document()['font_files']), \
                     patch.object(module, 'verify_source'), \
                     patch.object(module, 'run') as run, self.assertRaises(ValueError):
                    module.select(args)
                run.assert_not_called()
            compose.write_text(json.dumps(rendered))
            with patch.object(module, 'font_files', return_value=document()['font_files']), \
                 patch.object(module, 'verify_source'), \
                 patch.object(module, 'probe') as probe, patch.object(module, 'run') as run:
                module.select(args)
            self.assertEqual(probe.call_count, 2)
            self.assertEqual(run.call_count, 2)
            self.assertTrue(all(call.args[1] == 'tag' for call in run.call_args_list))

    def test_wrong_binary_or_browser_version_fails_probe(self):
        doc = document()
        info = {'Os': 'linux', 'Architecture': 'amd64', 'Config': {'User': '1003:1003',
                'Labels': {module.LABEL + '.sha': SHA, module.LABEL + '.role': 'free'}}}
        with patch.object(module, 'inspect', return_value=info), patch.object(module, 'run', return_value='wrong'):
            with self.assertRaises(ValueError): module.probe(doc, 'free')
        info['Config']['Labels'][module.LABEL + '.role'] = 'browser'
        with patch.object(module, 'inspect', return_value=info), patch.object(module, 'run', return_value='1.61.0'):
            with self.assertRaises(ValueError): module.probe(doc, 'browser')

    def test_registry_cleanup_only_deletes_owned_obsolete_versions(self):
        doc = document()
        kept = {role: {ref.split('@')[1]} for role, ref in doc['images'].items()}
        kept['catalog'] = {'sha256:' + '4' * 64}
        deleted = []
        def api(path, method='GET', **kwargs):
            if method == 'DELETE':
                deleted.append(path)
                return None
            role = path.split('/packages/container/')[1].split('/')[0].removeprefix(PREFIX + '-')
            return [{'id': 1, 'name': next(iter(kept[role]))},
                    {'id': 2, 'name': 'sha256:' + 'f' * 64}]
        with patch.object(module, 'retained_versions', return_value=kept), \
             patch.object(module, 'private_package', return_value={'visibility': 'private'}), \
             patch.object(module, 'api', side_effect=api):
            module.prune_registry([doc], [], REG, PREFIX)
        self.assertEqual(len(deleted), 4)
        for role in ('free', 'licensed', 'browser', 'catalog'):
            self.assertIn('orgs/example/packages/container/' + PREFIX + '-' + role + '/versions/2', deleted)

    def test_host_cleanup_protects_stopped_container_image(self):
        with tempfile.TemporaryDirectory() as directory:
            old = document('c' * 40)
            old['images'] = {role: ref.rsplit(':', 1)[0] + ':' + '8' * 64 for role, ref in old['images'].items()}
            pathlib.Path(directory, 'sets.json').write_text(json.dumps({'sets': [old]}))
            args = argparse.Namespace(state_dir=directory, registry=REG, prefix=PREFIX, require_licensed=True)
            removed = []
            def run(*args):
                if args[1:3] == ('ps', '--all'): return 'stopped-container'
                if args[1] == 'inspect': return json.dumps([{'Image': 'in-use'}])
                if args[1:3] == ('image', 'rm'): removed.append(args)
                return ''
            def inspect(ref):
                return {'Id': 'in-use' if ref == old['images']['free'] else 'not-in-use'}
            with patch.object(module, 'catalog', side_effect=[(document(), 'catalog'), None]), \
                 patch.object(module, 'run', side_effect=run), patch.object(module, 'probe'), \
                 patch.object(module, 'inspect', side_effect=inspect):
                module.prefetch(args)
            self.assertEqual(len(removed), 2)
            for call in removed:
                self.assertEqual(call[:3], ('docker', 'image', 'rm'))
                self.assertNotIn('--force', call)
                self.assertNotIn(old['images']['free'], call)
            state = json.loads(pathlib.Path(directory, 'sets.json').read_text())
            self.assertEqual(state['pending_eviction'], [old['images']['free']])
            # The later unchanged poll still retries a protected old image.
            with patch.object(module, 'catalog', side_effect=[(document(), 'catalog'), None]), \
                 patch.object(module, 'run', return_value=''), patch.object(module, 'inspect', return_value={'Id': 'unused'}):
                module.prefetch(args)
            state = json.loads(pathlib.Path(directory, 'sets.json').read_text())
            self.assertEqual(state['pending_eviction'], [])

    def test_source_checkout_mismatch_and_dirty_tree_fail_before_admission(self):
        with patch.object(module, 'run', return_value='c' * 40) as run, self.assertRaises(ValueError):
            module.verify_source('/checkout', SHA)
        self.assertEqual(run.call_count, 1)
        with patch.object(module, 'run', side_effect=[SHA, ' M compose.yaml']) as run, self.assertRaises(ValueError):
            module.verify_source('/checkout', SHA)
        self.assertEqual(run.call_count, 2)

    def test_eviction_race_keeps_retry_queue(self):
        refs = list(document()['images'].values())
        def run(*args):
            if args[1] == 'ps': return 'vanished'
            raise subprocess.CalledProcessError(1, args)
        with patch.object(module, 'run', side_effect=run):
            self.assertEqual(module.evict(refs, REG, PREFIX), refs)
        with self.assertRaises(ValueError):
            module.evict(['other.example/user-image@sha256:' + 'f' * 64], REG, PREFIX)

    def test_first_licensed_set_can_replace_free_only_previous(self):
        previous = document('c' * 40)
        del previous['images']['licensed']
        previous['font_files'] = {}
        with tempfile.TemporaryDirectory() as directory:
            args = argparse.Namespace(state_dir=directory, registry=REG, prefix=PREFIX, require_licensed=True)
            with patch.object(module, 'catalog', side_effect=[(document(), 'catalog'), (previous, 'catalog-old')]), \
                 patch.object(module, 'run', return_value=''), patch.object(module, 'probe'), \
                 patch.object(module, 'evict', return_value=[]):
                module.prefetch(args)
            state = json.loads(pathlib.Path(directory, 'sets.json').read_text())
            self.assertEqual(state['sets'], [document()])

    def test_font_fingerprints_are_from_bytes_not_just_labels(self):
        fingerprints = document()['font_files']
        info = {'Config': {'Labels': {module.FONT_LABEL: json.dumps(fingerprints)}}}
        with patch.object(module, 'inspect', return_value=info), \
             patch.object(module, 'run', return_value='scratch-container') as run, \
             patch.object(module, 'font_files', return_value={'Euclid.woff2': 'f' * 64}), self.assertRaises(ValueError):
            module.font_context_files('pinned-input')
        self.assertEqual(run.call_args_list[-1].args, ('docker', 'rm', 'scratch-container'))
        self.assertFalse(any('start' in call.args for call in run.call_args_list))
        with patch.object(module, 'inspect', return_value=info), \
             patch.object(module, 'run', return_value='scratch-container'), \
             patch.object(module, 'font_files', return_value=fingerprints):
            self.assertEqual(module.font_context_files('pinned-input'), fingerprints)


if __name__ == '__main__':
    unittest.main()
