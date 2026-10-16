#!/usr/bin/env python3
"""Bounded, local-only benchmark experiments; never uses an existing tenant.

Results contain synthetic IDs/times, never credentials or payloads. See benchmark
methodology before interpreting short developer-machine measurements.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
from contextlib import ExitStack
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
import os
from pathlib import Path
import platform
import re
import resource
import runpy
import signal
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from urllib.parse import parse_qs, urlsplit
from smoke_db import isolated_database

ROOT = Path(__file__).resolve().parents[1]
helpers = runpy.run_path(str(ROOT / 'scripts/smoke-worker.py'))
admin, free_port = helpers['admin'], helpers['free_port']


def sql(statement):
    schema = parse_qs(urlsplit(os.environ['DATABASE_URL']).query).get('search_path', [''])[0]
    if not re.fullmatch(r'rf_smoke_[0-9a-f]{32}', schema):
        raise RuntimeError('benchmark SQL requires its disposable schema')
    result = subprocess.run(['docker', 'compose', '--env-file', str(ROOT / '.env'),
        '-f', str(ROOT / 'deploy/compose.yaml'), 'exec', '-T', 'postgres',
        'psql', '-U', 'relayforge', '-d', 'relayforge_dev', '-qAt', '-v',
        'ON_ERROR_STOP=1', '-c', 'SET search_path TO '+schema+'; '+statement],
        capture_output=True, timeout=45)
    if result.returncode:
        raise RuntimeError('benchmark SQL failed; connection details withheld')
    return result.stdout.decode().strip()


def call(address, method, path, token='', data=None, key=None):
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer '+token
    if key:
        headers['Idempotency-Key'] = key
    req = urllib.request.Request('http://'+address+path, method=method, headers=headers,
                                 data=data)
    try:
        response = urllib.request.urlopen(req, timeout=7)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.read()


def ready(address):
    deadline = time.monotonic()+15
    while time.monotonic()<deadline:
        try:
            if call(address, 'GET', '/health/ready')[0]==200:
                return
        except OSError:
            pass
        time.sleep(.05)
    raise RuntimeError('benchmark process readiness failed')


def percentiles(values):
    a = sorted(values)
    return {name: a[max(0, math.ceil(q*len(a))-1)] if a else None
            for name, q in [('p50', .50), ('p95', .95), ('p99', .99)]}


def snapshot():
    return json.loads(sql("SELECT json_build_object('statuses',(SELECT COALESCE(json_object_agg(status,n),'{}') FROM (SELECT status,count(*) n FROM deliveries GROUP BY status) s),'attempts',(SELECT count(*) FROM delivery_attempts),'successes',(SELECT count(*) FROM delivery_attempts WHERE outcome='success'),'lease_expirations',(SELECT count(*) FROM delivery_attempts WHERE error_code='lease_expired'))"))


def experiment(kind, rate, duration, warmup, repetition, output):
    packets, lock = [], threading.Lock()
    state = {'restore_at': float('inf'), 'gate': threading.Event(), 'entered': threading.Event()}

    class Receiver(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            self.rfile.read(int(self.headers.get('Content-Length', '0')))
            started = time.monotonic()
            if kind=='slow':
                time.sleep(.2)
            if kind=='crash':
                state['entered'].set()
                state['gate'].wait(timeout=15)
            code = 400 if kind=='permanent' else 503 if kind=='outage' and started<state['restore_at'] else 204
            try:
                self.send_response(code)
                self.end_headers()
            except OSError:
                pass
            with lock:
                packets.append({'started_offset_s': started-state.get('start', started),
                                'http_duration_ms': (time.monotonic()-started)*1000,
                                'status': code})

    receiver = ThreadingHTTPServer(('127.0.0.1', 0), Receiver)
    threading.Thread(target=receiver.serve_forever, daemon=True).start()
    accounts = [admin('tenant-create', '--name', 'Benchmark synthetic tenant') for _ in range(4)]
    api = f'127.0.0.1:{free_port()}'
    worker_addresses = [f'127.0.0.1:{free_port()}' for _ in range(2)]
    env = dict(os.environ, API_ADDR=api, WORKER_CONCURRENCY='4', DB_MAX_CONNS='10',
               QUEUE_POLL_INTERVAL='20ms', MAX_TENANT_IN_FLIGHT='4', MAX_TENANT_BACKLOG='10000',
               RATE_LIMIT_ENABLED='false', TRACE_ENDPOINT='',
               DEV_RECEIVER_URL=f'http://localhost:{receiver.server_port}/webhooks',
               OUTBOUND_TIMEOUT='10s', DELIVERY_LEASE='30s')
    children, logs, endpoint_ids = [], [], []
    fanout = 10 if kind=='fanout' else 1
    try:
        with ExitStack() as stack:
            def launch(role, address=None):
                log = stack.enter_context(tempfile.TemporaryFile())
                e = dict(env)
                if address:
                    e['WORKER_ADDR'] = address
                p = subprocess.Popen([str(ROOT/'bin'/role)], env=e, stdout=log, stderr=log)
                children.append(p)
                logs.append(log)
                return p

            launch('api')
            ready(api)
            for account in accounts:
                for _ in range(fanout):
                    code, raw = call(api, 'POST', '/v1/endpoints', account['api_key'],
                        json.dumps({'url': env['DEV_RECEIVER_URL'], 'subscribed_event_types': ['bench.event']}).encode())
                    if code!=201:
                        raise RuntimeError('benchmark endpoint seed failed')
                    endpoint_ids.append(json.loads(raw)['endpoint']['id'])
            active_addresses=worker_addresses[:1] if kind=='crash' else worker_addresses
            workers = [launch('worker', a) for a in active_addresses]
            for a in active_addresses:
                ready(a)
            payload = json.dumps({'type': 'bench.event', 'data': {'synthetic': 'x'*1024}}, separators=(',', ':')).encode()
            # Warmup is separate, tagged, and drained before the measured cohort.
            for i in range(round(warmup*10)):
                code, _ = call(api, 'POST', '/v1/events', accounts[i%4]['api_key'], payload, 'warm-'+str(i))
                if code!=202:
                    raise RuntimeError('warmup acceptance failed')
                time.sleep(.1)
            # Outage/permanent warmup would poison its own receiver: use zero warmup for these cases.
            end = time.monotonic()+15
            while warmup and time.monotonic()<end:
                s = snapshot()['statuses']
                if not sum(s.get(k, 0) for k in ('pending','in_flight','retry_scheduled')):
                    break
                time.sleep(.1)
            baseline = snapshot()
            start_wall = time.time()
            start = time.monotonic()
            state.update(start=start, restore_at=start+duration/2)
            offers, samples, process_samples = [], [], []
            outstanding = threading.BoundedSemaphore(64)
            stop_monitor = threading.Event()
            def monitor():
                while not stop_monitor.is_set():
                    samples.append({'offset_s':time.monotonic()-start, **snapshot()})
                    # docker stats covers only this experiment's shared database service.
                    r=subprocess.run(['docker','stats','--no-stream','--format','{{json .}}','relayforge-postgres-1'],capture_output=True,timeout=10)
                    if r.returncode==0:
                        process_samples.append({'offset_s':time.monotonic()-start,'postgres':json.loads(r.stdout)})
                    stop_monitor.wait(.5)
            thread = threading.Thread(target=monitor, daemon=True)
            thread.start()
            def publish(i, scheduled):
                actual = time.monotonic()
                try:
                    code, raw = call(api,'POST','/v1/events',accounts[i%4]['api_key'],payload,'measured-'+str(i))
                    event_id = json.loads(raw).get('event_id') if code==202 else None
                    return {'ordinal':i,'scheduled_offset_s':scheduled-start,'generator_lag_ms':(actual-scheduled)*1000,'request_ms':(time.monotonic()-actual)*1000,'arrival_to_response_ms':(time.monotonic()-scheduled)*1000,'status':code,'event_id':event_id}
                except (OSError, ValueError):
                    return {'ordinal':i,'scheduled_offset_s':scheduled-start,'status':0,'error':'transport_or_decode'}
                finally:
                    outstanding.release()
            futures=[]
            with ThreadPoolExecutor(max_workers=64) as pool:
                for i in range(round(rate*duration)):
                    scheduled=start+i/rate
                    delay=scheduled-time.monotonic()
                    if delay>0:
                        time.sleep(delay)
                    if outstanding.acquire(blocking=False):
                        futures.append(pool.submit(publish,i,scheduled))
                    else:
                        offers.append({'ordinal':i,'scheduled_offset_s':scheduled-start,'status':-1,'error':'generator_saturated'})
                remaining=start+duration-time.monotonic()
                if remaining>0:
                    time.sleep(remaining)
                offered_end = time.monotonic()
                window_end = snapshot()
                offers.extend(f.result() for f in as_completed(futures))
            crash_at=None
            if kind=='crash':
                if not state['entered'].wait(timeout=5):raise RuntimeError('crash fixture did not enter HTTP')
                crash_at=time.monotonic()
                workers[0].kill();workers[0].wait(timeout=5)
                state['gate'].set()
                launch('worker',active_addresses[0]);ready(active_addresses[0])
            # Drain is explicitly bounded: unfinished rows remain censored, not discarded successes.
            drain_start = time.monotonic()
            deadline = drain_start+45
            while time.monotonic()<deadline:
                final = snapshot()
                if not sum(final['statuses'].get(k,0) for k in ('pending','in_flight','retry_scheduled')):
                    break
                time.sleep(.15)
            observed_end = time.monotonic()
            stop_monitor.set()
            thread.join(timeout=60)
            if thread.is_alive():raise RuntimeError('benchmark monitor did not stop')
            attempts=json.loads(sql("SELECT COALESCE(json_agg(row_to_json(x)),'[]') FROM (SELECT a.delivery_id,a.cycle_number,a.attempt_number,a.outcome,a.http_status,a.latency_ms,a.error_code,a.started_at,a.finished_at,d.created_at,d.delivered_at,d.status FROM delivery_attempts a JOIN deliveries d ON d.id=a.delivery_id JOIN ingestion_idempotency i ON i.event_id=d.event_id WHERE i.idempotency_key LIKE 'measured-%' ORDER BY a.started_at,a.id) x"))
            deliveries=json.loads(sql("SELECT COALESCE(json_agg(row_to_json(x)),'[]') FROM (SELECT d.id,d.status,d.created_at,d.delivered_at,d.lifetime_attempt_count FROM deliveries d JOIN ingestion_idempotency i ON i.event_id=d.event_id WHERE i.idempotency_key LIKE 'measured-%' ORDER BY d.id) x"))
            for p in children:
                if p.poll() is None:
                    p.send_signal(signal.SIGTERM)
            for p in children:
                if p.wait(timeout=25)!=0 and not (kind=='crash' and p is workers[0]):
                    raise RuntimeError('benchmark process exited unsuccessfully')
            # Go shutdown logs remain private; only bounded CPU/RSS summaries are retained.
            secret_values=[a['api_key'].encode() for a in accounts]
            for log in logs:
                log.seek(0)
                if any(key in log.read() for key in secret_values):
                    raise RuntimeError('credential detected in process log')
            parse_time=lambda v:datetime.strptime(v.replace('Z','+00:00'),'%Y-%m-%dT%H:%M:%S.%f%z' if '.' in v else '%Y-%m-%dT%H:%M:%S%z')
            first_queue=[(parse_time(a['started_at'])-parse_time(a['created_at'])).total_seconds()*1000 for a in attempts if a['attempt_number']==1]
            ack=[(parse_time(d['delivered_at'])-parse_time(d['created_at'])).total_seconds()*1000 for d in deliveries if d.get('delivered_at')]
            accepted=[o for o in offers if o['status']==202]
            attempted_window=sum(start_wall<=parse_time(a['started_at']).timestamp()<start_wall+offered_end-start for a in attempts)
            success_window=sum(a['outcome']=='success' and a.get('finished_at') and start_wall<=parse_time(a['finished_at']).timestamp()<start_wall+offered_end-start for a in attempts)
            summary={'workload':kind,'repetition':repetition,'offered_events_per_s':rate,'configured_duration_s':duration,'window_start_unix_s':start_wall,'actual_offer_window_s':offered_end-start,'warmup_s':warmup,'warmup_events':round(warmup*10),'tenants':4,'endpoints_per_event':fanout,'workers':len(active_addresses),'crash_to_all_recovered_s':observed_end-crash_at if crash_at else None,'crash_kill_offset_s':crash_at-start if crash_at else None,'slots_per_worker':4,'tenant_in_flight':4,'db_pool_per_process':10,'poll_ms':20,'payload_bytes':len(payload),'rate_limit_enabled':False,'tracing_enabled':False,'offered':len(offers),'accepted':len(accepted),'nonaccepted_or_dropped':len(offers)-len(accepted),'accepted_completed_by_window':sum(o['status']==202 and o.get('scheduled_offset_s',0)+(o.get('arrival_to_response_ms',1e15)/1000)<=offered_end-start for o in offers),'acceptances_per_s_total_over_offer_window':len(accepted)/(offered_end-start),'attempts_per_s_in_window':attempted_window/(offered_end-start),'successes_per_s_in_window':success_window/(offered_end-start),'drain_s_after_responses':observed_end-drain_start,'window_end_backlog':sum(window_end['statuses'].get(k,0) for k in ('pending','in_flight','retry_scheduled')),'unfinished_deliveries':sum(d['status'] in ('pending','in_flight','retry_scheduled') for d in deliveries),'dead_letter_deliveries':sum(d['status']=='dead_letter' for d in deliveries),'delivered':len(ack),'lease_expirations':final['lease_expirations']-baseline['lease_expirations'],'ingestion_request_ms':percentiles([o['request_ms'] for o in offers if 'request_ms' in o]),'arrival_to_response_ms':percentiles([o['arrival_to_response_ms'] for o in offers if 'arrival_to_response_ms' in o]),'generator_lag_ms':percentiles([o['generator_lag_ms'] for o in offers if 'generator_lag_ms' in o]),'first_attempt_queue_ms':percentiles(first_queue),'attempt_http_ms':percentiles([a['latency_ms'] for a in attempts if a['latency_ms'] is not None]),'ack_ms_successes_only':percentiles(ack),'receiver_503':sum(p['status']==503 for p in packets),'receiver_400':sum(p['status']==400 for p in packets),'final_statuses':final['statuses']}
            result={'summary':summary,'offers':sorted(offers,key=lambda x:x['ordinal']),'attempts':attempts,'deliveries':deliveries,'queue_samples':samples,'database_resource_samples':process_samples,'receiver_packets':packets}
            (output/(kind+'-'+str(rate)+'-r'+str(repetition)+'.json')).write_text(json.dumps(result,indent=2)+'\n')
            print(json.dumps(summary),flush=True)
            return summary
    finally:
        state['gate'].set()
        for p in children:
            if p.poll() is None:
                p.kill()
                p.wait(timeout=10)
        receiver.shutdown()
        receiver.server_close()


def plans(output):
    account=admin('tenant-create','--name','Planner synthetic tenant')
    api=f'127.0.0.1:{free_port()}'
    env=dict(os.environ,API_ADDR=api,TRACE_ENDPOINT='')
    with tempfile.TemporaryFile() as logs:
        p=subprocess.Popen([str(ROOT/'bin/api')],env=env,stdout=logs,stderr=logs)
        try:
            ready(api)
            code,raw=call(api,'POST','/v1/endpoints',account['api_key'],json.dumps({'url':'https://hooks.example.com/benchmark','subscribed_event_types':['bench.event']}).encode())
            if code!=201:raise RuntimeError('planner endpoint seed failed')
            endpoint=json.loads(raw)['endpoint']['id'];tenant=account['tenant_id']
            # Direct SQL fixtures ONLY in disposable schema. All outgoing bytes and hashes
            # are constructed consistently; no claims about API seeding throughput.
            sql(f"""WITH seed AS (SELECT gen_random_uuid() id,clock_timestamp() created FROM generate_series(1,50000)), bytes AS (SELECT id,created,convert_to(json_build_object('id',id,'type','bench.event','created_at',created,'data',json_build_object('fixture',true))::text,'UTF8') body FROM seed), inserted AS (INSERT INTO events(id,tenant_id,event_type,outgoing_body,body_sha256,created_at,expires_at) SELECT id,'{tenant}','bench.event',body,sha256(body),created,created+interval '30 days' FROM bytes RETURNING id,created_at) INSERT INTO deliveries(id,tenant_id,event_id,endpoint_id,url_snapshot,next_attempt_at,cycle_started_at,created_at,updated_at) SELECT gen_random_uuid(),'{tenant}',id,'{endpoint}','https://hooks.example.com/benchmark',created_at+interval '1 hour',created_at,created_at,created_at FROM inserted""")
            # Another endpoint has earlier due work. A target-specific claim must not
            # scan its fifty thousand neighbours before finding the ten target rows.
            code,raw=call(api,'POST','/v1/endpoints',account['api_key'],json.dumps({'url':'https://hooks.example.com/other','subscribed_event_types':['bench.event']}).encode())
            if code!=201:raise RuntimeError('planner second endpoint seed failed')
            other=json.loads(raw)['endpoint']['id']
            sql(f"INSERT INTO deliveries(id,tenant_id,event_id,endpoint_id,url_snapshot,next_attempt_at,cycle_started_at,created_at,updated_at) SELECT gen_random_uuid(),tenant_id,id,'{other}','https://hooks.example.com/other',created_at-interval '1 minute',created_at,created_at,created_at FROM events; UPDATE deliveries SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id IN (SELECT id FROM deliveries WHERE endpoint_id='{endpoint}' ORDER BY id DESC LIMIT 10); ANALYZE deliveries")
            claim=(ROOT/'internal/repository/postgres/claim.go').read_text()
            statement=next(q for q in re.findall(r'`([^`]+)`',claim) if q.startswith('SELECT id,event_id,url_snapshot'))
            statement=statement.replace('$1',"'"+tenant+"'").replace('$2',"'"+endpoint+"'")
            plan=json.loads(sql('EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) '+statement))
            # The live backlog check and global telemetry aggregate are also retained.
            queries={'claim':statement,'backlog':next(q for q in re.findall(r'`([^`]+)`',(ROOT/'internal/repository/postgres/admission.go').read_text()) if q.startswith('SELECT count(*)')).replace('$1',"'"+tenant+"'").replace('$2','10000'),'snapshot':next(q for q in re.findall(r'`([^`]+)`',(ROOT/'internal/repository/postgres/telemetry.go').read_text()) if q.startswith('SELECT d.status,count(*)'))}
            result={'fixture_rows':100000,'target_due_rows':10,'other_endpoint_due_rows':50000,'queries':queries,'plans':{'claim':plan}}
            for name in ('backlog','snapshot'):
                result['plans'][name]=json.loads(sql('EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) '+queries[name]))
            (output/'query-plans.json').write_text(json.dumps(result,indent=2)+'\n')
            print('Planner fixture: '+str(plan[0]['Execution Time'])+' ms claim',flush=True)
        finally:
            p.send_signal(signal.SIGTERM);p.wait(timeout=20)


def environment():
    def command(args):
        try:
            r=subprocess.run(args,capture_output=True,text=True,timeout=15)
        except (OSError, subprocess.TimeoutExpired):
            return 'unavailable'
        return r.stdout.strip() if r.returncode==0 else 'unavailable'
    return {'recorded_at':datetime.now(timezone.utc).isoformat(),'os':platform.platform(),'machine':platform.machine(),'logical_cpus':os.cpu_count(),'hardware':command(['/usr/sbin/system_profiler','SPHardwareDataType']).split('Serial Number')[0].strip(),'memory_bytes':command(['/usr/sbin/sysctl','-n','hw.memsize']),'go':command(['go','version']),'docker':command(['docker','version','--format','{{json .Server}}']),'postgres_version':command(['docker','compose','--env-file',str(ROOT/'.env'),'-f',str(ROOT/'deploy/compose.yaml'),'exec','-T','postgres','postgres','--version']),'docker_resources':command(['docker','info','--format','{"cpus":{{.NCPU}},"memory_bytes":{{.MemTotal}}}']),'topology':'Python generator + API + two Go workers + Python receiver on macOS loopback; PostgreSQL Docker Desktop published TCP; Redis unused; same shared host','process_resource_note':'Database docker stats sampled at about 1.5 s. Whole harness child CPU/RSS includes short admin/migration/docker clients; no per-process Go CPU/RSS claim.'}


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--output',required=True,type=Path)
    parser.add_argument('--duration',type=float,default=10)
    parser.add_argument('--repetitions',type=int,default=2)
    parser.add_argument('--plans-only',action='store_true')
    args=parser.parse_args()
    if not 2<=args.duration<=60 or not 1<=args.repetitions<=5:parser.error('duration 2–60; repetitions 1–5')
    if args.output.exists():parser.error('output must be a new directory to preserve raw runs')
    args.output.mkdir(parents=True)
    (args.output/'environment.json').write_text(json.dumps(environment(),indent=2)+'\n')
    with isolated_database():plans(args.output)
    summaries=[]
    if not args.plans_only:
        for kind,rate in [('healthy',10),('healthy',50),('healthy',100),('slow',50),('permanent',10),('outage',10),('fanout',10)]:
            for rep in range(1,args.repetitions+1):
                with isolated_database():
                    summaries.append(experiment(kind,rate,args.duration,2 if kind in ('healthy','slow','fanout') else 0,rep,args.output))
                if summaries[-1]['nonaccepted_or_dropped'] or summaries[-1]['unfinished_deliveries']:
                    print('Stopping rate escalation: generator/admission/unfinished work requires inspection.',flush=True)
                    break
            else:continue
            break
    if not args.plans_only:
        with isolated_database():summaries.append(experiment('crash',1,2,0,1,args.output))
    usage=resource.getrusage(resource.RUSAGE_CHILDREN)
    (args.output/'summary.json').write_text(json.dumps({'runs':summaries,'child_resource_usage':{'user_cpu_s':usage.ru_utime,'system_cpu_s':usage.ru_stime,'max_rss_native_units':usage.ru_maxrss}},indent=2)+'\n')


if __name__=='__main__':main()
