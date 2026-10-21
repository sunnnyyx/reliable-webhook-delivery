'use strict';
$('#github-signin').addEventListener('click',()=>busy($('#github-signin'),async()=>{
 const result=await request('/auth/github','POST',{});
 const target=new URL(result.url);
 if(target.protocol!=='https:'||!target.hostname.endsWith('.supabase.co')||target.pathname!=='/auth/v1/authorize')throw Error('Invalid sign-in destination.');
 window.location.assign(target.href);
}));
async function loadKeys(){
 const keys=await request('/account/api-key');const list=$('#keys-list');list.replaceChildren();
 for(const key of keys){const row=node('div',undefined,'row-card');row.append(node('span',key.prefix+'…','mono'),button('Revoke',async()=>{if(!confirm('Revoke this API key? Applications using it will stop authenticating.'))return;await request('/account/api-key','DELETE',{id:key.id});await loadKeys();}));list.append(row);}
 if(!keys.length)list.append(node('p','No integration keys yet.'));
}
$('#account-tools').addEventListener('click',()=>busy($('#account-tools'),async()=>{await loadKeys();$('#keys-dialog').showModal();}));
$('#keys-close').addEventListener('click',()=>$('#keys-dialog').close());
$('#keys-dialog').addEventListener('close',()=>{$('#new-api-key').textContent='';$('#new-api-key').hidden=true;$('#keys-list').replaceChildren();});
$('#key-create').addEventListener('click',()=>busy($('#key-create'),async()=>{const result=await request('/account/api-key','POST',{});$('#new-api-key').hidden=false;$('#new-api-key').textContent=result.api_key;await loadKeys();}));
(async()=>{
 try{const result=await request('/auth/config');hostedAuth=result.enabled;
 if(result.demo){
 demoMode=true;document.body.classList.add('demo-mode');$('#demo-banner').hidden=false;$('#demo-actions').hidden=false;
 $('#login-form').hidden=true;$('#demo-entry').hidden=false;$('#public-auth').hidden=true;
 $('#login h2').textContent='Start a fresh playground.';$('#login p:not(.eyebrow)').textContent='No login needed. Your temporary demo is separate from other visitors.';$('#login small').textContent='10 events per workspace. Data lives in memory and is deleted after 30 minutes or a restart.';
 document.querySelector('.workspace-local').textContent='Demo';document.querySelector('.version').textContent='RelayForge / Temporary playground';
 document.querySelector('.welcome-card h2').textContent='Webhooks, without the setup.';document.querySelector('.welcome-card p').textContent='Send a sample event. Watch a signed HTTP delivery and try a retry against the built-in receiver.';
 document.querySelector('.welcome-card .welcome-actions').hidden=true;
 $('#endpoint-form').closest('.form-panel').hidden=true;$('#logout').textContent='End demo';
 $('#subtitle').textContent='A temporary webhook playground. No account required.';
 await startDemo();
 }
 if(hostedAuth){document.querySelector('.workspace-local').textContent='Hosted';document.querySelector('.version').textContent='RelayForge / Web console';$('#public-auth').hidden=false;$('#login-form').hidden=true;$('#login h2').textContent='Your delivery workspace.';$('#login p:not(.eyebrow)').textContent='Sign in with GitHub to publish events, connect receivers, and follow every delivery.';$('#login small').textContent='GitHub handles sign-in. Browser sessions last 30 minutes and end when the service restarts.';$('#account-tools').hidden=!csrf;}}
 catch{message('Account configuration unavailable. Refresh to try again.',true);}
})();

async function startDemo(){const result=await request('/demo/session','POST',{});csrf=result.csrf;connected(true);message('Your private demo is ready. Send a sample event to see it delivered.');await load();}
$('#demo-start').addEventListener('click',()=>busy($('#demo-start'),startDemo));
async function demoSample(type){
 const result=await api('events','POST',{type,data:{order_id:'DEMO-'+Date.now(),item:'Notebook',quantity:1}},crypto.randomUUID());
 await navigate('deliveries');message(type==='demo.retry'?'Sample accepted. The receiver returns 503 once, then succeeds on retry.':'Sample accepted. Inspect its delivery for the signed HTTP acknowledgment.');
 setTimeout(()=>{if(csrf&&demoMode&&view==='deliveries'&&!pending)load(false,true);},1500);
}
$('#demo-sample').addEventListener('click',()=>busy($('#demo-sample'),()=>demoSample('order.created')));
$('#demo-retry').addEventListener('click',()=>busy($('#demo-retry'),()=>demoSample('demo.retry')));
