#!/usr/bin/env python3
"""Certify a disposable standalone relay without host mounts or published ports."""
import argparse
import json
import subprocess
import signal
import sys
import uuid
import time

PROBE = r'''
import certifi, importlib.util, json, pathlib, shutil, ssl, sys, urllib.error, urllib.request
assert sys.version_info >= (3, 13)
assert importlib.util.find_spec('chromadb') is None
for command in ('node', 'npm', 'git'):
    assert shutil.which(command) is None, command
bundle = pathlib.Path(certifi.where())
assert bundle.resolve() == pathlib.Path('/etc/ssl/certs/ca-certificates.crt')
for context in (ssl.create_default_context(), ssl.create_default_context(cafile=str(bundle))):
    roots = context.get_ca_certs()
    assert roots
    assert not any(v == 'emSign ECC Root CA - G3' for c in roots for r in c['subject'] for k, v in r)
assert json.loads(pathlib.Path('/data/config.json').read_text())['backend'] == 'sqlite_exact'
def call(method, params, token='relay-acceptance-synthetic'):
    headers = {'Content-Type': 'application/json'}
    if token is not None:
        headers['Authorization'] = 'Bearer ' + token
    payload = json.dumps({'jsonrpc': '2.0', 'id': 1, 'method': method, 'params': params}).encode()
    req = urllib.request.Request('http://127.0.0.1:8766/mcp', data=payload, headers=headers)
    with urllib.request.urlopen(req, timeout=10) as response:
        assert response.status == 200
        result = json.load(response)
    assert 'error' not in result, result
    return result['result']
params = {'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'relay-acceptance','version':'1'}}
for token in (None, 'wrong-token'):
    try:
        call('initialize', params, token)
    except urllib.error.HTTPError as error:
        assert error.code == 401, error.code
    else:
        raise AssertionError('unauthenticated relay access succeeded')
assert call('initialize', params)['serverInfo']['version'] == '3.11.0'
def tool(name, arguments):
    result = call('tools/call', {'name':name, 'arguments':arguments})
    assert not result.get('isError'), result
    return json.loads(next(c['text'] for c in result['content'] if c['type'] == 'text'))
event = tool('mempalace_event_append', {'type':'acceptance.synthetic','stream':'acceptance','room':'synthetic','from_agent':'relay-acceptance','body':'synthetic relay round trip'})
assert event['success']
events = tool('mempalace_event_list', {'stream':'acceptance','limit':10})
assert any(e['id'] == event['event']['id'] and e['body'] == 'synthetic relay round trip' for e in events['events'])
print('Standalone relay: SQLite, TLS distrust, package absence, bearer authentication and synthetic event round trip passed')
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('image')
    parser.add_argument('--container-name', default='deck-relay-acceptance-' + uuid.uuid4().hex)
    args = parser.parse_args()
    image = subprocess.check_output(['docker', 'image', 'inspect', args.image, '--format', '{{.Id}}'], text=True).strip()
    container = subprocess.check_output([
        'docker', 'run', '--detach', '--name', args.container_name,
        '--label', 'com.noisefloor.acceptance=standalone-relay', '--network', 'none', '--read-only', '--cap-drop', 'ALL',
        '--security-opt', 'no-new-privileges', '--tmpfs', '/tmp:rw,nosuid,nodev,size=128m,mode=1777',
        '--tmpfs', '/home/relay:rw,nosuid,nodev,size=256m,uid=1003,gid=1003,mode=0700',
        '--tmpfs', '/data:rw,nosuid,nodev,size=256m,uid=1003,gid=1003,mode=0700',
        '--env', 'MEMPALACE_MCP_HTTP_TOKEN=relay-acceptance-synthetic',
        '--env', 'MEMPALACE_SYNC_INTERVAL=0', image], text=True).strip()
    def interrupted(signum, frame):
        raise SystemExit(128 + signum)

    previous_handler = signal.signal(signal.SIGTERM, interrupted)
    passed = False
    try:
        health = "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8766/healthz', timeout=2).close()"
        for _ in range(45):
            result = subprocess.run(['docker', 'exec', container, '/opt/mempalace/venv/bin/python', '-c', health], capture_output=True)
            if result.returncode == 0:
                break
            running = subprocess.run(['docker', 'inspect', '--format', '{{.State.Running}}', container], capture_output=True, text=True)
            if running.returncode or running.stdout.strip() != 'true':
                raise RuntimeError('Disposable relay exited during startup')
            time.sleep(1)
        else:
            raise RuntimeError('Disposable relay did not become ready')
        subprocess.run(['docker', 'exec', '-i', container, '/opt/mempalace/venv/bin/python', '-'], input=PROBE, text=True, check=True)
        passed = True
    except BaseException:
        subprocess.run(['docker', 'logs', '--tail', '100', container], check=False)
        raise
    finally:
        signal.signal(signal.SIGTERM, previous_handler)
        cleanup = subprocess.run(['docker', 'rm', '--force', container], capture_output=True, text=True)
        if cleanup.returncode:
            print('Disposable relay cleanup failed: ' + cleanup.stderr, file=sys.stderr)
            if passed:
                raise RuntimeError('Disposable relay was not removed')


if __name__ == '__main__':
    main()
