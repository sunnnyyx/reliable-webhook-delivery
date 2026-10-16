#!/usr/bin/env python3
"""Test actual admin/API binaries; keep credential values out of output.

Creates synthetic Smoke tenants in DATABASE_URL and disables them on completion.
"""
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

from smoke_db import isolated_database

ROOT = Path(__file__).resolve().parents[1]


def admin(*args):
    result = subprocess.run([str(ROOT / 'bin/admin'), *args],
                            capture_output=True, text=True, timeout=15)
    if result.returncode:
        raise RuntimeError('admin command failed (credential output withheld)')
    return json.loads(result.stdout)


def request(address, method, path, token, body=None):
    data = None if body is None else json.dumps(body).encode()
    headers = {'Authorization': f'Bearer {token}', 'Content-Type': 'application/json'}
    request = urllib.request.Request('http://' + address + path,
                                     data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.load(error)


def main():
    accounts = []
    try:
        first = admin('tenant-create', '--name', 'Smoke tenant A')
        accounts.append(first)
        second = admin('tenant-create', '--name', 'Smoke tenant B')
        accounts.append(second)
        replacement = admin('key-create', '--tenant', first['tenant_id'])
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            address = f'127.0.0.1:{listener.getsockname()[1]}'
        env = os.environ.copy()
        env['API_ADDR'] = address
        with tempfile.TemporaryFile() as logs:
            process = subprocess.Popen([str(ROOT / 'bin/api')], env=env,
                                       stdout=logs, stderr=logs)
            secrets = [first['api_key'], second['api_key'], replacement['api_key']]
            try:
                deadline = time.monotonic() + 10
                while True:
                    if process.poll() is not None:
                        raise RuntimeError('API exited before readiness')
                    try:
                        code, _ = request(address, 'GET', '/health/ready', first['api_key'])
                        if code == 200:
                            break
                    except (OSError, urllib.error.URLError):
                        pass
                    if time.monotonic() > deadline:
                        raise RuntimeError('readiness deadline exceeded')
                    time.sleep(0.05)
                status, body = request(address, 'POST', '/v1/endpoints', first['api_key'],
                                       {'url': 'https://hooks.example.com/orders',
                                        'subscribed_event_types': ['order.created']})
                assert status == 201
                endpoint = body['endpoint']['id']
                secrets.append(body['signing_secret'])
                path = '/v1/endpoints/' + endpoint
                assert request(address, 'GET', path, second['api_key'])[0] == 404
                code, metadata = request(address, 'GET', path, first['api_key'])
                assert code == 200 and 'signing_secret' not in metadata
                status, rotation = request(address, 'POST', path + '/rotate-secret',
                                           first['api_key'], {})
                assert status == 201 and rotation['signing_secret'] != secrets[-1]
                secrets.append(rotation['signing_secret'])
                admin('key-revoke', '--tenant', first['tenant_id'], '--key', first['key_id'])
                assert request(address, 'GET', path, first['api_key'])[0] == 401
                assert request(address, 'GET', path, replacement['api_key'])[0] == 200
                admin('tenant-disable', '--tenant', first['tenant_id'])
                assert request(address, 'GET', path, replacement['api_key'])[0] == 401
                print('PASS: admin provisioning, key creation/revocation, tenant disabling, API isolation and rotation')
            finally:
                if process.poll() is None:
                    process.send_signal(signal.SIGTERM)
                try:
                    result = process.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
                    raise RuntimeError('API shutdown timed out')
                assert result == 0
                logs.seek(0)
                output = logs.read().decode()
                assert all(secret not in output for secret in secrets)
                print('PASS: credential values absent from API logs')
    finally:
        for account in accounts:
            admin('tenant-disable', '--tenant', account['tenant_id'])


if __name__ == '__main__':
    if not os.environ.get('DATABASE_URL') or not os.environ.get('ENCRYPTION_KEY'):
        raise SystemExit('DATABASE_URL and ENCRYPTION_KEY required; run make test-accounts-smoke')
    with isolated_database():
        main()
