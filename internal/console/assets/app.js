'use strict';
const $ = s => document.querySelector(s);
const titles = {deliveries:['Overview','A little clarity for every delivery.'],events:['Events','Start something. We’ll handle the handoff.'],endpoints:['Endpoints','The right message. The right destination.']};
const labels = {pending:'Queued',in_flight:'Sending',retry_scheduled:'Retry scheduled',delivered:'Delivered',dead_letter:'Failed',cancelled:'Cancelled',enabled:'Enabled',disabled:'Paused'};
let hostedAuth = false, demoMode = false;
let csrf = '', view = 'deliveries', epoch = 0, filters = {}, cursors = {}, pending = false;
let live = true, lastPollAt = 0, currentCursor = '', seenDeliveries = new Set(), hasDeliveryPage = false;
const fmt = x => x ? new Date(x).toLocaleString() : '—';
const short = x => x ? x.slice(0,8) + '…' : '—';
function node(tag, text, cls) { const n=document.createElement(tag); if(text!==undefined)n.textContent=String(text); if(cls)n.className=cls; return n; }
function message(text,error=false){const n=$('#notice');n.textContent=text;n.className=error?'error':'';n.hidden=false;}
function clearMessage(){$('#notice').hidden=true;}
function button(text,run,cls='secondary'){const n=node('button',text,cls);n.type='button';n.addEventListener('click',()=>busy(n,run));return n;}
async function busy(n,run){if(n.disabled)return;const text=n.textContent;n.disabled=true;n.textContent='Working…';try{await run();}catch(e){message(e.message,true);}finally{n.disabled=false;n.textContent=text;}}
function connected(ok) {
 $('#account-tools').hidden=!ok||!hostedAuth;
 $('#login').hidden=ok; $('#app').hidden=!ok; $('#logout').hidden=!ok; $('#heading-actions').hidden=!ok;
 $('#session-status').textContent=ok?'Session active':'Disconnected';
 $('#side-status').textContent=ok?'Workspace connected':'Ready to connect';
 $('#connection-light').classList.toggle('connected',ok);
 if(!ok) {
  csrf=''; epoch++; pending=false; currentCursor=''; hasDeliveryPage=false; seenDeliveries.clear();
  for(const x of ['deliveries','events','endpoints']) $('#'+x+'-list').replaceChildren();
  $('#new-api-key').textContent=''; $('#keys-list').replaceChildren();
  $('#publish-result').textContent=''; $('#secret-value').textContent=''; $('#detail-content').replaceChildren();
  $('#updated-at').textContent=''; resetOverview();
  document.querySelectorAll('dialog[open]').forEach(d=>d.close());
 }
}
async function request(path,method='GET',body, key){const sessionAtStart=csrf;let response;try{response=await fetch(path,{method,credentials:'same-origin',cache:'no-store',redirect:'error',headers:{'Content-Type':'application/json',...(csrf?{'X-CSRF-Token':csrf}:{}),...(key?{'Idempotency-Key':key}:{})},body:body===undefined?undefined:JSON.stringify(body)});}catch{throw Error('Connection interrupted. Retry with the same idempotency key and unchanged data.');}const data=response.status===204?null:await response.json();if(path.startsWith('/api/') && sessionAtStart!==csrf)throw Error('Session changed. Refresh before continuing.');if(!response.ok){if(response.status===401)connected(false);throw Error((data.message||'Request failed')+(data.request_id?' · Request '+data.request_id:''));}return data;}
function api(path,method,body,key){return request('/api/v1/'+path,method,body,key);}
function badge(status){return node('span',labels[status]||status.replaceAll('_',' '),'badge '+status);}
function empty(target,text){target.replaceChildren(node('div',text,'empty'));}
function formData(id){return Object.fromEntries(new FormData($(id)));}
function query(){return new URLSearchParams(view==='deliveries'?filters:{});}
function resetOverview() {
 for(const id of ['stat-total','stat-delivered','stat-progress','stat-failed','chart-total']) $('#'+id).textContent='—';
 $('#activity-bars').replaceChildren();
 $('#page-count').textContent='Delivery counts unavailable';
}
function overview(items) {
 const count=status=>items.filter(x=>x.status===status).length;
 $('#stat-total').textContent=items.length;
 $('#stat-delivered').textContent=count('delivered');
 $('#stat-progress').textContent=count('pending')+count('in_flight')+count('retry_scheduled');
 $('#stat-failed').textContent=count('dead_letter');
 const filtered=Object.keys(filters).length>0;
 $('#stats-scope').textContent=(filtered?'Filtered page':'Current page')+' · up to 25 deliveries';
 $('#page-count').textContent=items.length+' '+(items.length===1?'delivery':'deliveries')+' on this page · newest first';
 const now=Date.now(), start=now-12*3600000;
 const buckets=Array.from({length:12},()=>({count:0,failed:0,pending:0}));
 for(const item of items) {
  const i=Math.floor((new Date(item.created_at).getTime()-start)/3600000);
  if(i>=0&&i<12) { buckets[i].count++; if(item.status==='dead_letter') buckets[i].failed++; if(['pending','in_flight','retry_scheduled'].includes(item.status)) buckets[i].pending++; }
 }
 const max=Math.max(1,...buckets.map(b=>b.count));
 $('#chart-total').textContent=buckets.reduce((sum,b)=>sum+b.count,0);
 const bars=$('#activity-bars'); bars.replaceChildren();
 buckets.forEach((bucket,i)=>{
  const bar=node('div',undefined,'activity-bar '+(bucket.count?'h'+Math.max(1,Math.round(bucket.count/max*10)):'empty-bar'));
  if(bucket.failed) bar.classList.add('issue'); else if(bucket.pending) bar.classList.add('processing');
  const time=new Date(start+i*3600000).toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
  const text=time+'–'+new Date(start+(i+1)*3600000).toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'})+': '+bucket.count+' '+(bucket.count===1?'delivery':'deliveries');
  bar.title=text; bar.setAttribute('role','listitem'); bar.setAttribute('aria-label',text); bars.append(bar);
 });
 $('#chart-start').textContent=new Date(start).toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
}
function syncQuickFilters() {
 document.querySelectorAll('[data-status]').forEach(n=>{const selected=n.dataset.status===(filters.status||'');n.classList.toggle('selected',selected);n.setAttribute('aria-pressed',String(selected));});
}
async function load(next=false,background=false) {
 const generation=++epoch, resource=view, target=$('#'+resource+'-list');
 const q=query(); q.set('limit','25');
 const cursor=background?currentCursor:next?cursors[resource]:'';
 if(cursor) q.set('cursor',cursor);
 pending=true; lastPollAt=Date.now(); $('#refresh').disabled=true;
 if(!background) { $('#'+resource+'-next').hidden=true; empty(target,'Loading '+resource+'…'); }
 try {
  const result=await api(resource+'?'+q); if(generation!==epoch) return;
  currentCursor=cursor||''; cursors[resource]=result.next_cursor||''; target.replaceChildren();
  const items=result.data||[];
  if(resource==='deliveries') overview(items);
  if(!items.length) {
   empty(target,resource==='deliveries'?'A clear stream. No deliveries match this view.':resource==='events'?'Your first event starts here. Publish one to see its journey.':'Your next connection starts here. Add a receiver to get going.');
   if(resource==='deliveries') target.querySelector('.empty').append(button('Publish an event →',()=>navigate('events')));
  } else if(resource==='deliveries') renderDeliveries(target,items);
  else if(resource==='events') items.forEach(x=>renderEvent(target,x));
  else items.forEach(x=>renderEndpoint(target,x));
  $('#'+resource+'-next').hidden=!cursors[resource];
  $('#updated-at').textContent='Updated '+new Date().toLocaleTimeString([],{hour:'2-digit',minute:'2-digit',second:'2-digit'});
  $('#live-status').textContent=live?'Updates every 8s':'Live updates paused';
 } catch(e) {
  if(generation===epoch) {
   if(!background) { empty(target,'This view is taking a break. Refresh to try again.'); if(resource==='deliveries')resetOverview(); }
   $('#live-status').textContent='Update unavailable · retrying';
   if(!background) message(e.message,true);
  }
 } finally { if(generation===epoch) {pending=false;$('#refresh').disabled=false;} }
}
function table(headers){const t=node('table'),head=node('thead'),row=node('tr');headers.forEach(h=>row.append(node('th',h)));head.append(row);t.append(head);const body=node('tbody');t.append(body);return [t,body];}
function renderDeliveries(target,items) {
 const [t,b]=table(['Delivery','Destination','Status','Attempts','Last activity','']);
 items.forEach(x=>{
  const r=node('tr'); if(hasDeliveryPage&&!seenDeliveries.has(x.id)) r.className='new-row';
  const identity=node('td'); identity.append(node('div',short(x.id),'mono'),node('div','Event '+short(x.event_id),'muted')); identity.title=x.id;
  let destination=x.url_snapshot; try {destination=new URL(x.url_snapshot).host;}catch{}
  const status=node('td'); status.append(badge(x.status));
  const action=node('td'); action.append(button('View →',()=>detail(x.id)));
  r.append(identity,node('td',destination),status,node('td',x.lifetime_attempt_count+' · cycle '+x.cycle_number),node('td',fmt(x.updated_at)),action); b.append(r);
 });
 seenDeliveries=new Set(items.map(x=>x.id)); hasDeliveryPage=true; target.append(t);
}
function renderEvent(target,x){const card=node('div',undefined,'row-card');card.append(node('h3',x.type),node('p',x.id,'mono'),node('p',fmt(x.created_at)));card.append(button('View event →',()=>eventDetail(x.id)));target.append(card);}
function renderEndpoint(target,x){if(demoMode){const card=node('div',undefined,'row-card');card.append(node('h3','Built-in signed receiver'),badge('enabled'),node('p','Receives sample events inside this server. External destinations are disabled.'));target.append(card);return;}const card=node('div',undefined,'row-card');card.append(node('h3',x.url),badge(x.enabled?'enabled':'disabled'),node('p',x.id,'mono'),node('p',x.subscribed_event_types.join(', ')));const actions=node('div',undefined,'row-actions');actions.append(button(x.enabled?'Pause':'Enable',async()=>{await api('endpoints/'+x.id,'PATCH',{enabled:!x.enabled});message('Endpoint '+(x.enabled?'paused.':'enabled.'));await load();}),button('Rotate secret',async()=>{if(!confirm('Rotate the signing secret? The previous secret remains valid for 24 hours.'))return;const result=await api('endpoints/'+x.id+'/rotate-secret','POST',{});showSecret(result.signing_secret);}),button('Delete',async()=>{if(!confirm('Delete this endpoint and cancel unfinished deliveries?'))return;await api('endpoints/'+x.id,'DELETE');message('Endpoint deleted.');await load();},'secondary danger'));card.append(actions);target.append(card);}
function showSecret(secret){$('#secret-value').textContent=secret;$('#secret-dialog').showModal();}
function jsonBlock(parent,value){parent.append(node('pre',JSON.stringify(value,null,2)));}
function facts(parent,entries) {
 const grid=node('div',undefined,'detail-summary');
 for(const [label,value] of entries) {const field=node('div',undefined,'detail-field');field.append(node('span',label),node('strong',value??'—'));grid.append(field);}
 parent.append(grid);
}
function technical(parent,value) {const details=node('details',undefined,'technical');details.append(node('summary','Technical details & identifiers'));jsonBlock(details,value);parent.append(details);}

async function eventDetail(id){const result=await api('events/'+id);$('#detail-title').textContent='Event detail';const content=$('#detail-content');content.replaceChildren();facts(content,[['Event type',result.event.type],['Accepted',fmt(result.event.created_at)]]);content.append(node('h3','Event payload'));jsonBlock(content,result.payload);technical(content,result);content.append(button('Find deliveries',()=>{ $('#detail-dialog').close();filters={event_id:id};$('#filters').elements.event_id.value=id;$('#filters').elements.endpoint_id.value='';$('#filters').elements.status.value='';return navigate('deliveries');}));$('#detail-dialog').showModal();}
async function detail(id){const result=await api('deliveries/'+id);$('#detail-title').textContent='Delivery detail';const content=$('#detail-content');content.replaceChildren();content.append(badge(result.status));facts(content,[['Destination',result.url_snapshot],['Last response',result.last_http_status?'HTTP '+result.last_http_status:'Not sent yet'],['Total attempts',result.lifetime_attempt_count],['Last activity',fmt(result.updated_at)]]);if(result.last_error_code) content.append(node('p','Last issue: '+result.last_error_code.replaceAll('_',' ')));technical(content,result);if(result.status==='dead_letter'){
 const section=node('section');section.append(node('h3','Replay this delivery'),node('p','Starts a new retry cycle with the original payload, URL, and delivery ID. The endpoint must be enabled and the event unexpired.'));
 const key=node('input');key.value=crypto.randomUUID();key.maxLength=128;key.setAttribute('aria-label','Replay idempotency key');section.append(key,button('Replay delivery',async()=>{if(!confirm('Start a new replay cycle?'))return;const receipt=await api('deliveries/'+id+'/replay','POST',{},key.value);message('Replay accepted · cycle '+receipt.new_cycle_number);section.replaceChildren(node('p','Replay accepted. Close and refresh to see the new state.'));await load();}));content.append(section);
 }
 for(const [route,label] of [['attempts','Attempt history'],['replays','Replay history']]){
 const section=node('section');section.append(node('h3',label));const rows=node('div');section.append(rows);content.append(section);
 async function page(cursor=''){const q=new URLSearchParams({limit:'25'});if(cursor)q.set('cursor',cursor);empty(rows,'Loading history…');try{const result=await api('deliveries/'+id+'/'+route+'?'+q);rows.replaceChildren();if(!result.data?.length)empty(rows,'No '+route+' recorded.');else if(route==='attempts'){const [t,b]=table(['Cycle / attempt','Outcome','HTTP','Latency','Started / finished']);result.data.forEach(a=>{const row=node('tr');for(const value of [a.cycle_number+' / '+a.attempt_number,(a.error_code||a.outcome).replaceAll('_',' '),a.http_status||'—',a.latency_ms===undefined?'—':a.latency_ms+' ms',fmt(a.started_at)+' / '+fmt(a.finished_at)])row.append(node('td',value));b.append(row);});rows.append(t);}else jsonBlock(rows,result.data);if(result.next_cursor)rows.append(button('Next history page →',()=>page(result.next_cursor)));}catch(e){empty(rows,'History unavailable.');rows.append(button('Retry history',()=>page(cursor)));message(e.message,true);}}
 await page();
 }
 if(csrf)$('#detail-dialog').showModal();
}
async function navigate(next){view=next;clearMessage();$('#breadcrumb').textContent=next==='deliveries'?'Overview':next[0].toUpperCase()+next.slice(1);currentCursor='';$('#title').replaceChildren(document.createTextNode(titles[next][0]),node('span','.','heading-dot'));$('#subtitle').textContent=titles[next][1];for(const name of Object.keys(titles)){$('#'+name+'-view').hidden=name!==next;document.querySelector('[data-view="'+name+'"]').classList.toggle('active',name===next);}syncQuickFilters();if(csrf)await load();}
document.querySelectorAll('[data-view]').forEach(n=>n.addEventListener('click',()=>navigate(n.dataset.view)));
$('#login-form').addEventListener('submit',e=>{e.preventDefault();const key=$('#api-key').value;$('#api-key').value='';busy(e.submitter,async()=>{const result=await request('/session','POST',{key});csrf=result.csrf;connected(true);message('Workspace connected.');await load();});});
$('#logout').addEventListener('click',()=>busy($('#logout'),async()=>{await request('/session','DELETE');connected(false);message('Disconnected. Server-side credentials removed.');}));
$('#refresh').addEventListener('click',()=>load());
$('#filters').addEventListener('submit',e=>{e.preventDefault();filters=Object.fromEntries(Object.entries(formData('#filters')).filter(([,v])=>v));syncQuickFilters();clearMessage();load();});
for(const name of Object.keys(titles))$('#'+name+'-next').addEventListener('click',()=>load(true));
$('#new-key').addEventListener('click',()=>{$('#publish').elements.key.value=crypto.randomUUID();$('#publish-result').hidden=true;});
$('#publish').elements.key.value=crypto.randomUUID();
$('#publish').addEventListener('submit',e=>{e.preventDefault();busy(e.submitter,async()=>{const input=formData('#publish');let data;try{data=JSON.parse(input.data);}catch{throw Error('Data must be valid JSON. Nothing was submitted.');}const result=await api('events','POST',{type:input.type,data},input.key);$('#publish-result').hidden=false;$('#publish-result').textContent=JSON.stringify(result,null,2);message('Event accepted · '+result.delivery_count+' deliveries. Keep this key for retries; choose New key for your next event.');await load();});});
$('#endpoint-form').addEventListener('submit',e=>{e.preventDefault();busy(e.submitter,async()=>{const input=formData('#endpoint-form');const result=await api('endpoints','POST',{url:input.url,subscribed_event_types:input.types.split(',').map(t=>t.trim())});showSecret(result.signing_secret);message('Endpoint created. Save its signing secret.');await load();});});
$('#secret-close').addEventListener('click',()=>$('#secret-dialog').close());$('#secret-dialog').addEventListener('close',()=>{$('#secret-value').textContent='';});$('#detail-close').addEventListener('click',()=>$('#detail-dialog').close());$('#detail-dialog').addEventListener('close',()=>$('#detail-content').replaceChildren());
(async()=>{try{const config=await request('/auth/config');if(config.demo)return;const result=await request('/session');csrf=result.csrf;connected(true);await load();}catch{connected(false);}})();

$('#today').textContent=new Intl.DateTimeFormat(undefined,{weekday:'long',month:'long',day:'numeric'}).format(new Date()).toUpperCase();
document.querySelectorAll('[data-go]').forEach(n=>n.addEventListener('click',()=>navigate(n.dataset.go)));
document.querySelectorAll('[data-status]').forEach(n=>n.addEventListener('click',()=>{
 if(n.dataset.status) filters.status=n.dataset.status; else delete filters.status;
 $('#filters').elements.status.value=filters.status||''; syncQuickFilters(); clearMessage(); load();
}));
$('#filter-toggle').addEventListener('click',()=>{
 const panel=$('#filter-panel'); panel.hidden=!panel.hidden;
 $('#filter-toggle').setAttribute('aria-expanded',String(!panel.hidden));
});
$('#live-toggle').addEventListener('click',()=>{
 live=!live; $('#live-toggle').setAttribute('aria-checked',String(live));
 $('#live-toggle').replaceChildren(node('span',undefined,'pulse-dot'),document.createTextNode(live?'Live':'Paused'));
 $('#live-status').textContent=live?'Updates every 8s':'Live updates paused';
 if(live&&csrf&&!pending&&view==='deliveries') load(false,true);
});
setInterval(()=>{
 if(!csrf||!live||pending||document.hidden||view!=='deliveries') return;
 if(document.querySelector('dialog[open]')) {$('#live-status').textContent='Paused while inspecting';return;}
 if(Date.now()-lastPollAt>=8000) load(false,true);
},1000);
syncQuickFilters();
