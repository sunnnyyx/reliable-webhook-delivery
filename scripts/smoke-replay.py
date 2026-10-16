#!/usr/bin/env python3
"""Exercise replay on compiled API/worker/receiver processes in disposable schemas."""
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import runpy
import signal
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from smoke_db import isolated_database, receiver_counts

ROOT = Path(__file__).resolve().parents[1]
helpers = runpy.run_path(str(ROOT / 'scripts/smoke-worker.py'))
free_port, admin = helpers['free_port'], helpers['admin']


def main():
    account = admin('tenant-create', '--name', 'Replay smoke tenant')
    receiver_address = f'127.0.0.1:{free_port()}'
    api_address = f'127.0.0.1:{free_port()}'
    children, logs = [], []
    received, packets = [], []
    marker = 'replay-payload-must-not-appear-in-logs'

    class Proxy(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers['Content-Length']))
            received.append(body)
            if len(received) == 1:
                self.send_response(400)
                self.end_headers()
                return
            headers = {name: self.headers[name] for name in
                       ['webhook-id', 'webhook-timestamp', 'webhook-signature', 'Content-Type']}
            packets.append((body, headers))
            req = urllib.request.Request('http://' + receiver_address + '/webhooks',
                                         data=body, headers=headers, method='POST')
            try:
                with urllib.request.urlopen(req, timeout=3) as response:
                    code = response.status
            except urllib.error.HTTPError as error:
                code = error.code
            self.send_response(code)
            self.end_headers()

    proxy = ThreadingHTTPServer(('127.0.0.1', 0), Proxy)
    thread = threading.Thread(target=proxy.serve_forever, daemon=True)
    thread.start()
    destination = f'http://localhost:{proxy.server_port}/webhooks'
    env = dict(os.environ, API_ADDR=api_address, DEV_RECEIVER_URL=destination,
               WORKER_CONCURRENCY='1', QUEUE_POLL_INTERVAL='20ms')

    def call(method, path, data=None, key=None):
        headers = {'Authorization': 'Bearer ' + account['api_key'], 'Content-Type': 'application/json'}
        if key:
            headers['Idempotency-Key'] = key
        req = urllib.request.Request('http://' + api_address + path,
                                     data=None if data is None else json.dumps(data).encode(),
                                     headers=headers, method=method)
        try:
            response = urllib.request.urlopen(req, timeout=6)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            assert response.headers['X-Request-ID']
            return response.status, response.read()

    def wait_ready(address):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                with urllib.request.urlopen('http://' + address + '/health/ready', timeout=1) as response:
                    if response.status == 200:
                        return
            except OSError:
                pass
            time.sleep(.05)
        raise RuntimeError('local process readiness timed out')

    def wait_delivery(status):
        deadline = time.monotonic() + 12
        while time.monotonic() < deadline:
            code, raw = call('GET', '/v1/deliveries')
            assert code == 200
            rows = json.loads(raw)['data']
            if len(rows) == 1 and rows[0]['status'] == status:
                return rows[0]
            time.sleep(.05)
        raise RuntimeError('delivery state transition timed out')

    def stop(child):
        if child.poll() is None:
            child.send_signal(signal.SIGTERM)
        assert child.wait(timeout=20) == 0

    try:
        with ExitStack() as stack:
            def launch(role, child_env):
                log = stack.enter_context(tempfile.TemporaryFile())
                child = subprocess.Popen([str(ROOT / 'bin' / role)], env=child_env,
                                         stdout=log, stderr=log)
                children.append(child)
                logs.append(log)
                return child

            launch('api', env)
            wait_ready(api_address)
            code, raw = call('POST', '/v1/endpoints',
                             {'url': destination, 'subscribed_event_types': ['order.created']})
            assert code == 201
            created = json.loads(raw)
            receiver_env = dict(env, RECEIVER_ADDR=receiver_address,
                                RECEIVER_SECRETS=created['signing_secret'])
            launch('receiver', receiver_env)
            wait_ready(receiver_address)
            code, raw = call('POST', '/v1/events',
                             {'type': 'order.created', 'data': {'marker': marker}}, 'replay-event')
            assert code == 202 and json.loads(raw)['delivery_count'] == 1
            worker_env = dict(env, WORKER_ADDR=f'127.0.0.1:{free_port()}')
            worker = launch('worker', worker_env)
            before = wait_delivery('dead_letter')
            stop(worker)
            path = '/v1/deliveries/' + before['id'] + '/replay'
            with ThreadPoolExecutor(max_workers=12) as pool:
                results = list(pool.map(lambda _: call('POST', path, {}, 'one-replay'), range(20)))
            assert all(code == 202 and body == results[0][1] for code, body in results)
            assert call('POST', path, {}, 'another-key')[0] == 409
            worker = launch('worker', worker_env)
            after = wait_delivery('delivered')
            assert before['id'] == after['id'] and before['url_snapshot'] == after['url_snapshot']
            assert after['cycle_number'] == 2 and after['cycle_attempt_count'] == 1
            assert after['lifetime_attempt_count'] == 2
            assert len(received) == 2 and received[0] == received[1]
            assert call('POST', path, {}, 'one-replay') == results[0]
            code, raw = call('GET', '/v1/deliveries/' + before['id'] + '/attempts')
            history = json.loads(raw)['data']
            assert code == 200 and [row['cycle_number'] for row in history] == [1, 2]
            assert [row['outcome'] for row in history] == ['permanent_failure', 'success']
            code, raw = call('GET', '/v1/deliveries/' + before['id'] + '/replays')
            assert code == 200 and len(json.loads(raw)['data']) == 1
            assert receiver_counts() == (1, 1)
            for body, headers in packets:
                req = urllib.request.Request('http://' + receiver_address + '/webhooks',
                                             data=body, headers=headers, method='POST')
                with urllib.request.urlopen(req, timeout=3) as response:
                    assert response.status == 204
            assert receiver_counts() == (1, 1)
            for child in reversed(children):
                stop(child)
            children.clear()
            for log in logs:
                log.seek(0)
                output = log.read().decode()
                assert all(value not in output for value in
                           [marker, account['api_key'], created['signing_secret'], env['ENCRYPTION_KEY']])
            print('PASS: compiled replay API accepted 20 same-key requests as one cycle; old attempts/bytes preserved')
            print('PASS: replay delivered once, duplicate acknowledged with one durable receiver business record; clean logs/shutdown')
    finally:
        for child in reversed(children):
            if child.poll() is None:
                child.kill()
                child.wait(timeout=5)
        proxy.shutdown()
        proxy.server_close()
        thread.join(timeout=5)
        admin('tenant-disable', '--tenant', account['tenant_id'])


if __name__ == '__main__':
    with isolated_database():
        main()
