#!/usr/bin/env python3
"""Exercise durable ingestion with actual binaries and a real development DB.

Creates synthetic tenant/event history, then disables its tenant. Sends no webhooks.
"""
import json
import os
from pathlib import Path
import runpy
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

from smoke_db import isolated_database

ROOT = Path(__file__).resolve().parents[1]
admin = runpy.run_path(str(ROOT / 'scripts/smoke-accounts.py'))['admin']


def request(address, token, method, path, data=None, key=None):
    headers = {'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'}
    if key is not None:
        headers['Idempotency-Key'] = key
    req = urllib.request.Request('http://' + address + path, data=data,
                                 headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=5) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def stop(process):
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
    try:
        if process.wait(timeout=20):
            raise RuntimeError('API exited unsuccessfully')
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)
        raise RuntimeError('API shutdown deadline exceeded')


def main():
    account = admin('tenant-create', '--name', 'Smoke ingestion tenant')
    secrets = [account['api_key'], os.environ['ENCRYPTION_KEY']]
    marker = 'synthetic-payload-must-stay-out-of-logs'
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        address = f'127.0.0.1:{listener.getsockname()[1]}'
    env = os.environ.copy()
    env['API_ADDR'] = address
    process = None
    try:
        with tempfile.TemporaryFile() as logs:
            def launch():
                child = subprocess.Popen([str(ROOT / 'bin/api')], env=env,
                                         stdout=logs, stderr=logs)
                try:
                    deadline = time.monotonic() + 10
                    while time.monotonic() < deadline:
                        if child.poll() is not None:
                            raise RuntimeError('API exited before readiness')
                        try:
                            if request(address, account['api_key'], 'GET', '/health/ready')[0] == 200:
                                return child
                        except (OSError, urllib.error.URLError):
                            pass
                        time.sleep(0.05)
                    raise RuntimeError('API readiness timed out')
                except BaseException:
                    if child.poll() is None:
                        child.kill()
                    child.wait(timeout=5)
                    raise

            process = launch()
            for subscription in ['order.created', '*']:
                body = json.dumps({'url': 'https://hooks.example.com/demo',
                                   'subscribed_event_types': [subscription]}).encode()
                status, raw = request(address, account['api_key'], 'POST', '/v1/endpoints', body)
                assert status == 201
                secrets.append(json.loads(raw)['signing_secret'])
            event = json.dumps({'type': 'order.created', 'data': {'marker': marker}}).encode()
            status, original = request(address, account['api_key'], 'POST', '/v1/events', event, 'smoke-order')
            assert status == 202
            accepted = json.loads(original)
            assert accepted['delivery_count'] == 2
            assert request(address, account['api_key'], 'POST', '/v1/events', b' ' + event, 'smoke-order')[0] == 409
            stop(process)
            process = None
            process = launch()
            status, repeated = request(address, account['api_key'], 'POST', '/v1/events', event, 'smoke-order')
            assert status == 202 and repeated == original
            status, body = request(address, account['api_key'], 'GET', '/v1/events/' + accepted['event_id'])
            detail = json.loads(body)
            assert status == 200 and detail['delivery_summary'] == {'pending': 2}
            assert detail['payload']['data']['marker'] == marker
            status, body = request(address, account['api_key'], 'GET', '/v1/deliveries?event_id=' + accepted['event_id'])
            deliveries = json.loads(body)['data']
            assert status == 200 and len(deliveries) == 2
            endpoint = deliveries[0]['endpoint_id']
            assert request(address, account['api_key'], 'DELETE', '/v1/endpoints/' + endpoint)[0] == 204
            status, body = request(address, account['api_key'], 'GET', '/v1/deliveries/' + deliveries[0]['id'])
            assert status == 200 and json.loads(body)['status'] == 'cancelled'
            stop(process)
            process = None
            logs.seek(0)
            output = logs.read().decode()
            assert all(secret not in output for secret in secrets) and marker not in output
            print('PASS: durable fan-out, exact idempotent response after API restart, mismatch, tenant views, deletion cancellation')
            print('PASS: credentials, encryption key, signing secrets, and payload marker absent from API logs')
    finally:
        if process is not None:
            stop(process)
        admin('tenant-disable', '--tenant', account['tenant_id'])


if __name__ == '__main__':
    if not os.environ.get('DATABASE_URL') or not os.environ.get('ENCRYPTION_KEY'):
        raise SystemExit('DATABASE_URL and ENCRYPTION_KEY required; run make test-ingestion-smoke')
    with isolated_database():
        main()
