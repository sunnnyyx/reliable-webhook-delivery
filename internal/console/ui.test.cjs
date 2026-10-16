// Behavioral checks for the buildless UI. A small DOM shim complements the
// real-browser checks; it is not a layout/accessibility/browser emulator.
const {test}=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const source=readFileSync(__dirname+'/assets/app.js','utf8');
class Element {
 constructor(tag='div'){this.tag=tag;this.children=[];this.hidden=false;this.disabled=false;this.attrs={};this.dataset={};this.value='';this.className='';this.listeners={};this.elements=new Proxy({}, {get:(o,k)=>o[k]||(o[k]={value:''})});this.classList={add:n=>{this.className+=' '+n;},toggle:()=>{}};}
 set textContent(v){this.text=String(v);this.children=[];}
 get textContent(){return this.text||this.children.map(x=>x.textContent).join('');}
 append(...values){this.children.push(...values);}
 replaceChildren(...values){this.text='';this.children=values;}
 setAttribute(k,v){this.attrs[k]=v;}
 addEventListener(k,v){this.listeners[k]=v;}
 querySelector(selector){return this.children.find(x=>x.className===selector.slice(1))||new Element();}
 close(){this.open=false;}
}
async function setup(){
 const nodes=new Map(), calls=[],timers=[];
 let dialog=false, handler=()=>({data:[],next_cursor:''});
 const get=selector=>{if(!nodes.has(selector))nodes.set(selector,new Element());return nodes.get(selector);};
 const document={hidden:false,querySelector:s=>s==='dialog[open]'?(dialog?new Element():null):get(s),querySelectorAll:()=>[],createElement:tag=>new Element(tag),createTextNode:text=>{const n=new Element('text');n.textContent=text;return n;}};
 const context=vm.createContext({document,Date,Intl,URL,URLSearchParams,Set,FormData:class{},crypto:{randomUUID:()=> 'synthetic-only'},setInterval:fn=>timers.push(fn),fetch:async(path,options)=>{calls.push({path,options});if(path==='/session')return{status:401,ok:false,json:async()=>({message:'Connect'})};return{status:200,ok:true,json:async()=>handler(path)};}});
 vm.runInContext(source,context);await settle();
 vm.runInContext("csrf='synthetic-csrf'; connected(true)",context);
 return{get,document,calls,timers,run:code=>vm.runInContext(code,context),data:fn=>{handler=fn;},dialog:value=>{dialog=value;}};
}
async function settle(){await new Promise(resolve=>setImmediate(resolve));}
const delivery=(id,status='delivered',hours=1)=>({id,event_id:'event-'+id,status,created_at:new Date(Date.now()-hours*3600000).toISOString(),updated_at:new Date().toISOString(),url_snapshot:'https://receiver.example/orders',lifetime_attempt_count:1,cycle_number:1});

test('overview counts only its page and charts only the rolling 12h cohort',async()=>{
 const ui=await setup();ui.data(()=>({data:[delivery('a'),delivery('b','pending'),delivery('c','retry_scheduled'),delivery('d','dead_letter'),delivery('e','cancelled',24)],next_cursor:'next-page'}));
 await ui.run('load()');
 assert.equal(ui.get('#stat-total').textContent,'5');assert.equal(ui.get('#stat-delivered').textContent,'1');assert.equal(ui.get('#stat-progress').textContent,'2');assert.equal(ui.get('#stat-failed').textContent,'1');assert.equal(ui.get('#chart-total').textContent,'4');
 assert.equal(ui.get('#activity-bars').children.length,12);assert.match(ui.get('#stats-scope').textContent,/Current page/);assert.equal(ui.get('#deliveries-next').hidden,false);
});
test('live refresh retains the current cursor instead of jumping back to newest',async()=>{
 const ui=await setup();ui.data(path=>path.includes('cursor=next-page')?{data:[delivery('older','pending')],next_cursor:''}:{data:[delivery('newest')],next_cursor:'next-page'});
 await ui.run('load()');await ui.run('load(true)');await ui.run('load(false,true)');
 assert.match(ui.calls.at(-1).path,/cursor=next-page/);assert.equal(ui.get('#stat-progress').textContent,'1');assert.equal(ui.get('#deliveries-next').hidden,true);
});
test('polling pauses while hidden, inspecting, editing another view or waiting',async()=>{
 const ui=await setup();let before=ui.calls.length;
 ui.run('lastPollAt=0');ui.document.hidden=true;ui.timers[0]();await settle();assert.equal(ui.calls.length,before);
 ui.document.hidden=false;ui.dialog(true);ui.timers[0]();await settle();assert.equal(ui.calls.length,before);
 ui.dialog(false);ui.run("view='events'");ui.timers[0]();await settle();assert.equal(ui.calls.length,before);
 ui.run("view='deliveries';pending=true");ui.timers[0]();await settle();assert.equal(ui.calls.length,before);
 ui.run('pending=false;live=false');ui.timers[0]();await settle();assert.equal(ui.calls.length,before);
 ui.run('live=true');ui.timers[0]();await settle();assert.equal(ui.calls.length,before+1);
});
test('disconnect clears chart, counts and rendered tenant records and stops polling',async()=>{
 const ui=await setup();ui.data(()=>({data:[delivery('sensitive-id')],next_cursor:''}));await ui.run('load()');
 ui.run('connected(false)');const before=ui.calls.length;ui.timers[0]();await settle();
 assert.equal(ui.get('#stat-total').textContent,'—');assert.equal(ui.get('#activity-bars').children.length,0);assert.equal(ui.get('#deliveries-list').children.length,0);assert.equal(ui.calls.length,before);
});
test('unavailable foreground data shows unknown counts, not false zero',async()=>{
 const ui=await setup();ui.data(()=>{throw Error('network unavailable');});await ui.run('load()');assert.equal(ui.get('#stat-total').textContent,'—');assert.equal(ui.get('#chart-total').textContent,'—');assert.match(ui.get('#notice').textContent,/unavailable/);
});
