#!/usr/bin/env python3
"""Real console/API/worker workflow in disposable sender and receiver schemas."""
from contextlib import ExitStack
from http.cookiejar import CookieJar
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
from smoke_db import isolated_database

ROOT = Path(__file__).resolve().parents[1]
helpers = runpy.run_path(str(ROOT / 'scripts/smoke-worker.py'))
admin, free_port = helpers['admin'], helpers['free_port']


def main():
    account = admin('tenant-create', '--name', 'Console smoke tenant')
    other = admin('tenant-create', '--name', 'Console isolation tenant')
    children, logs, received = [], [], []
    secrets = [account['api_key'], other['api_key'], os.environ['ENCRYPTION_KEY']]

    class Receiver(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            received.append(self.rfile.read(int(self.headers['Content-Length'])))
            self.send_response(400 if len(received) == 1 else 204)
            self.end_headers()

    receiver = ThreadingHTTPServer(('127.0.0.1', 0), Receiver)
    thread = threading.Thread(target=receiver.serve_forever, daemon=True)
    thread.start()
    api = f'127.0.0.1:{free_port()}'
    address = f'127.0.0.1:{free_port()}'
    origin = 'http://' + address
    env = dict(os.environ, API_ADDR=api, CONSOLE_ADDR=address,
               CONSOLE_API_URL='http://' + api,
               DEV_RECEIVER_URL=f'http://localhost:{receiver.server_port}/webhooks',
               WORKER_ADDR=f'127.0.0.1:{free_port()}', RATE_LIMIT_ENABLED='false',
               WORKER_CONCURRENCY='1', QUEUE_POLL_INTERVAL='20ms')
    jar = CookieJar()
    client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    csrf = ''

    def call(method, path, data=None, key=None, token=None):
        headers = {'Content-Type': 'application/json', 'Origin': origin}
        if token is not None:
            headers['X-CSRF-Token'] = token
        elif csrf:
            headers['X-CSRF-Token'] = csrf
        if key:
            headers['Idempotency-Key'] = key
        req = urllib.request.Request(origin + path, method=method, headers=headers,
                                     data=None if data is None else json.dumps(data).encode())
        try:
            response = client.open(req, timeout=8)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            assert response.headers['Cache-Control'] == 'no-store'
            return response.status, raw, response.headers

    def wait_status(status):
        deadline = time.monotonic() + 12
        while time.monotonic() < deadline:
            code, raw, _ = call('GET', '/api/v1/deliveries?status=' + status)
            assert code == 200
            rows = json.loads(raw)['data']
            if len(rows) == 1:
                return rows[0]
            time.sleep(.05)
        raise RuntimeError('console delivery transition timed out')

    try:
        with ExitStack() as stack:
            def launch(role):
                log = stack.enter_context(tempfile.TemporaryFile())
                process = subprocess.Popen([str(ROOT / 'bin' / role)], env=env,
                                           stdout=log, stderr=log)
                children.append(process)
                logs.append(log)
                return process

            launch('api')
            launch('console')
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                try:
                    if call('GET', '/')[0] == 200:
                        break
                except OSError:
                    pass
                time.sleep(.05)
            assert call('GET', '/api/v1/events')[0] == 401
            for _ in range(30):
                code, raw, _ = call('POST', '/session', {'key': account['api_key']})
                if code == 200:
                    break
                assert code in (502, 503)
                time.sleep(.1)
            assert code == 200
            csrf = json.loads(raw)['csrf']
            assert account['api_key'].encode() not in raw
            assert call('POST', '/api/v1/events', {}, token='wrong')[0] == 403
            code, raw, _ = call('POST', '/api/v1/endpoints', {
                'url': env['DEV_RECEIVER_URL'], 'subscribed_event_types': ['order.created']})
            assert code == 201
            created = json.loads(raw)
            secrets.append(created['signing_secret'])
            endpoint_id = created['endpoint']['id']
            code, raw, _ = call('POST', '/api/v1/events', {
                'type': 'order.created', 'data': {'console_smoke': True}}, 'console-publish')
            assert code == 202
            event_id = json.loads(raw)['event_id']
            assert json.loads(raw)['delivery_count'] == 1
            assert call('POST', '/api/v1/events', {
                'type': 'order.created', 'data': {'console_smoke': True}}, 'console-publish')[1] == raw
            assert call('GET', '/api/v1/events/' + event_id)[0] == 200
            launch('worker')
            delivery = wait_status('dead_letter')
            path = '/api/v1/deliveries/' + delivery['id']
            code, raw, _ = call('GET', path + '/attempts')
            assert code == 200 and json.loads(raw)['data'][0]['http_status'] == 400
            code, receipt, _ = call('POST', path + '/replay', {}, 'console-replay')
            assert code == 202 and json.loads(receipt)['new_cycle_number'] == 2
            delivered = wait_status('delivered')
            assert delivered['id'] == delivery['id']
            assert call('POST', path + '/replay', {}, 'console-replay')[1] == receipt
            assert call('POST', path + '/replay', {}, 'another-replay')[0] == 409
            assert len(received) == 2 and received[0] == received[1]
            assert len(json.loads(call('GET', path + '/attempts')[1])['data']) == 2
            assert len(json.loads(call('GET', path + '/replays')[1])['data']) == 1
            # Cursor pagination is forwarded unchanged; no UI cap masquerades as a total.
            assert call('POST', '/api/v1/events', {'type': 'other.event', 'data': {}}, 'second-event')[0] == 202
            code, raw, _ = call('GET', '/api/v1/events?limit=1')
            cursor = json.loads(raw)['next_cursor']
            assert code == 200 and cursor
            assert call('GET', '/api/v1/events?limit=1&cursor=' + urllib.parse.quote(cursor))[0] == 200
            assert call('PATCH', '/api/v1/endpoints/' + endpoint_id, {'enabled': False})[0] == 200
            assert call('PATCH', '/api/v1/endpoints/' + endpoint_id, {'enabled': True})[0] == 200
            code, raw, _ = call('POST', '/api/v1/endpoints/' + endpoint_id + '/rotate-secret', {})
            assert code == 201
            secrets.append(json.loads(raw)['signing_secret'])
            # Another tenant gets a separate server-side session and cannot inspect these IDs.
            csrf = json.loads(call('POST', '/session', {'key': other['api_key']})[1])['csrf']
            assert call('GET', path)[0] == 404
            assert call('GET', '/api/v1/endpoints/' + endpoint_id)[0] == 404
            csrf = json.loads(call('POST', '/session', {'key': account['api_key']})[1])['csrf']
            assert call('DELETE', '/api/v1/endpoints/' + endpoint_id)[0] == 204
            admin('key-revoke', '--tenant', account['tenant_id'], '--key', account['key_id'])
            assert call('GET', '/api/v1/events')[0] == 401
            assert call('GET', '/session')[0] == 401
            csrf = json.loads(call('POST', '/session', {'key': other['api_key']})[1])['csrf']
            assert call('DELETE', '/session')[0] == 204
            assert call('GET', '/api/v1/events')[0] == 401
            # Console restart also invalidates all in-memory sessions.
            csrf = json.loads(call('POST', '/session', {'key': other['api_key']})[1])['csrf']
            children[1].send_signal(signal.SIGTERM)
            assert children[1].wait(timeout=20) == 0
            launch('console')
            for _ in range(50):
                try:
                    if call('GET', '/session')[0] == 401:
                        break
                except OSError:
                    pass
                time.sleep(.05)
            assert call('GET', '/session')[0] == 401
            for child in children:
                if child.poll() is None:
                    child.send_signal(signal.SIGTERM)
            for child in children:
                assert child.wait(timeout=20) == 0
            for log in logs:
                log.seek(0)
                content = log.read()
                assert all(secret.encode() not in content for secret in secrets)
            print('PASS: console sessions/CSRF, real API endpoint/event workflow, exact-byte replay/history, pagination, tenant isolation, revocation, logout and restart')
    finally:
        for child in children:
            if child.poll() is None:
                child.kill()
                child.wait(timeout=5)
        receiver.shutdown()
        receiver.server_close()
        thread.join(timeout=3)


if __name__ == '__main__':
    with isolated_database():
        main()
