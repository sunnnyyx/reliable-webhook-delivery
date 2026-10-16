#!/usr/bin/env python3
"""Run two compiled workers and the compiled receiver through a gated local proxy.

The first two proxy requests wait for each other. With one slot per worker,
this proves both processes participated rather than relying on scheduling luck.
Creates synthetic development history and disables its tenant on completion.
"""
from contextlib import ExitStack
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import runpy
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

from smoke_db import isolated_database, receiver_counts

ROOT = Path(__file__).resolve().parents[1]
admin = runpy.run_path(str(ROOT / 'scripts/smoke-accounts.py'))['admin']


def free_port():
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        return listener.getsockname()[1]


def main():
    account = admin('tenant-create', '--name', 'Smoke two-worker tenant')
    children = []
    processes = []
    secrets = [account['api_key'], os.environ['ENCRYPTION_KEY']]
    marker = 'two-worker-payload-must-not-be-logged'
    receiver_port, api_port = free_port(), free_port()
    receiver_address = f'127.0.0.1:{receiver_port}'
    api_address = f'127.0.0.1:{api_port}'
    expected = {}
    seen = {}
    packets = {}
    failures = []
    lock = threading.Lock()
    gate = threading.Barrier(2)

    class Proxy(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            raw = self.rfile.read(int(self.headers.get('Content-Length', '0')))
            delivery_id = self.headers.get('webhook-id', '')
            try:
                envelope = json.loads(raw)
                assert self.path == '/webhooks'
                assert hashlib.sha256(raw).hexdigest() == expected[envelope['id']]
                with lock:
                    ordinal = sum(seen.values()) + 1
                    seen[delivery_id] = seen.get(delivery_id, 0) + 1
                if ordinal <= 2:
                    gate.wait(timeout=4)
                headers = {name: self.headers[name] for name in
                           ['webhook-id', 'webhook-timestamp', 'webhook-signature', 'Content-Type']}
                with lock:
                    packets[delivery_id] = (raw, headers)
                req = urllib.request.Request('http://' + receiver_address + '/webhooks',
                                             data=raw, headers=headers, method='POST')
                with urllib.request.urlopen(req, timeout=3) as response:
                    assert response.status == 204
                self.send_response(204)
                self.end_headers()
            except Exception:
                with lock:
                    failures.append('local proxy or receiver assertion failed')
                self.send_response(500)
                self.end_headers()

    proxy = ThreadingHTTPServer(('127.0.0.1', 0), Proxy)
    proxy_thread = threading.Thread(target=proxy.serve_forever, daemon=True)
    proxy_thread.start()
    dev_url = f'http://localhost:{proxy.server_port}/webhooks'
    env = os.environ.copy()
    env.update(API_ADDR=api_address, DEV_RECEIVER_URL=dev_url,
               WORKER_CONCURRENCY='1', QUEUE_POLL_INTERVAL='20ms',
               OUTBOUND_TIMEOUT='5s', DELIVERY_LEASE='30s')

    def call(method, path, data=None, key=None):
        headers = {'Authorization': 'Bearer ' + account['api_key'],
                   'Content-Type': 'application/json'}
        if key:
            headers['Idempotency-Key'] = key
        raw = None if data is None else json.dumps(data).encode()
        req = urllib.request.Request('http://' + api_address + path,
                                     data=raw, headers=headers, method=method)
        with urllib.request.urlopen(req, timeout=5) as response:
            return response.status, json.load(response)

    def wait_url(address, path):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                with urllib.request.urlopen('http://' + address + path, timeout=1) as response:
                    if response.status == 200:
                        return
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.05)
        raise RuntimeError('local process readiness timed out')

    def stop(child):
        if child.poll() is None:
            child.send_signal(signal.SIGTERM)
        try:
            result = child.wait(timeout=20)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=5)
            raise RuntimeError('process shutdown timed out')
        if result:
            raise RuntimeError('local process exited unsuccessfully')

    try:
        with ExitStack() as stack:
            def launch(role, child_env):
                log = stack.enter_context(tempfile.TemporaryFile())
                child = subprocess.Popen([str(ROOT / 'bin' / role)], env=child_env,
                                         stdout=log, stderr=log)
                children.append(child)
                processes.append((role, child, log))
                return child

            launch('api', env)
            wait_url(api_address, '/health/ready')
            code, created = call('POST', '/v1/endpoints',
                                 {'url': dev_url, 'subscribed_event_types': ['order.created']})
            assert code == 201
            secrets.append(created['signing_secret'])
            receiver_env = env.copy()
            receiver_env.update(RECEIVER_ADDR=receiver_address,
                                RECEIVER_SECRETS=created['signing_secret'])
            receiver_child = launch('receiver', receiver_env)
            wait_url(receiver_address, '/health/live')
            for i in range(12):
                code, accepted = call('POST', '/v1/events',
                                      {'type': 'order.created', 'data': {'marker': marker, 'n': i}},
                                      f'two-workers-{i}')
                assert code == 202 and accepted['delivery_count'] == 1
                _, detail = call('GET', '/v1/events/' + accepted['event_id'])
                expected[accepted['event_id']] = detail['event']['body_sha256']
            for _ in range(2):
                child_env = env.copy()
                child_env['WORKER_ADDR'] = f'127.0.0.1:{free_port()}'
                launch('worker', child_env)
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                _, page = call('GET', '/v1/deliveries')
                rows = page['data']
                if len(rows) == 12 and all(row['status'] == 'delivered' for row in rows):
                    break
                if any(child.poll() is not None for child in children):
                    raise RuntimeError('process exited during worker demonstration')
                time.sleep(.05)
            else:
                raise RuntimeError('deliveries did not all acknowledge within test deadline')
            worker_ids = set()
            for row in rows:
                assert row['cycle_attempt_count'] == row['lifetime_attempt_count'] == 1
                _, history = call('GET', '/v1/deliveries/' + row['id'] + '/attempts')
                assert len(history['data']) == 1
                attempt = history['data'][0]
                assert attempt['outcome'] == 'success' and attempt['http_status'] == 204
                worker_ids.add(attempt['worker_id'])
            with lock:
                assert not failures and len(seen) == 12 and all(count == 1 for count in seen.values())
            assert len(worker_ids) == 2
            assert receiver_counts() == (12, 12)
            stop(receiver_child)
            receiver_child = launch('receiver', receiver_env)
            wait_url(receiver_address, '/health/live')
            for raw, headers in packets.values():
                req = urllib.request.Request('http://' + receiver_address + '/webhooks',
                                             data=raw, headers=headers, method='POST')
                with urllib.request.urlopen(req, timeout=3) as response:
                    assert response.status == 204
            assert receiver_counts() == (12, 12)
            for child in reversed(children):
                stop(child)
            children.clear()
            for _, _, log in processes:
                log.seek(0)
                output = log.read().decode()
                assert marker not in output and all(secret not in output for secret in secrets)
            print('PASS: two actual worker processes delivered 12 signed immutable payloads to the compiled receiver')
            print('PASS: compiled receiver restart and 12 duplicate requests preserved exactly 12 business records')
            print('PASS: one successful attempt each, both worker IDs persisted, bounded shutdown, no payload/secrets in logs')
    finally:
        for child in reversed(children):
            if child.poll() is None:
                child.kill()
                child.wait(timeout=5)
        proxy.shutdown()
        proxy.server_close()
        proxy_thread.join(timeout=5)
        admin('tenant-disable', '--tenant', account['tenant_id'])


if __name__ == '__main__':
    if not os.environ.get('DATABASE_URL') or not os.environ.get('ENCRYPTION_KEY'):
        raise SystemExit('DATABASE_URL and ENCRYPTION_KEY required; run make test-worker-smoke')
    with isolated_database():
        main()
