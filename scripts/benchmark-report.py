#!/usr/bin/env python3
"""Regenerate tables from retained raw results; fail if summaries disagree."""
import argparse
import json
from pathlib import Path
from benchmark import percentiles


def fmt(value):
    return '—' if value is None else f'{value:.2f}'


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('directory',type=Path)
    parser.add_argument('--output',required=True,type=Path)
    args=parser.parse_args()
    summary=json.loads((args.directory/'summary.json').read_text())
    lines=['# Per-run local results', '', 'Generated from retained raw JSON by `scripts/benchmark-report.py`. These are short local experiments; see [method and limits](README.md). Rates are per actual offer window; acknowledgment latency includes only eventual delivered work and must be read with outcome counts.', '', '| Workload / repetition | Offered events/s | Fan-out | Accepted / offered | Attempts/s | Successes/s | Ingestion p95 ms | Ack p95 ms | Delivered / dead / unfinished | End backlog | Drain s |', '| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
    extra=['', '## Tail latencies and backlog', '', '| Workload / repetition | Ingestion p50 / p95 / p99 ms | Arrival-response p99 ms | Generator lag p99 ms | Queue p95 ms | HTTP p95 ms | Ack p99 ms | Peak sampled backlog |', '| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |']
    resources=['', '## Database resource samples', '', 'Coarse Docker samples; CPU uses Docker percent semantics (may exceed 100% across cores). Memory includes PostgreSQL caches and is not API/worker RSS. Sampling itself adds load. Aggregate child resource usage includes admin/migration/Docker clients and is retained in summary.json without attributing it to the Go processes.', '', '| Workload / repetition | Database CPU peak % | Database memory samples (first → last) |', '| --- | ---: | --- |']
    counts={'offered':0,'accepted':0,'delivered':0,'dead_letter_deliveries':0,'unfinished_deliveries':0,'nonaccepted_or_dropped':0}
    for s in summary['runs']:
        name=s['workload']+'-'+str(s['offered_events_per_s'])+'-r'+str(s['repetition'])
        raw=json.loads((args.directory/(name+'.json')).read_text())
        assert raw['summary']==s
        assert len(raw['offers'])==s['offered']
        assert sum(o['status']==202 for o in raw['offers'])==s['accepted']
        assert len(raw['deliveries'])==s['accepted']*s['endpoints_per_event']
        assert sum(d['status']=='delivered' for d in raw['deliveries'])==s['delivered']
        assert sum(d['status']=='dead_letter' for d in raw['deliveries'])==s['dead_letter_deliveries']
        assert percentiles([o['request_ms'] for o in raw['offers'] if 'request_ms' in o])==s['ingestion_request_ms']
        assert s['delivered']+s['dead_letter_deliveries']+s['unfinished_deliveries']==len(raw['deliveries'])
        for k in counts:counts[k]+=s[k]
        label=s['workload']+' / '+str(s['repetition'])
        lines.append(f"| {label} | {s['offered_events_per_s']} | {s['endpoints_per_event']} | {s['accepted']} / {s['offered']} | {fmt(s['attempts_per_s_in_window'])} | {fmt(s['successes_per_s_in_window'])} | {fmt(s['ingestion_request_ms']['p95'])} | {fmt(s['ack_ms_successes_only']['p95'])} | {s['delivered']} / {s['dead_letter_deliveries']} / {s['unfinished_deliveries']} | {s['window_end_backlog']} | {fmt(s['drain_s_after_responses'])} |")
        peak=max((sum(q['statuses'].get(k,0) for k in ('pending','in_flight','retry_scheduled')) for q in raw['queue_samples']),default=0)
        lat=' / '.join(fmt(s['ingestion_request_ms'][q]) for q in ('p50','p95','p99'))
        extra.append(f"| {label} @ {s['offered_events_per_s']}/s | {lat} | {fmt(s['arrival_to_response_ms']['p99'])} | {fmt(s['generator_lag_ms']['p99'])} | {fmt(s['first_attempt_queue_ms']['p95'])} | {fmt(s['attempt_http_ms']['p95'])} | {fmt(s['ack_ms_successes_only']['p99'])} | {peak} |")
        samples=raw['database_resource_samples']
        cpu=max((float(x['postgres']['CPUPerc'].rstrip('%')) for x in samples),default=None)
        memory=(samples[0]['postgres']['MemUsage']+' → '+samples[-1]['postgres']['MemUsage']) if samples else 'unavailable'
        resources.append(f'| {label} @ {s["offered_events_per_s"]}/s | {fmt(cpu)} | {memory} |')
        if s['workload']=='crash':
            resources.extend(['',f"Crash kill -> all measured deliveries observed terminal: **{s['crash_to_all_recovered_s']:.3f} s**, with {s['lease_expirations']} expired leases. Both HTTP requests entered before SIGKILL; exact abandoned attempts and their later attempts remain in raw JSON. This includes recovery scheduling/backoff and polling overhead."])
    args.output.write_text('\n'.join(lines+extra+resources+['','Totals across measured cohorts: `'+json.dumps(counts,sort_keys=True)+'`. Warmup excluded.',''])+'\n')
    print('PASS: raw cohort counts and percentiles match every summary; report regenerated')


if __name__=='__main__':main()
