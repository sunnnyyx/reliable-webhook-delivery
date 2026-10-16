#!/usr/bin/env python3
"""Two compiled workers, one real Redis bucket, controlled outage and recovery."""
from contextlib import ExitStack
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import runpy
import select
import signal
import socket
import socketserver
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid
from smoke_db import isolated_database, receiver_counts

ROOT = Path(__file__).resolve().parents[1]
helpers = runpy.run_path(str(ROOT / 'scripts/smoke-worker.py'))
free_port, admin = helpers['free_port'], helpers['admin']


class RedisProxy(socketserver.ThreadingTCPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, upstream):
        self.lock = threading.Lock()
        self.online = False
        self.active = set()
        self.upstream = upstream
        super().__init__(('127.0.0.1', 0), RedisForwarder)

    def set_online(self, online):
        with self.lock:
            self.online = online
            if not online:
                for connection in list(self.active):
                    try:
                        connection.shutdown(socket.SHUT_RDWR)
                    except OSError:
                        pass
                    connection.close()


class RedisForwarder(socketserver.BaseRequestHandler):
    def handle(self):
        proxy = self.server
        front, back = self.request, None
        try:
            with proxy.lock:
                if not proxy.online:
                    return
            back = socket.create_connection(proxy.upstream, timeout=1)
            with proxy.lock:
                if not proxy.online:
                    return
                proxy.active.update([front, back])
            while True:
                ready, _, _ = select.select([front, back], [], [], 1)
                for source in ready:
                    body = source.recv(65536)
                    if not body:
                        return
                    (back if source is front else front).sendall(body)
        except (OSError, ValueError):
            pass
        finally:
            with proxy.lock:
                proxy.active.discard(front)
                if back:
                    proxy.active.discard(back)
            if back:
                back.close()


def main():
    with socket.create_connection(('127.0.0.1', int(os.environ.get('REDIS_HOST_PORT', '16379'))), timeout=2) as probe:
        probe.sendall(b'*1\r\n$4\r\nPING\r\n')
        if probe.recv(64) != b'+PONG\r\n':
            raise RuntimeError('start the project Redis using make dev-rate-up')
    account = admin('tenant-create', '--name', 'Rate-limit smoke tenant')
    children, logs, health_addresses = [], [], []
    received, failures = [], []
    lock = threading.Lock()
    gate = threading.Barrier(2)
    marker = 'rate-limit-payload-must-not-be-logged'
    prefix = 'rf-smoke-' + uuid.uuid4().hex
    redis_proxy = RedisProxy(('127.0.0.1', int(os.environ.get('REDIS_HOST_PORT', '16379'))))
    redis_thread = threading.Thread(target=redis_proxy.serve_forever, daemon=True)
    redis_thread.start()
    receiver_address, api_address = f'127.0.0.1:{free_port()}', f'127.0.0.1:{free_port()}'

    class WebhookProxy(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers['Content-Length']))
            headers = {name: self.headers[name] for name in
                       ['webhook-id', 'webhook-timestamp', 'webhook-signature', 'Content-Type']}
            try:
                with lock:
                    received.append((body, headers))
                    ordinal = len(received)
                # One slot per worker: two concurrent requests prove both processes participated.
                if ordinal <= 2:
                    if ordinal == 2:
                        redis_proxy.set_online(False)
                    gate.wait(timeout=6)
                req = urllib.request.Request('http://' + receiver_address + '/webhooks',
                                             data=body, headers=headers, method='POST')
                with urllib.request.urlopen(req, timeout=3) as response:
                    assert response.status == 204
                self.send_response(204)
                self.end_headers()
            except Exception:
                with lock:
                    failures.append('webhook gate/receiver failure')
                self.send_response(500)
                self.end_headers()

    webhook = ThreadingHTTPServer(('127.0.0.1', 0), WebhookProxy)
    webhook_thread = threading.Thread(target=webhook.serve_forever, daemon=True)
    webhook_thread.start()
    destination = f'http://localhost:{webhook.server_port}/webhooks'
    env = dict(os.environ, API_ADDR=api_address, DEV_RECEIVER_URL=destination,
               WORKER_CONCURRENCY='1', QUEUE_POLL_INTERVAL='20ms',
               RATE_LIMIT_ENABLED='true', REDIS_URL=f'redis://127.0.0.1:{redis_proxy.server_address[1]}/15',
               RATE_LIMIT_PREFIX=prefix, TENANT_RATE_PER_SECOND='1', TENANT_BURST='1',
               MAX_TENANT_IN_FLIGHT='2', OUTBOUND_TIMEOUT='8s', DELIVERY_LEASE='30s')

    def get(address, path):
        try:
            response = urllib.request.urlopen('http://' + address + path, timeout=2)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.read()

    def call(method, path, data=None, key=None):
        headers = {'Authorization': 'Bearer ' + account['api_key'], 'Content-Type': 'application/json'}
        if key:
            headers['Idempotency-Key'] = key
        req = urllib.request.Request('http://' + api_address + path,
                                     data=None if data is None else json.dumps(data).encode(),
                                     headers=headers, method=method)
        with urllib.request.urlopen(req, timeout=5) as response:
            return response.status, json.load(response)

    def wait(predicate, description, timeout=12):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                if predicate():
                    return
            except OSError:
                pass
            time.sleep(.03)
        raise RuntimeError(description)

    def rows():
        return call('GET', '/v1/deliveries')[1]['data']

    def outage_state(expected_delivered):
        records = rows()
        return (len(records) == 4
                and sum(row['status'] == 'delivered' for row in records) == expected_delivered
                and all(row['cycle_attempt_count'] == 0
                        and row.get('last_error_code') == 'limiter_unavailable'
                        for row in records if row['status'] != 'delivered'))

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
            wait(lambda: get(api_address, '/health/ready')[0] == 200, 'API not ready')
            _, created = call('POST', '/v1/endpoints',
                              {'url': destination, 'subscribed_event_types': ['order.created']})
            launch('receiver', dict(env, RECEIVER_ADDR=receiver_address,
                                   RECEIVER_SECRETS=created['signing_secret']))
            wait(lambda: get(receiver_address, '/health/ready')[0] == 200, 'receiver not ready')
            for i in range(4):
                code, accepted = call('POST', '/v1/events',
                                      {'type': 'order.created', 'data': {'marker': marker, 'n': i}},
                                      f'rate-event-{i}')
                assert code == 202 and accepted['delivery_count'] == 1
            for _ in range(2):
                address = f'127.0.0.1:{free_port()}'
                launch('worker', dict(env, WORKER_ADDR=address))
                health_addresses.append(address)
            # Workers start degraded and keep all work durable with no attempts.
            wait(lambda: outage_state(0), 'startup outage did not defer without attempts')
            for address in health_addresses:
                code, raw = get(address, '/health/ready')
                assert code == 503 and b'limiter_unavailable' in raw
                assert get(address, '/health/live')[0] == 200
            assert not received
            redis_proxy.set_online(True)
            # Receiver gate cuts existing Redis connections after two authorized requests.
            wait(lambda: len(received) == 2 and outage_state(2), 'runtime outage did not preserve pending work')
            for address in health_addresses:
                assert get(address, '/health/ready')[0] == 503
            assert get(api_address, '/health/ready')[0] == 200
            pending = [row for row in rows() if row['status'] != 'delivered']
            for row in pending:
                _, history = call('GET', '/v1/deliveries/' + row['id'] + '/attempts')
                assert history['data'] == []
            redis_proxy.set_online(True)
            wait(lambda: all(row['status'] == 'delivered' for row in rows()), 'restored Redis did not resume delivery')
            worker_ids = set()
            for row in rows():
                assert row['cycle_attempt_count'] == row['lifetime_attempt_count'] == 1
                _, history = call('GET', '/v1/deliveries/' + row['id'] + '/attempts')
                assert len(history['data']) == 1 and history['data'][0]['outcome'] == 'success'
                worker_ids.add(history['data'][0]['worker_id'])
            assert len(worker_ids) == 2 and len(received) == 4 and not failures
            assert receiver_counts() == (4, 4)
            errors = 0
            for address in health_addresses:
                wait(lambda: get(address, '/health/ready')[0] == 200, 'worker readiness did not recover')
                code, raw = get(address, '/metrics')
                assert code == 200
                lines = raw.decode().splitlines()
                errors += sum(int(line.split()[1]) for line in lines
                              if line.startswith(('relayforge_rate_limit_errors_total ', 'relayforge_rate_limit_errors_total{')))
                assert account['tenant_id'] not in raw.decode()
            assert errors > 0
            for child in reversed(children):
                stop(child)
            children.clear()
            for log in logs:
                log.seek(0)
                output = log.read().decode()
                assert all(value not in output for value in
                           [marker, account['api_key'], created['signing_secret'], env['ENCRYPTION_KEY']])
            print('PASS: two compiled workers shared one rate=1/burst=1 bucket; both worker IDs persisted')
            print('PASS: startup/runtime Redis outages deferred without attempts; readiness degraded; same workers resumed after restore')
            print('PASS: four signed deliveries, four receiver effects, bounded counters, no payload/credential logs')
    finally:
        for child in reversed(children):
            if child.poll() is None:
                child.kill()
                child.wait(timeout=5)
        redis_proxy.set_online(False)
        redis_proxy.shutdown()
        redis_proxy.server_close()
        redis_thread.join(timeout=5)
        webhook.shutdown()
        webhook.server_close()
        webhook_thread.join(timeout=5)
        admin('tenant-disable', '--tenant', account['tenant_id'])


if __name__ == '__main__':
    with isolated_database():
        main()
