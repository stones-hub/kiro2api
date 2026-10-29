'use strict';
const $=id=>document.getElementById(id);
const state={accounts:[],proxies:[],keys:[],selected:new Set(),visibleIds:[],catalog:null,check:null,checkPick:new Set(),modelRemove:null,metricsKey:'',keyEdit:null,keyDelete:null,planOrder:[],orderDraft:[],orderCounts:{},proxyEdit:null,proxyDelete:null,tests:{},filter:'ready',csrf:'',loading:false,edit:null,delete:null,mode:'single',range:'hours',metrics:null};
const fmt=new Intl.NumberFormat('zh-CN',{maximumFractionDigits:2});
const integer=new Intl.NumberFormat('zh-CN');
const time=value=>value?new Date(value).toLocaleString('zh-CN',{hour12:false,timeZone:'Asia/Shanghai'}):'—';
const statusLabels={ready:'可用',busy:'并发已满',checking:'检查中',cooldown:'冷却中',error:'需检查',exhausted:'额度耗尽',disabled:'已停用',deleted:'已删除'};
const shortTime=value=>value?new Date(value).toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false,timeZone:'Asia/Shanghai'}):'—';
function until(value){if(!value)return '官方未返回';const h=(new Date(value)-Date.now())/36e5;if(h<=0)return '已到重置时间';if(h<1)return Math.max(1,Math.round(h*60))+' 分钟后';return h<48?Math.round(h)+' 小时后':Math.round(h/24)+' 天后';}
let toastTimer;
function toast(message){$('toast').textContent=message;$('toast').hidden=false;clearTimeout(toastTimer);toastTimer=setTimeout(()=>{$('toast').hidden=true;},4500);}
function errorAt(id,message){$(id).textContent=message||'';$(id).hidden=!message;}
function el(tag,className,text){const node=document.createElement(tag);if(className)node.className=className;if(text!==undefined)node.textContent=text;return node;}
function number(value){return value===undefined||value===null?'—':fmt.format(value);}
function button(text,action,className=''){const b=el('button',className,text);b.type='button';b.addEventListener('click',action);return b;}
async function request(path,options={}){
  const headers={Accept:'application/json',...options.headers};
  if(options.method&&options.method!=='GET'){headers['Content-Type']='application/json';headers['X-CSRF-Token']=state.csrf;}
  const r=await fetch(new URL('api/'+path,document.baseURI),{credentials:'same-origin',cache:'no-store',...options,headers});
  let data;try{data=await r.json();}catch{throw new Error(r.status===401?'管理登录已失效，请刷新页面重新登录':'服务暂时不可用，请稍后重试');}
  if(!r.ok)throw new Error(data.error||'操作失败');return data;
}
async function reload(){
  if(state.loading)return;state.loading=true;
  try{
    const scope=state.metricsKey;
    const [data,metrics,keys,catalog]=await Promise.all([request('accounts'),request('metrics'+(scope?'?key='+encodeURIComponent(scope):'')).catch(()=>null),request('keys').catch(()=>null),request('models').catch(()=>null)]);
    if(keys)state.keys=keys.keys;if(catalog)state.catalog=catalog;
    // A key whose usage expired disappears; fall back to the whole pool on the next sync.
    if(scope&&!state.keys.some(k=>k.id===scope))state.metricsKey='';
    state.accounts=data.accounts;state.proxies=data.proxies||[];state.planOrder=data.plan_order||[];if(metrics&&scope===state.metricsKey)state.metrics=metrics;
    errorAt('global-error',data.storage_error?'账号数据暂时无法保存，请检查服务器磁盘。已保存的配置仍有效。':'');
    render(data);$('connection-status').replaceChildren(el('i'),document.createTextNode('运行状态已同步'));
  }catch(e){errorAt('global-error',e.message);$('connection-status').textContent='连接中断';}
  finally{state.loading=false;}
}
function render(data){
  const live=state.accounts.filter(a=>!a.deleted_at);
  const ready=live.filter(a=>a.enabled&&['ready','busy'].includes(a.status));
  const hasUsage=ready.filter(a=>a.usage);
  const todayCredits=state.metrics?.days?.at(-1)?.credits;
  const today=live.reduce((s,a)=>s+(a.today.requests||0),0);
  const failed=live.reduce((s,a)=>s+(a.today.failed||0),0);
  $('side-count').textContent=live.length;$('ready-count').textContent=ready.length;$('total-count').textContent='/ '+live.length;
  $('account-caption').textContent=`${live.filter(a=>!a.enabled).length} 个已停用 · ${live.filter(a=>a.enabled&&!['ready','busy'].includes(a.status)).length} 个需关注`;
  $('remaining-credits').textContent=hasUsage.length?number(hasUsage.reduce((n,a)=>n+Math.max(a.usage.remaining,0),0)):'—';
  $('credits-caption').textContent=`仅统计可用账号 · 已获取 ${hasUsage.length} / ${ready.length} 个的官方额度`;
  $('today-requests').textContent=integer.format(today);$('today-credits').textContent=state.metrics?'消耗 '+number(todayCredits||0)+' 积分':'';$('requests-caption').textContent=`${integer.format(failed)} 次失败 · 北京时间今日`;
  $('active-count').textContent=state.accounts.reduce((s,a)=>s+a.active,0);$('waiting-caption').textContent=`等待中的请求 ${data.waiting}`;
  $('updated-at').textContent='最后同步 '+time(data.updated_at);$('stats-since').textContent='本地统计始于 '+time(data.stats_since);
  renderRows();renderKeyFilter();renderMetrics();renderProxies();renderKeys();renderCatalog();
}
function renderRows(){
  const query=$('search').value.trim().toLowerCase();
  const filtered=state.accounts.filter(a=>{
    if(state.filter==='deleted')return !!a.deleted_at&&(!query||(a.name+' '+a.email).toLowerCase().includes(query));
    if(a.deleted_at)return false;
    if(query&&!(a.name+' '+a.email).toLowerCase().includes(query))return false;
    if(state.filter==='ready')return ['ready','busy'].includes(a.status);
    if(state.filter==='attention')return a.enabled&&!['ready','busy'].includes(a.status);
    if(state.filter==='disabled')return !a.enabled;
    return true;
  });
  // Same order the server allocates in; Array.prototype.sort is stable, so pool order holds within a type.
  if(state.planOrder.length)filtered.sort((x,y)=>planRank(x)-planRank(y));
  $('order-caption').textContent=state.planOrder.length?`优先分配：${state.planOrder.map(planLabel).join(' → ')}，其余账号随后。达到并发上限后使用下一个可用账号。`:'按账号顺序分配，达到并发上限后使用下一个可用账号。';
  // Drop selections that were deleted elsewhere; the select-all box covers what is listed.
  const live=new Set(state.accounts.filter(a=>!a.deleted_at).map(a=>a.id));
  for(const id of state.selected)if(!live.has(id))state.selected.delete(id);
  state.visibleIds=filtered.filter(a=>!a.deleted_at).map(a=>a.id);
  $('visible-count').textContent=filtered.length;$('account-rows').replaceChildren();
  $('empty-state').hidden=!!filtered.length;
  if(!filtered.length){$('empty-state').querySelector('h3').textContent=state.accounts.length?'没有符合条件的账号':'这里还没有账号';$('empty-state').querySelector('p').textContent=state.accounts.length?'调整筛选条件，或添加一个新账号。':'添加一个 Kiro 账号，开始管理额度与请求。';}
  for(const a of filtered){
    const row=el('tr');const pick=el('td');
    if(!a.deleted_at){const input=el('input');input.type='checkbox';input.checked=state.selected.has(a.id);input.setAttribute('aria-label','选择 '+(a.email||a.name));
      input.addEventListener('change',()=>{if(input.checked)state.selected.add(a.id);else state.selected.delete(a.id);renderBatch();});pick.append(input);}
    row.append(pick);
    const identity=el('td');const wrap=el('div','account-identity');
    wrap.append(el('div','avatar',(a.email||a.name||'K').slice(0,2).toUpperCase()));
    const name=el('div');const titleRow=el('div','identity-title');const title=el('div','identity-name',a.name);title.title=a.name;
    const plan=el('span',planClass(a.plan_type),planLabel(a.plan_type));plan.title=a.plan_type?'账号类型（官方订阅）：'+(a.usage?.plan||a.plan_type):'首次查询官方额度后显示账号类型';
    titleRow.append(title,plan);name.append(titleRow,el('div','identity-email',a.email||'验证后显示邮箱'));
    if(a.proxy_id){const tag=el('div','proxy-tag','⇄ '+proxyLabel(a.proxy_id));tag.title='经由 SOCKS5 代理访问 Kiro';name.append(tag);}wrap.append(name);identity.append(wrap);row.append(identity);
    const usage=el('td');const value=el('div','credit-value',number(a.usage?.remaining));
    value.append(el('span','credit-total','/ '+number(a.usage?.limit)));usage.append(value);
    const progress=el('progress','credit-meter'+(a.usage&&a.usage.remaining<.1*a.usage.limit?' low':''));progress.max=100;progress.value=a.usage?.limit?Math.max(0,a.usage.remaining/a.usage.limit*100):0;
    progress.setAttribute('aria-label','剩余积分百分比');usage.append(progress);usage.title='上次官方查询：'+time(a.usage?.checked_at);row.append(usage);
    const reset=el('td');reset.append(el('div','credit-value',shortTime(a.usage?.reset_at)),el('div','subtext',until(a.usage?.reset_at)));row.append(reset);
    const status=el('td');const badge=el('span','state '+a.status);badge.append(el('i'),document.createTextNode(statusLabels[a.status]||a.status));status.append(badge);
    if(a.refreshing)status.append(el('div','subtext','正在更新额度'));
    if(a.disabled_reason&&!a.enabled){const why=el('div','subtext',a.disabled_reason);why.title=a.disabled_reason;status.append(why);}
    if(a.error){const reason=el('div','subtext',a.error);reason.title=a.error;status.append(reason);}row.append(status);
    const concurrency=el('td');const concurrent=el('div','concurrency');concurrent.append(el('strong','',String(a.active)),document.createTextNode('/ '+a.concurrency));
    if(!a.deleted_at){const edit=button('↗',()=>openEdit(a),'limit-link');edit.setAttribute('aria-label','修改 '+a.name+' 的并发上限');concurrent.append(edit);}concurrency.append(concurrent);row.append(concurrency);
    const requests=el('td');requests.append(el('div','credit-value',integer.format(a.today.requests)),el('div','subtext',a.today.failed?`${a.today.failed} 次失败`:'累计 '+integer.format(a.totals.requests)));row.append(requests);
    const actions=el('td');const group=el('div','row-actions');group.append(button('用量',()=>openDetail(a)));
    if(!a.deleted_at){group.append(button('设置',()=>openEdit(a)),button(a.enabled?'停用':'启用',()=>toggleAccount(a)),button('删除',()=>openDelete(a),'delete'));}else group.append(button('还原',()=>openRestore(a)));
    actions.append(group);row.append(actions);$('account-rows').append(row);
  }
  renderBatch();
}
// ---- Batch edit: selections survive the 15 s refresh and filter changes until cleared.
function selectedAccounts(){return state.accounts.filter(a=>!a.deleted_at&&state.selected.has(a.id));}
function renderBatch(){
  const n=state.selected.size;$('batch-bar').hidden=!n;$('batch-count').textContent=`已选 ${n} 个账号`;
  const all=$('select-all'),vis=state.visibleIds||[],on=vis.filter(id=>state.selected.has(id)).length;
  all.disabled=!vis.length;all.checked=!!vis.length&&on===vis.length;all.indeterminate=on>0&&on<vis.length;
}
$('select-all').addEventListener('change',e=>{for(const id of state.visibleIds||[]){if(e.target.checked)state.selected.add(id);else state.selected.delete(id);}renderRows();});
$('batch-clear').addEventListener('click',()=>{state.selected.clear();renderRows();});
async function runBatch(body,message){
  const r=await request('accounts/batch',{method:'POST',body:JSON.stringify({ids:[...state.selected],...body})});
  toast(message.replace('{n}',r.changed));await reload();return r;
}
async function quickBatch(action,message,b){b.disabled=true;try{await runBatch({action},message);}catch(e){errorAt('global-error',e.message);toast(e.message);await reload();}finally{b.disabled=false;}}
$('batch-enable').addEventListener('click',e=>quickBatch('enable','已启用 {n} 个账号',e.currentTarget));
$('batch-disable').addEventListener('click',e=>quickBatch('disable','已停用 {n} 个账号，进行中的请求继续完成',e.currentTarget));
$('batch-edit').addEventListener('click',()=>{
  const picked=selectedAccounts();$('batch-form').reset();errorAt('batch-error','');
  $('batch-intro').textContent=`将修改 ${picked.length} 个账号。未勾选或选择「不修改」的项目保持各账号原值。`;
  const select=$('batch-proxy');select.replaceChildren(new Option('不修改',' keep'),new Option('直连（不使用代理）',''));
  for(const x of state.proxies)select.append(new Option(proxyLabel(x.id)+' · '+proxyAddress(x),x.id));select.value=' keep';
  const limits=[...new Set(picked.map(a=>a.concurrency))];$('batch-limit').value=limits.length===1?limits[0]:2;$('batch-limit').disabled=true;
  $('batch-dialog').showModal();
});
$('batch-set-limit').addEventListener('change',e=>{$('batch-limit').disabled=!e.target.checked;if(e.target.checked)$('batch-limit').focus();});
$('batch-form').addEventListener('submit',async event=>{
  event.preventDefault();errorAt('batch-error','');const b=$('batch-save');
  const body={action:'update'};
  if($('batch-set-limit').checked)body.concurrency=Number($('batch-limit').value);
  if($('batch-proxy').value!==' keep')body.proxy_id=$('batch-proxy').value;
  if($('batch-enabled').value)body.enabled=$('batch-enabled').value==='true';
  if(Object.keys(body).length===1){errorAt('batch-error','请至少选择一项要修改的设置');return;}
  b.disabled=true;
  try{await runBatch(body,'已更新 {n} 个账号的设置');$('batch-dialog').close();}catch(e){errorAt('batch-error',e.message);}finally{b.disabled=false;}
});
$('batch-delete').addEventListener('click',()=>{
  const picked=selectedAccounts();$('batch-delete-title').textContent=`删除所选的 ${picked.length} 个账号？`;
  const names=picked.map(a=>a.email||a.name);$('batch-delete-names').textContent=names.slice(0,5).join('、')+(names.length>5?` 等 ${names.length} 个`:'');$('batch-delete-names').title=names.join('\n');
  errorAt('batch-delete-error','');$('batch-delete-dialog').showModal();
});
$('batch-delete-form').addEventListener('submit',async event=>{event.preventDefault();const b=$('batch-delete-submit');b.disabled=true;
  try{await runBatch({action:'delete'},'已删除 {n} 个账号，现有请求继续完成');state.selected.clear();$('batch-delete-dialog').close();renderRows();}catch(e){errorAt('batch-delete-error',e.message);}finally{b.disabled=false;}});
// ---- Account types come from the official subscription title (plan_type) and decide allocation order.
const planLabels={FREE:'免费版',STUDENT:'学生版',PRO:'Pro','PRO+':'Pro+',POWER:'Power'};
const planLabel=t=>t?(planLabels[t]||t):'待查询';
const planClass=t=>'plan-tag plan-'+(t||'unknown').toLowerCase().replace(/\+/g,'-plus').replace(/[^a-z0-9-]/g,'');
function planRank(a){const i=state.planOrder.indexOf(a.plan_type);return i<0?state.planOrder.length:i;}
function openOrder(){
  const counts={};for(const a of state.accounts)if(!a.deleted_at&&a.plan_type)counts[a.plan_type]=(counts[a.plan_type]||0)+1;
  const rest=Object.keys(counts).filter(t=>!state.planOrder.includes(t)).sort((x,y)=>counts[y]-counts[x]);
  state.orderCounts=counts;state.orderDraft=[...state.planOrder.map(type=>({type,on:true})),...rest.map(type=>({type,on:false}))];
  errorAt('order-error','');renderOrder();$('order-dialog').showModal();
}
function renderOrder(focus){
  const box=$('order-list'),d=state.orderDraft;box.replaceChildren();
  if(!d.length){box.append(el('p','chart-empty','还没有查询到账号的订阅类型'));return;}
  // Checked types stay on top, so the list always reads as the saved order.
  d.sort((x,y)=>y.on-x.on);
  let n=0,target;
  d.forEach((item,i)=>{
    const row=el('div','order-item'+(item.on?'':' off'));row.setAttribute('role','listitem');
    const label=el('label','order-check');const input=el('input');input.type='checkbox';input.checked=item.on;
    input.addEventListener('change',()=>{item.on=input.checked;renderOrder({type:item.type,part:'check'});});
    label.append(input,el('span','order-rank',item.on?String(++n):'—'),el('span',planClass(item.type),planLabel(item.type)),el('span','order-count',(state.orderCounts[item.type]||0)+' 个账号'));
    const move=(j,part)=>{[d[i],d[j]]=[d[j],d[i]];renderOrder({type:item.type,part});};
    const up=button('↑',()=>move(i-1,'up'));up.disabled=!item.on||i===0;up.setAttribute('aria-label','上移 '+planLabel(item.type));
    const down=button('↓',()=>move(i+1,'down'));down.disabled=!item.on||!d[i+1]?.on;down.setAttribute('aria-label','下移 '+planLabel(item.type));
    const moves=el('span','order-moves');moves.append(up,down);row.append(label,moves);box.append(row);
    if(focus?.type===item.type)target={check:input,up,down}[focus.part];
    if(target&&target.disabled)target=input;
  });
  target?.focus();
}
async function saveOrder(order,message){
  errorAt('order-error','');for(const b of [$('order-save'),$('order-reset')])b.disabled=true;
  try{await request('plan-order',{method:'PUT',body:JSON.stringify({order})});$('order-dialog').close();toast(message);await reload();}
  catch(e){errorAt('order-error',e.message);}finally{for(const b of [$('order-save'),$('order-reset')])b.disabled=false;}
}
$('edit-order').addEventListener('click',openOrder);
$('order-form').addEventListener('submit',event=>{event.preventDefault();const order=state.orderDraft.filter(x=>x.on).map(x=>x.type);saveOrder(order,order.length?'分配顺序已生效：'+order.map(planLabel).join(' → '):'已恢复按账号池顺序分配');});
$('order-reset').addEventListener('click',()=>saveOrder([],'已恢复按账号池顺序分配'));
async function toggleAccount(a){try{await request('accounts/'+encodeURIComponent(a.id),{method:'PATCH',body:JSON.stringify({version:a.version,enabled:!a.enabled})});toast(a.enabled?'已停止向此账号分配新请求':'账号已启用');await reload();}catch(e){errorAt('global-error',e.message);await reload();toast(e.message);}}
function openEdit(a){state.edit={id:a.id,version:a.version,proxy:a.proxy_id||''};const select=$('edit-proxy');select.replaceChildren(new Option('直连（不使用代理）',''));for(const x of state.proxies)select.append(new Option(proxyLabel(x.id)+' · '+proxyAddress(x),x.id));select.value=a.proxy_id||'';$('edit-name').value=a.name;$('edit-email').textContent=a.email||a.name;$('edit-limit').value=a.concurrency;$('edit-enabled').checked=a.enabled;$('edit-token').value='';errorAt('edit-error','');$('edit-dialog').querySelector('details').open=false;$('edit-dialog').showModal();}
function openRestore(a){state.restore={id:a.id,version:a.version};$('restore-form').reset();$('restore-name').textContent=a.email||a.name;errorAt('restore-error','');$('restore-dialog').querySelector('details').open=false;$('restore-dialog').showModal();}
function openDelete(a){state.delete={id:a.id,version:a.version};$('delete-name').textContent=a.email||a.name;errorAt('delete-error','');$('delete-dialog').showModal();}
function openDetail(a){
  $('detail-title').textContent=a.name;const content=$('detail-content');content.replaceChildren(el('p','detail-subtitle',a.email||'邮箱尚未验证'));
  const grid=el('div','detail-grid');for(const [label,value]of [['剩余积分',number(a.usage?.remaining)],['已用积分',number(a.usage?.used)],['额度总量',number(a.usage?.limit)]]){const box=el('div','detail-box');box.append(el('span','',label),el('strong','',value));grid.append(box);}content.append(grid);
  const table=el('table','detail-table');const head=el('tr');for(const s of ['本地请求统计','今日','累计'])head.append(el('th','',s));const thead=el('thead');thead.append(head);table.append(thead);const body=el('tbody');
  for(const[label,k]of [['完成请求','requests'],['成功','succeeded'],['失败','failed'],['已取消','canceled'],['输入 token（含缓存）','input_tokens'],['缓存读取 token','cache_read_tokens'],['输出 token','output_tokens']]){const tr=el('tr');tr.append(el('td','',label),el('td','',integer.format(a.today[k])),el('td','',integer.format(a.totals[k])));body.append(tr);}table.append(body);content.append(table);
  const meta=el('dl','detail-meta');for(const[label,value]of [['订阅',a.usage?.plan||'—'],['官方用量更新',time(a.usage?.checked_at)],['额度重置',time(a.usage?.reset_at)],['访问令牌有效期',time(a.token_expires_at)],['最近请求',time(a.last_used)],['最近模型',a.last_model||'—'],['实时并发',a.active+' / '+a.concurrency],['出口代理',a.proxy_id?proxyLabel(a.proxy_id):'直连'],['账号状态',statusLabels[a.status]],['停用原因',a.disabled_reason||'—'],['最近异常',a.error||'—']]){meta.append(el('dt','',label),el('dd','',value));}content.append(meta);
  if(!a.deleted_at){const refresh=button('刷新此账号额度',async()=>{refresh.disabled=true;try{await request('accounts/'+encodeURIComponent(a.id)+'/refresh',{method:'POST',body:'{}'});toast('已安排额度更新，请稍后重新查看');$('detail-dialog').close();await reload();}catch(e){toast(e.message);}finally{refresh.disabled=false;}},'button secondary');content.append(refresh);}
  $('detail-dialog').showModal();
}
function setImportMode(mode){state.mode=mode;$('single-fields').hidden=mode!=='single';$('json-fields').hidden=mode!=='json';$('mode-single').classList.toggle('active',mode==='single');$('mode-json').classList.toggle('active',mode==='json');errorAt('add-error','');}
$('mode-single').addEventListener('click',()=>setImportMode('single'));$('mode-json').addEventListener('click',()=>setImportMode('json'));
$('add-account').addEventListener('click',()=>{$('add-form').reset();errorAt('add-error','');setImportMode('single');$('add-dialog').showModal();});
for(const b of document.querySelectorAll('[data-close]'))b.addEventListener('click',()=>$(b.dataset.close).close());
for(const d of document.querySelectorAll('dialog'))d.addEventListener('close',()=>{if(d.id==='add-dialog'){$('add-token').value='';$('add-json').value='';$('json-file').value='';}if(d.id==='edit-dialog')$('edit-token').value='';if(d.id==='restore-dialog')$('restore-form').reset();});
$('json-file').addEventListener('change',async event=>{const file=event.target.files[0];if(!file)return;if(file.size>1024*1024){errorAt('add-error','文件不能超过 1 MiB');return;}$('add-json').value=await file.text();});
$('add-form').addEventListener('submit',async event=>{
  event.preventDefault();errorAt('add-error','');const submit=$('save-add');submit.disabled=true;
  try{
    const concurrency=Number($('add-limit').value);let items;
    if(state.mode==='json'){try{items=JSON.parse($('add-json').value.replace(/\\([_@])/g,'$1'));}catch{throw new Error('JSON 格式不正确，请粘贴一个账号对象或数组');}if(!Array.isArray(items))items=[items];}
    else items=[{email:$('add-email').value,name:$('add-name').value,refreshToken:$('add-token').value,provider:'Github'}];
    if(!items.length||items.length>100)throw new Error('每次支持 1–100 个账号');
    items=items.map(a=>{if(!a||typeof a!=='object'||Array.isArray(a))throw new Error('每个账号必须是 JSON 对象');return {...a,concurrency:a.concurrency??concurrency};});
    const r=await request('accounts',{method:'POST',body:JSON.stringify({accounts:items})});$('add-dialog').close();toast(`已添加 ${r.added} 个账号${r.duplicates?'，跳过 '+r.duplicates+' 个重复账号':''}`);await reload();
  }catch(e){errorAt('add-error',e.message);}finally{submit.disabled=false;}
});
$('edit-form').addEventListener('submit',async event=>{event.preventDefault();errorAt('edit-error','');const b=event.submitter;b.disabled=true;try{const patch={version:state.edit.version,name:$('edit-name').value,concurrency:Number($('edit-limit').value),enabled:$('edit-enabled').checked};if($('edit-token').value.trim())patch.refreshToken=$('edit-token').value;if($('edit-proxy').value!==state.edit.proxy)patch.proxy_id=$('edit-proxy').value;await request('accounts/'+encodeURIComponent(state.edit.id),{method:'PATCH',body:JSON.stringify(patch)});$('edit-dialog').close();toast('设置已生效');await reload();}catch(e){errorAt('edit-error',e.message);}finally{b.disabled=false;}});
$('restore-form').addEventListener('submit',async event=>{event.preventDefault();errorAt('restore-error','');const b=$('restore-submit');b.disabled=true;try{const body={version:state.restore.version,refreshToken:$('restore-token').value};const id=$('restore-client-id').value.trim(),secret=$('restore-client-secret').value.trim();if(id||secret){body.clientId=id;body.clientSecret=secret;body.provider='BuilderId';}else body.provider='Github';await request('accounts/'+encodeURIComponent(state.restore.id)+'/restore',{method:'POST',body:JSON.stringify(body)});$('restore-dialog').close();toast('账号已还原，正在检查额度');await reload();}catch(e){errorAt('restore-error',e.message);}finally{b.disabled=false;}});
$('delete-form').addEventListener('submit',async event=>{event.preventDefault();const b=event.submitter;b.disabled=true;try{await request('accounts/'+encodeURIComponent(state.delete.id),{method:'DELETE',body:JSON.stringify({version:state.delete.version})});$('delete-dialog').close();toast('账号已删除，现有请求继续完成');await reload();}catch(e){errorAt('delete-error',e.message);}finally{b.disabled=false;}});
for(const b of document.querySelectorAll('[data-filter]'))b.addEventListener('click',()=>{state.filter=b.dataset.filter;document.querySelectorAll('[data-filter]').forEach(x=>x.classList.toggle('active',x===b));renderRows();});
$('search').addEventListener('input',renderRows);
$('refresh-usage').addEventListener('click',async()=>{const b=$('refresh-usage');b.disabled=true;try{const r=await request('refresh',{method:'POST',body:'{}'});toast(r.scheduled?`正在更新 ${r.scheduled} 个账号的官方额度`:'额度已是最新或正在更新');await reload();}catch(e){toast(e.message);}finally{b.disabled=false;}});
async function init(){try{const config=await request('status');state.csrf=config.csrf_token;$('cache-policy').textContent=config.cache_enabled?`命中重复前缀后，按总输入 ${config.cache_percent||'40'}% 计入缓存读取`:'未启用';await reload();}catch(e){errorAt('global-error',e.message);}}
init();setInterval(()=>{if(!document.hidden)reload();},15000);document.addEventListener('visibilitychange',()=>{if(!document.hidden)reload();});
// ---- Overview charts: inline SVG only, so the page keeps its strict CSP and has no dependencies.
const SVG_NS='http://www.w3.org/2000/svg';
const palette={ok:'#3f8f6b',failed:'#d0826a',canceled:'#bcc6c0',rejected:'#c9a24a',fb:'#7297b7',dur:'#245e4d',active:'#3f8f6b',waiting:'#c9a24a',capacity:'#9aa8a1',credits:'#86b59c'};
const errorLabels={upstream_429:'上游限流 429',upstream_402:'上游额度不足 402',upstream_auth:'上游认证失败 401/403',upstream_5xx:'上游服务错误 5xx',upstream_4xx:'上游拒绝请求 4xx',stream:'上游流中断或异常',network:'网络或内部错误',request:'请求无效',canceled:'客户端取消',empty:'空输出（无文本/工具）',pool_busy:'排队超时或队列已满',pool_unavailable:'没有可用账号',proxy:'账号代理连接失败',other:'其他'};
function svgEl(tag,attrs={},text){const n=document.createElementNS(SVG_NS,tag);for(const k in attrs)n.setAttribute(k,attrs[k]);if(text!==undefined)n.textContent=text;return n;}
function niceMax(v){if(!(v>0))return 1;const e=10**Math.floor(Math.log10(v)),f=v/e;return (f<=1?1:f<=2?2:f<=5?5:10)*e;}
const compact=v=>v>=1e4?fmt.format(Math.round(v/1e3))+'k':fmt.format(Math.round(v*10)/10);
const ms=v=>!v?'—':v>=1000?(v/1000).toFixed(v>=1e4?0:1)+' s':Math.round(v)+' ms';
const msTick=v=>v>=1000?(v/1000).toFixed(v>=1e4?0:1)+'s':Math.round(v)+'ms';
const percent=(a,b)=>b?(a/b*100).toFixed(a===b?0:1)+'%':'—';
function rel(value){const h=(new Date(value)-Date.now())/36e5;if(h<1)return '约 '+Math.max(1,Math.round(h*60))+' 分钟后';return h<48?'约 '+Math.round(h)+' 小时后':'约 '+(h/24).toFixed(1)+' 天后';}
function legend(id,items){const box=$(id);box.replaceChildren();for(const it of items){const item=el('span','legend-item');const sw=svgEl('svg',{width:14,height:8,'aria-hidden':'true'});sw.append(svgEl('line',{x1:0,x2:14,y1:4,y2:4,stroke:it.color,'stroke-width':it.bar?8:2,'stroke-dasharray':it.dash||''}));item.append(sw,document.createTextNode(it.label));box.append(item);}}
function frame(box,max,fmtY,labels,aria){
  const W=Math.max(box.clientWidth||0,280),H=176,L=46,R=8,T=10,B=22,pw=W-L-R,ph=H-T-B,n=Math.max(labels.length,1);
  const svg=svgEl('svg',{viewBox:`0 0 ${W} ${H}`,width:W,height:H,role:'img','aria-label':aria});
  for(let i=0;i<=4;i++){const y=T+ph-ph*i/4;svg.append(svgEl('line',{x1:L,x2:W-R,y1:y,y2:y,class:i?'grid':'axis'}),svgEl('text',{x:L-7,y:y+3,class:'tick','text-anchor':'end'},fmtY(max*i/4)));}
  const step=Math.max(1,Math.ceil(n/(W<420?4:7)));
  for(let i=0;i<labels.length;i+=step)if(i<step||labels[i]!==labels[i-step])svg.append(svgEl('text',{x:L+pw*(i+.5)/n,y:H-5,class:'tick','text-anchor':'middle'},labels[i]));
  return {svg,T,pw,ph,n,slot:pw/n,x:i=>L+pw*(i+.5)/n,y:v=>T+ph-ph*Math.min(Math.max(v,0)/max,1),empty(){svg.append(svgEl('text',{x:L+pw/2,y:T+ph/2,class:'chart-empty-text','text-anchor':'middle'},'暂无数据'));}};
}
function hoverTarget(f,i,title){const r=svgEl('rect',{x:f.x(i)-f.slot/2,y:f.T,width:f.slot,height:f.ph,class:'hover'});r.append(svgEl('title',{},title));return r;}
function stackedBars(box,pts,keys,labels,titles,fmtY,aria){
  const peak=Math.max(0,...pts.map(p=>keys.reduce((a,k)=>a+(p[k.key]||0),0)));
  const f=frame(box,niceMax(peak),fmtY,labels,aria),bw=Math.max(1,Math.min(f.slot*.68,22));
  pts.forEach((p,i)=>{let acc=0;const g=svgEl('g');for(const k of keys){const v=p[k.key]||0;if(!v)continue;const top=f.y(acc+v),bottom=f.y(acc);g.append(svgEl('rect',{x:f.x(i)-bw/2,y:top,width:bw,height:Math.max(bottom-top,1),fill:k.color,class:'bar'}));acc+=v;}g.append(hoverTarget(f,i,titles(p)));f.svg.append(g);});
  if(!peak)f.empty();box.replaceChildren(f.svg);
}
function lineChart(box,pts,keys,labels,titles,fmtY,aria){
  const peak=Math.max(0,...pts.flatMap(p=>keys.map(k=>p[k.key]||0)));
  const f=frame(box,niceMax(peak),fmtY,labels,aria),dots=pts.length<=62;
  for(const k of keys){
    let d='',pen=false;
    pts.forEach((p,i)=>{const v=p[k.key];if(k.gaps&&!v){pen=false;return;}d+=(pen?'L':'M')+f.x(i).toFixed(1)+' '+f.y(v||0).toFixed(1);pen=true;
      if(dots&&k.gaps)f.svg.append(svgEl('circle',{cx:f.x(i),cy:f.y(v),r:2.2,fill:k.color,class:'dot'}));});
    if(d)f.svg.append(svgEl('path',{d,fill:'none',stroke:k.color,'stroke-width':k.width||1.8,'stroke-dasharray':k.dash||'','stroke-linejoin':'round','stroke-linecap':'round',class:'line'}));
  }
  // Transparent hover slots go on top so every point shows its exact values.
  if(dots)pts.forEach((p,i)=>f.svg.append(hoverTarget(f,i,titles(p))));
  else if(pts.length){const step=Math.ceil(pts.length/60);for(let i=0;i<pts.length;i+=step){const r=hoverTarget(f,i,titles(pts[Math.min(i+step-1,pts.length-1)]));r.setAttribute('width',f.slot*step);f.svg.append(r);}}
  if(!peak)f.empty();box.replaceChildren(f.svg);
}
function barList(box,items,total,labelOf,color){
  box.replaceChildren();if(!items.length){box.append(el('p','chart-empty','暂无数据'));return;}
  const top=items[0].count;
  for(const it of items){const row=el('div','bar-row');const name=el('span','bar-name',labelOf(it.name));name.title=it.name;
    const track=el('span','bar-track');const fill=el('span','bar-fill');fill.style.width=Math.max(2,it.count/top*100)+'%';fill.style.background=color;track.append(fill);
    row.append(name,track,el('span','bar-value',integer.format(it.count)+(total?' · '+percent(it.count,total):'')));box.append(row);}
}
function renderForecast(f){
  const box=$('forecast');box.replaceChildren();
  const head=el('div','forecast-head');let big='—',sub='',warn=false;
  if(!f.accounts){big='暂无额度数据';sub='等待官方额度查询完成';}
  else if(f.exhaust_at){const beforeReset=!!f.next_reset_at&&new Date(f.exhaust_at)<new Date(f.next_reset_at);warn=beforeReset||new Date(f.exhaust_at)-Date.now()<864e5;big=rel(f.exhaust_at)+'耗尽';sub=shortTime(f.exhaust_at)+(beforeReset||!f.next_reset_at?'':' · 已计入重置恢复')+(beforeReset?' · 早于下次重置':'');}
  else if(f.window_hours<1){big='正在积累数据';sub='需要至少 1 小时的额度变化记录';}
  else{big='近期没有消耗';sub='按当前速度不会耗尽';}
  head.append(el('strong','forecast-big'+(warn?' warn':''),big),el('span','forecast-sub',sub));box.append(head);
  const meta=el('dl','forecast-meta');
  for(const[label,value]of [['剩余积分',f.accounts?number(f.remaining)+' / '+number(f.limit):'—'],['消耗速度',f.burn_per_hour?number(f.burn_per_hour)+' / 小时（近 '+fmt.format(Math.round(f.window_hours*10)/10)+' 小时）':'—'],['下次重置',f.next_reset_at?shortTime(f.next_reset_at)+' · 约恢复 '+number(f.next_reset_credits):'官方未返回'],['计入账号',f.accounts+' 个已启用']])meta.append(el('dt','',label),el('dd','',value));
  box.append(meta,el('p','chart-note','官方额度约每 5 分钟更新一次，这里是估算值；重置按恢复已用额度计算。'));
}
function renderMetrics(){
  // Hidden charts would size to zero; showView re-renders when the overview comes back.
  const m=state.metrics;if(!m||document.querySelector('.overview').hidden)return;
  const scoped=keyById(m.key_id);
  errorAt('key-scope',scoped?`当前只显示「${scoped.name}」的请求量、延迟、错误和模型。积分消耗、耗尽预测和实时并发统计的是整个账号池，不按 Key 区分。`:'');
  const days=state.range==='days',pts=days?m.days:m.hours,span=days?'近 31 天':'近 24 小时';
  const labels=pts.map(p=>days?p.key.slice(5):p.key.slice(11)+':00');
  const when=p=>days?p.key:p.key.slice(5,10)+' '+p.key.slice(11)+':00';
  const sum=k=>pts.reduce((a,p)=>a+(p[k]||0),0);
  const total=sum('requests'),ok=sum('succeeded'),canceled=sum('canceled');
  const tokensIn=sum('input_tokens'),tokensOut=sum('output_tokens');
  $('trend-caption').textContent=`${span} ${integer.format(total)} 次 · 成功率 ${percent(ok,total-canceled)}`+(tokensIn||tokensOut?` · 输入 ${compact(tokensIn)} / 输出 ${compact(tokensOut)} tokens`:'');
  const trendKeys=[{key:'succeeded',label:'成功',color:palette.ok},{key:'failed',label:'失败',color:palette.failed},{key:'canceled',label:'取消',color:palette.canceled},{key:'rejected',label:'被拒（排队/无账号）',color:palette.rejected}];
  legend('trend-legend',trendKeys.map(k=>({...k,bar:true})));
  stackedBars($('trend-chart'),pts,trendKeys,labels,p=>`${when(p)}\n请求 ${p.requests} · 成功 ${p.succeeded} · 失败 ${p.failed} · 取消 ${p.canceled}${p.rejected?' · 被拒 '+p.rejected:''}${p.empty?' · 空输出 '+p.empty:''}`,compact,span+'请求量趋势');

  const l=m.latency_24h,stats=$('latency-stats');stats.replaceChildren();
  for(const[label,value]of [['首字 P50',ms(l.first_byte_p50_ms)],['首字 P95',ms(l.first_byte_p95_ms)],['总耗时 P50',ms(l.duration_p50_ms)],['总耗时 P95',ms(l.duration_p95_ms)]]){const b=el('div','stat');b.append(el('span','',label),el('strong','',value));stats.append(b);}
  stats.title=`近 24 小时 ${l.samples} 个成功请求，其中 ${l.first_byte_samples} 个有首字时间`;
  const latKeys=[{key:'first_byte_p50_ms',label:'首字 P50',color:palette.fb,gaps:true},{key:'first_byte_p95_ms',label:'首字 P95',color:palette.fb,dash:'4 3',gaps:true},{key:'duration_p50_ms',label:'总耗时 P50',color:palette.dur,gaps:true},{key:'duration_p95_ms',label:'总耗时 P95',color:palette.dur,dash:'4 3',gaps:true}];
  legend('latency-legend',latKeys);
  lineChart($('latency-chart'),pts,latKeys,labels,p=>`${when(p)}\n首字 P50 ${ms(p.first_byte_p50_ms)} · P95 ${ms(p.first_byte_p95_ms)}\n总耗时 P50 ${ms(p.duration_p50_ms)} · P95 ${ms(p.duration_p95_ms)}`,msTick,span+'延迟趋势');

  const live=m.live,liveKeys=[{key:'active',label:'处理中',color:palette.active,width:2},{key:'waiting',label:'排队',color:palette.waiting,width:2},{key:'capacity',label:'可用并发上限',color:palette.capacity,dash:'4 3'}];
  legend('live-legend',liveKeys);
  const lastLive=live[live.length-1];
  $('live-caption').textContent=lastLive?`当前 ${lastLive.active} 处理中 · ${lastLive.waiting} 排队 · 上限 ${lastLive.capacity}`:'服务刚启动，等待采样';
  const clock=v=>new Date(v).toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit',hour12:false,timeZone:'Asia/Shanghai'});
  lineChart($('live-chart'),live,liveKeys,live.map(s=>clock(s.at)),s=>`${clock(s.at)} 处理中 ${s.active} · 排队 ${s.waiting} · 上限 ${s.capacity}`,v=>fmt.format(Math.round(v)),'最近一小时并发与排队');

  const errors=days?m.errors_days:m.errors_24h,attempts=sum('tracked')+sum('rejected'),errTotal=errors.reduce((a,e)=>a+e.count,0);
  $('errors-caption').textContent=errTotal?`${integer.format(errTotal)} 次 · 占 ${percent(errTotal,attempts)}`:span+'无异常';
  barList($('error-list'),errors,attempts,n=>errorLabels[n]||n,palette.failed);
  const models=days?m.models_days:m.models_24h,modelTotal=models.reduce((a,e)=>a+e.count,0);
  $('models-caption').textContent=modelTotal?`${integer.format(modelTotal)} 次请求`:'';
  barList($('model-list'),models,modelTotal,n=>n==='other'?'其他模型':n,palette.dur);
  const since='错误、模型与延迟统计始于 '+time(m.since);$('errors-caption').title=$('models-caption').title=since;

  const credits=sum('credits');
  $('credits-trend-caption').textContent=`${span}消耗 ${number(credits)} 积分 · 按官方用量变化估算`;
  stackedBars($('credits-chart'),pts,[{key:'credits',color:palette.credits}],labels,p=>`${when(p)} 消耗 ${number(p.credits)} 积分`,compact,span+'积分消耗');
  renderForecast(m.forecast);
}
for(const b of document.querySelectorAll('[data-range]'))b.addEventListener('click',()=>{state.range=b.dataset.range;document.querySelectorAll('[data-range]').forEach(x=>{x.classList.toggle('active',x===b);x.setAttribute('aria-pressed',String(x===b));});renderMetrics();});
// ---- SOCKS5 proxies: each account either connects directly or through exactly one proxy.
const proxyById=id=>state.proxies.find(x=>x.id===id);
const proxyAddress=x=>(x.host.includes(':')?'['+x.host+']':x.host)+':'+x.port;
function proxyLabel(id){const x=proxyById(id);return x?(x.name||proxyAddress(x)):'未知代理';}
function renderProxies(){
  $('proxy-count').textContent=state.proxies.length;$('proxy-visible').textContent=state.proxies.length;
  $('proxy-rows').replaceChildren();$('proxy-empty').hidden=!!state.proxies.length;
  for(const x of state.proxies){
    const row=el('tr');const name=el('td');const wrap=el('div','account-identity');wrap.append(el('div','avatar','⇄'));
    const text=el('div');text.append(el('div','identity-name',x.name||proxyAddress(x)),el('div','identity-email','添加于 '+shortTime(x.created_at)));wrap.append(text);name.append(wrap);row.append(name);
    row.append(el('td','mono',proxyAddress(x)));
    row.append(el('td','',x.username?x.username+(x.has_password?' · 已设密码':''):'无认证'));
    const used=el('td');used.append(el('div','credit-value',x.accounts+' 个账号'));const names=state.accounts.filter(a=>!a.deleted_at&&a.proxy_id===x.id).map(a=>a.email||a.name);
    if(names.length){const list=el('div','subtext',names.slice(0,2).join('、')+(names.length>2?` 等 ${names.length} 个`:''));list.title=names.join('\n');used.append(list);}row.append(used);
    const test=el('td');const r=state.tests[x.id];
    if(!r)test.append(el('span','subtext','尚未测试'));
    else if(r.pending)test.append(el('span','subtext','正在测试…'));
    else{const badge=el('span','state '+(r.ok?'ready':'error'));badge.append(el('i'),document.createTextNode(r.ok?'可用 · '+ms(r.latency):'不可用'));test.append(badge);if(!r.ok){const why=el('div','subtext',r.error);why.title=r.error;test.append(why);}}
    row.append(test);
    const actions=el('td');const group=el('div','row-actions');group.append(button('测试',()=>testProxy(x)),button('编辑',()=>openProxy(x)),button('删除',()=>openProxyDelete(x),'delete'));actions.append(group);row.append(actions);
    $('proxy-rows').append(row);
  }
}
async function testProxy(x){state.tests[x.id]={pending:true};renderProxies();try{const r=await request('proxies/'+encodeURIComponent(x.id)+'/test',{method:'POST',body:'{}'});state.tests[x.id]={ok:r.ok,latency:r.latency_ms,error:r.error};}catch(e){state.tests[x.id]={ok:false,error:e.message};}renderProxies();}
function renderChecklist(){const n=$('proxy-accounts').querySelectorAll('input:checked').length;$('proxy-selected').textContent=`已选 ${n} 个`;}
function openProxy(x){
  state.proxyEdit=x?{id:x.id,version:x.version}:null;$('proxy-form').reset();errorAt('proxy-error','');
  $('proxy-title').textContent=x?'编辑 SOCKS5 代理':'添加 SOCKS5 代理';
  if(x){$('proxy-name').value=x.name;$('proxy-host').value=x.host;$('proxy-port').value=x.port;$('proxy-user').value=x.username||'';}
  $('proxy-pass').placeholder=x&&x.has_password?'留空保留原密码':'';
  const box=$('proxy-accounts');box.replaceChildren();const live=state.accounts.filter(a=>!a.deleted_at);
  if(!live.length)box.append(el('p','chart-empty','还没有账号'));
  for(const a of live){const label=el('label','check-item');const input=el('input');input.type='checkbox';input.value=a.id;input.checked=!!x&&a.proxy_id===x.id;input.addEventListener('change',renderChecklist);
    const text=el('span','check-text');text.append(el('span','check-name',a.email||a.name));
    if(a.proxy_id&&(!x||a.proxy_id!==x.id))text.append(el('span','check-note','当前：'+proxyLabel(a.proxy_id)));else if(!a.proxy_id)text.append(el('span','check-note','当前：直连'));
    label.append(input,text);box.append(label);}
  renderChecklist();$('proxy-dialog').showModal();
}
// Accept a pasted socks5://user:pass@host:port and split it into the form fields.
function splitProxyURL(){
  const v=$('proxy-host').value.trim();if(!/:\/\/|@|^[^:\[\]]+:\d+$|^\[.+\]:\d+$/.test(v))return true;
  let u;try{u=new URL(v.includes('://')?v:'socks5://'+v);}catch{errorAt('proxy-error','代理地址格式无法识别');return false;}
  if(!/^socks5h?:$/i.test(u.protocol)){errorAt('proxy-error','只支持 SOCKS5 代理');return false;}
  $('proxy-host').value=u.hostname.replace(/^\[|\]$/g,'');if(u.port)$('proxy-port').value=u.port;
  if(u.username){$('proxy-user').value=decodeURIComponent(u.username);$('proxy-pass').value=decodeURIComponent(u.password);}
  errorAt('proxy-error','');return true;
}
$('proxy-host').addEventListener('change',splitProxyURL);
$('add-proxy').addEventListener('click',()=>openProxy(null));
$('proxy-all').addEventListener('click',()=>{$('proxy-accounts').querySelectorAll('input').forEach(i=>{i.checked=true;});renderChecklist();});
$('proxy-none').addEventListener('click',()=>{$('proxy-accounts').querySelectorAll('input').forEach(i=>{i.checked=false;});renderChecklist();});
$('proxy-dialog').addEventListener('close',()=>{$('proxy-pass').value='';});
$('proxy-form').addEventListener('submit',async event=>{
  event.preventDefault();if(!splitProxyURL())return;errorAt('proxy-error','');const b=$('save-proxy');b.disabled=true;
  try{
    const body={name:$('proxy-name').value,host:$('proxy-host').value,port:Number($('proxy-port').value),username:$('proxy-user').value,account_ids:[...$('proxy-accounts').querySelectorAll('input:checked')].map(i=>i.value)};
    if($('proxy-pass').value)body.password=$('proxy-pass').value;
    if(state.proxyEdit){body.version=state.proxyEdit.version;await request('proxies/'+encodeURIComponent(state.proxyEdit.id),{method:'PATCH',body:JSON.stringify(body)});delete state.tests[state.proxyEdit.id];}
    else await request('proxies',{method:'POST',body:JSON.stringify(body)});
    $('proxy-dialog').close();toast(`代理已保存，${body.account_ids.length} 个账号经由此代理，正在重新检查`);await reload();
  }catch(e){errorAt('proxy-error',e.message);}finally{b.disabled=false;}
});
function openProxyDelete(x){state.proxyDelete={id:x.id,version:x.version};$('proxy-delete-name').textContent=(x.name?x.name+' · ':'')+proxyAddress(x);errorAt('proxy-delete-error',x.accounts?`${x.accounts} 个账号正在使用此代理。请先在「编辑」中取消勾选，避免这些账号改为直连。`:'');$('proxy-delete-submit').disabled=!!x.accounts;$('proxy-delete-dialog').showModal();}
$('proxy-delete-form').addEventListener('submit',async event=>{event.preventDefault();const b=$('proxy-delete-submit');b.disabled=true;try{await request('proxies/'+encodeURIComponent(state.proxyDelete.id),{method:'DELETE',body:JSON.stringify({version:state.proxyDelete.version})});$('proxy-delete-dialog').close();toast('代理已删除');await reload();}catch(e){errorAt('proxy-delete-error',e.message);}finally{b.disabled=false;}});
// ---- Menu: the overview and the account pool are separate views selected by the URL hash.
// ---- Client API keys. A created key is shown once; the server keeps only its hash.
const keyById=id=>id?state.keys.find(k=>k.id===id):null;
const keyLabel=k=>k.name+(k.deleted_at?'（已删除）':!k.enabled?'（已停用）':'');
function renderKeyFilter(){
  const select=$('metrics-key');const current=state.metricsKey;select.replaceChildren(new Option('全部 Key',''));
  for(const k of state.keys)select.append(new Option(keyLabel(k)+' · '+k.hint+'…',k.id));
  select.value=current;
}
async function loadMetrics(){
  const scope=state.metricsKey;
  try{const m=await request('metrics'+(scope?'?key='+encodeURIComponent(scope):''));if(scope===state.metricsKey){state.metrics=m;renderMetrics();}}
  catch(e){toast(e.message);}
}
function showKeyUsage(k){state.metricsKey=k.id;renderKeyFilter();if(location.hash!=='#overview')location.hash='#overview';loadMetrics();}
$('metrics-key').addEventListener('change',e=>{state.metricsKey=e.target.value;loadMetrics();});
function renderKeys(){
  const live=state.keys.filter(k=>!k.deleted_at);
  $('key-count').textContent=live.length;$('key-visible').textContent=live.length;$('key-rows').replaceChildren();
  for(const k of state.keys){
    const row=el('tr');const name=el('td');const wrap=el('div','account-identity');wrap.append(el('div','avatar','⚿'));
    const text=el('div');const title=el('div','identity-name',k.name);title.title=k.name;
    text.append(title,el('div','identity-email',k.builtin?'服务器 .env · KIRO_CLIENT_TOKEN':'创建于 '+shortTime(k.created_at)));wrap.append(text);name.append(wrap);row.append(name);
    row.append(el('td','mono',k.hint+'…'));
    const status=el('td');const st=k.deleted_at?'deleted':k.enabled?'ready':'disabled';const badge=el('span','state '+st);
    badge.append(el('i'),document.createTextNode(k.deleted_at?'已删除':k.enabled?'启用':'已停用'));status.append(badge);row.append(status);
    const today=el('td');today.append(el('div','credit-value',integer.format(k.today.requests)),el('div','subtext',k.today.failed?`${k.today.failed} 次失败`:k.today.rejected?`${k.today.rejected} 次被拒`:'无失败'));row.append(today);
    const day=el('td');day.append(el('div','credit-value',integer.format(k.last_24h.requests)+' 次'+(k.last_24h.credits?' · '+number(k.last_24h.credits)+' 积分':'')),el('div','subtext',`输入 ${compact(k.last_24h.input_tokens)} · 输出 ${compact(k.last_24h.output_tokens)}`));row.append(day);
    row.append(el('td','',time(k.last_used)));
    const actions=el('td');const group=el('div','row-actions');group.append(button('用量',()=>showKeyUsage(k)));
    if(!k.builtin&&!k.deleted_at)group.append(button('重命名',()=>openKeyEdit(k)),button(k.enabled?'停用':'启用',()=>toggleKey(k)),button('删除',()=>openKeyDelete(k),'delete'));
    actions.append(group);row.append(actions);$('key-rows').append(row);
  }
}
async function toggleKey(k){try{await request('keys/'+encodeURIComponent(k.id),{method:'PATCH',body:JSON.stringify({version:k.version,enabled:!k.enabled})});toast(k.enabled?'Key 已停用，新请求将返回 401':'Key 已启用');}catch(e){toast(e.message);}await reload();}
function openKeyEdit(k){state.keyEdit={id:k.id,version:k.version};$('key-edit-name').value=k.name;errorAt('key-edit-error','');$('key-edit-dialog').showModal();}
function openKeyDelete(k){state.keyDelete={id:k.id,version:k.version};$('key-delete-name').textContent=k.name+' · '+k.hint+'…';errorAt('key-delete-error','');$('key-delete-dialog').showModal();}
function resetKeyDialog(){$('key-form').reset();$('key-value').value='';$('key-create-fields').hidden=false;$('key-result').hidden=true;$('key-submit').hidden=false;$('key-cancel').textContent='取消';errorAt('key-error','');}
$('add-key').addEventListener('click',()=>{resetKeyDialog();$('key-dialog').showModal();});
$('key-dialog').addEventListener('close',resetKeyDialog);
// Also clear on the buttons themselves, so the one-time key never lingers in the page.
for(const b of $('key-dialog').querySelectorAll('[data-close]'))b.addEventListener('click',resetKeyDialog);
$('key-form').addEventListener('submit',async event=>{
  event.preventDefault();errorAt('key-error','');const b=$('key-submit');b.disabled=true;
  try{
    const r=await request('keys',{method:'POST',body:JSON.stringify({name:$('key-name').value})});
    $('key-value').value=r.key;$('key-create-fields').hidden=true;$('key-result').hidden=false;b.hidden=true;$('key-cancel').textContent='我已保存，关闭';
    $('key-value').focus();$('key-value').select();await reload();
  }catch(e){errorAt('key-error',e.message);}finally{b.disabled=false;}
});
$('key-copy').addEventListener('click',async()=>{const input=$('key-value');try{await navigator.clipboard.writeText(input.value);toast('已复制到剪贴板');}catch{input.select();toast('请按 Ctrl+C 复制');}});
$('key-edit-form').addEventListener('submit',async event=>{event.preventDefault();errorAt('key-edit-error','');const b=event.submitter;b.disabled=true;try{await request('keys/'+encodeURIComponent(state.keyEdit.id),{method:'PATCH',body:JSON.stringify({version:state.keyEdit.version,name:$('key-edit-name').value})});$('key-edit-dialog').close();toast('名称已更新');await reload();}catch(e){errorAt('key-edit-error',e.message);}finally{b.disabled=false;}});
$('key-delete-form').addEventListener('submit',async event=>{event.preventDefault();const b=$('key-delete-submit');b.disabled=true;try{await request('keys/'+encodeURIComponent(state.keyDelete.id),{method:'DELETE',body:JSON.stringify({version:state.keyDelete.version})});$('key-delete-dialog').close();toast('Key 已删除');await reload();}catch(e){errorAt('key-delete-error',e.message);}finally{b.disabled=false;}});
// ---- Model catalog: what /v1 serves, compared with what Kiro offers an account right now.
const tokens=v=>v>=1e6?fmt.format(v/1e6)+'M':v>=1e3?fmt.format(Math.round(v/1e3))+'K':String(v);
const thinkingLabels={adaptive:'自适应思考',reasoning:'推理强度',legacy_hint:'提示词思考',upstream_default:'上游默认'};
function thinkingText(t){if(!t)return '—';const label=thinkingLabels[t.mode]||t.mode;return t.efforts?.length?label+' · '+t.efforts.join('/'):label;}
const sourceLabels={builtin:'程序内置列表',file:'部署时的 account-models.json',admin:'管理页保存的列表'};
const checkLabels={new:'新增',changed:'有变化',same:'已在列表'};
function modelCell(m){const td=el('td');const name=el('div','identity-name',m.name);name.title=m.description||m.name;td.append(name,el('div','identity-email mono',m.id));return td;}
function renderCatalog(){
  const c=state.catalog;if(!c)return;
  $('model-count').textContent=c.models.length;$('model-visible').textContent=c.models.length;
  $('catalog-caption').textContent=`来源：${sourceLabels[c.source]||c.source}`+(c.updated_at?` · 更新于 ${time(c.updated_at)}`:'');
  $('model-rows').replaceChildren();
  for(const m of c.models){
    const row=el('tr');row.append(modelCell(m));
    row.append(el('td','',tokens(m.max_input_tokens)+' / '+tokens(m.max_output_tokens)));
    row.append(el('td','',m.rate_multiplier?'×'+fmt.format(m.rate_multiplier):'—'));
    row.append(el('td','',thinkingText(m.thinking)));
    const alias=el('td','subtext',(m.aliases||[]).join('、')||'—');alias.title=(m.aliases||[]).join('\n');row.append(alias);
    const actions=el('td');const group=el('div','row-actions');group.append(button('移除',()=>openModelRemove(m),'delete'));actions.append(group);row.append(actions);
    $('model-rows').append(row);
  }
}
function renderCheck(){
  const r=state.check;$('model-check-panel').hidden=!r;if(!r)return;
  const counts={new:0,changed:0,same:0};for(const m of r.models)counts[m.status]++;
  $('check-count').textContent=r.models.length;
  $('check-caption').textContent=`使用 ${r.account}${r.plan_type?'（'+planLabel(r.plan_type)+'）':''} 检测于 ${time(r.checked_at)} · 新增 ${counts.new} · 有变化 ${counts.changed} · 已在列表 ${counts.same}`;
  $('check-rows').replaceChildren();
  // New and changed models first; unchanged ones are listed for completeness.
  const order={new:0,changed:1,same:2};
  for(const m of [...r.models].sort((a,b)=>order[a.status]-order[b.status])){
    const row=el('tr',m.status==='same'?'muted-row':'');const pick=el('td');
    if(m.status!=='same'){const input=el('input');input.type='checkbox';input.checked=state.checkPick.has(m.id);input.setAttribute('aria-label',(m.status==='new'?'添加 ':'更新 ')+m.id);
      input.addEventListener('change',()=>{if(input.checked)state.checkPick.add(m.id);else state.checkPick.delete(m.id);renderApply();});pick.append(input);}
    row.append(pick,modelCell(m));
    const st=el('td');const badge=el('span','state '+({new:'ready',changed:'cooldown',same:'disabled'}[m.status]));badge.append(el('i'),document.createTextNode(checkLabels[m.status]));st.append(badge);
    if(m.status==='changed'){const cur=state.catalog?.models.find(x=>x.id===m.id);if(cur){const diffs=[];if(cur.max_input_tokens!==m.max_input_tokens||cur.max_output_tokens!==m.max_output_tokens)diffs.push('原 '+tokens(cur.max_input_tokens)+' / '+tokens(cur.max_output_tokens));if(cur.rate_multiplier!==m.rate_multiplier)diffs.push('原倍率 ×'+fmt.format(cur.rate_multiplier||0));if(thinkingText(cur.thinking)!==thinkingText(m.thinking))diffs.push('原 '+thinkingText(cur.thinking));if(cur.name!==m.name)diffs.push('原名 '+cur.name);const d=el('div','subtext',diffs.join(' · '));d.title=diffs.join('\n');st.append(d);}}
    row.append(st,el('td','',tokens(m.max_input_tokens)+' / '+tokens(m.max_output_tokens)),el('td','',m.rate_multiplier?'×'+fmt.format(m.rate_multiplier):'—'),el('td','',thinkingText(m.thinking)));
    $('check-rows').append(row);
  }
  const box=$('check-missing');box.replaceChildren();box.hidden=!r.missing.length&&!r.invalid.length;
  if(r.missing.length){box.append(el('strong','','这个账号看不到以下模型：'),document.createTextNode(r.missing.join('、')),el('p','field-hint','可能已下线，也可能只对其他订阅开放。确认后可在下方「当前模型」里移除。'));}
  if(r.invalid.length){box.append(el('p','field-hint','跳过 '+r.invalid.length+' 个格式无法识别的条目：'+r.invalid.join('；')));}
  renderApply();
}
function renderApply(){const n=state.checkPick.size;$('apply-models').disabled=!n;$('apply-models').textContent=n?`应用所选变更（${n}）`:'应用所选变更';}
$('check-models').addEventListener('click',async()=>{
  const b=$('check-models');b.disabled=true;errorAt('model-error','');
  try{const r=await request('models/check',{method:'POST',body:'{}'});state.check=r;state.checkPick=new Set(r.models.filter(m=>m.status==='new').map(m=>m.id));renderCheck();
    const fresh=r.models.filter(m=>m.status!=='same').length;toast(fresh?`发现 ${fresh} 个新增或有变化的模型`:'线上模型与当前列表一致');}
  catch(e){toast(e.message);errorAt('global-error',e.message);}finally{b.disabled=false;}
});
$('check-select-new').addEventListener('click',()=>{if(!state.check)return;state.checkPick=new Set(state.check.models.filter(m=>m.status!=='same').map(m=>m.id));renderCheck();});
$('apply-models').addEventListener('click',async()=>{
  const b=$('apply-models');b.disabled=true;errorAt('model-error','');
  try{const ids=[...state.checkPick];state.catalog=await request('models/apply',{method:'POST',body:JSON.stringify({revision:state.catalog.revision,add:ids})});
    // Re-mark against the new catalog so applied rows read as "已在列表".
    for(const m of state.check.models)if(ids.includes(m.id))m.status='same';state.checkPick.clear();renderCatalog();renderCheck();toast(`已生效：${ids.join('、')}`);}
  catch(e){errorAt('model-error',e.message);renderApply();}
});
function openModelRemove(m){state.modelRemove=m.id;$('model-remove-name').textContent=m.name+' · '+m.id;errorAt('model-remove-error','');$('model-remove-dialog').showModal();}
$('model-remove-form').addEventListener('submit',async event=>{event.preventDefault();const b=$('model-remove-submit');b.disabled=true;
  try{const id=state.modelRemove;state.catalog=await request('models/apply',{method:'POST',body:JSON.stringify({revision:state.catalog.revision,remove:[id]})});$('model-remove-dialog').close();
    if(state.check){const m=state.check.models.find(x=>x.id===id);if(m)m.status='new';state.check.missing=state.check.missing.filter(x=>x!==id);}renderCatalog();renderCheck();toast('已移除 '+id);}
  catch(e){errorAt('model-remove-error',e.message);}finally{b.disabled=false;}});
// ---- Usage log: one row per finished /v1 request, read a day at a time from the server.
const usageState={offset:0,limit:100,loading:false,date:''};
const resultLabels={ok:'成功',failed:'失败',canceled:'已取消'};
const credit=v=>v===undefined||v===null?'—':new Intl.NumberFormat('zh-CN',{maximumFractionDigits:4}).format(v);
function fillSelect(id,items,label,current){const s=$(id);const first=s.options[0];s.replaceChildren(first);for(const[value,text]of items)s.append(new Option(text,value));s.value=items.some(([v])=>v===current)?current:'';}
async function loadUsage(){
  if(usageState.loading)return;usageState.loading=true;
  const q=new URLSearchParams({limit:usageState.limit,offset:usageState.offset});
  for(const[k,id]of [['date','usage-date'],['key','usage-key'],['account','usage-account'],['model','usage-model'],['result','usage-result']]){const v=$(id).value;if(v)q.set(k,v);}
  if(!$('usage-date').value&&usageState.date)q.set('date',usageState.date);
  try{
    const r=await request('usage?'+q);const p=r.page;usageState.date=p.date;
    const dates=r.dates.includes(p.date)?r.dates:[p.date,...r.dates];
    const ds=$('usage-date');ds.replaceChildren(...dates.map(d=>new Option(d+(d===dates[0]?'（最新）':''),d)));ds.value=p.date;
    fillSelect('usage-key',[...new Set([...p.key_ids,...state.keys.map(k=>k.id)])].map(id=>{const k=keyById(id);return [id,k?keyLabel(k):id.slice(0,8)];}),'',$('usage-key').value||q.get('key')||'');
    fillSelect('usage-account',state.accounts.map(a=>[a.id,(a.email||a.name)+(a.deleted_at?'（已删除）':'')]),'',q.get('account')||'');
    fillSelect('usage-model',p.models.map(m=>[m,m]),'',q.get('model')||'');
    renderUsage(p);
  }catch(e){toast(e.message);}finally{usageState.loading=false;}
}
function renderUsage(p){
  const t=p.totals;const box=$('usage-totals');box.replaceChildren();
  for(const[label,value,sub]of [['请求',integer.format(t.requests),`成功 ${integer.format(t.succeeded)} · 失败 ${integer.format(t.failed)} · 取消 ${integer.format(t.canceled)}`],['输入 token',compact(t.input_tokens),'不含缓存读取'],['缓存读取',compact(t.cache_read_tokens),percent(t.cache_read_tokens,t.input_tokens+t.cache_read_tokens)+' 的输入'],['输出 token',compact(t.output_tokens),''],['Kiro 积分',credit(t.credits),`${integer.format(t.metered)} 次有计量`]]){const b=el('div','stat');b.append(el('span','',label),el('strong','',value));if(sub)b.append(el('small','',sub));box.append(b);}
  if(p.dropped)box.append(el('p','field-hint',`服务启动以来有 ${p.dropped} 条日志因写盘繁忙或失败未记录`));
  $('usage-rows').replaceChildren();$('usage-empty').hidden=!!p.entries.length;
  for(const e of p.entries){
    const row=el('tr');const k=keyById(e.key_id);
    row.append(el('td','mono',new Date(e.time).toLocaleTimeString('zh-CN',{hour12:false,timeZone:'Asia/Shanghai'})));
    const key=el('td','',k?k.name:(e.key_id?e.key_id.slice(0,8):'—'));row.append(key);
    const acc=el('td','subtext',e.account||e.account_id.slice(0,8));acc.title=e.account||e.account_id;if(e.affinity==='moved'){acc.append(el('div','subtext','换号 · 无缓存优惠'));}row.append(acc);
    const model=el('td','',e.model||'—');if(e.stream)model.append(el('span','subtext',' 流式'));row.append(model);
    row.append(el('td','',integer.format(e.input_tokens)),el('td','',integer.format(e.cache_read_tokens)),el('td','',integer.format(e.output_tokens)),el('td','credit-value',credit(e.credits)));
    row.append(el('td','',ms(e.duration_ms)+(e.first_byte_ms?' / '+ms(e.first_byte_ms):'')));
    const res=el('td');const badge=el('span','state '+({ok:'ready',failed:'error',canceled:'disabled'}[e.result]||'error'));badge.append(el('i'),document.createTextNode(resultLabels[e.result]||e.result));res.append(badge);
    if(e.error&&e.result!=='ok'){const why=el('div','subtext',(errorLabels[e.error]||e.error)+(e.upstream_status?' · 上游 '+e.upstream_status:''));res.append(why);}
    row.append(res);$('usage-rows').append(row);
  }
  const from=p.total?usageState.offset+1:0,to=usageState.offset+p.entries.length;
  $('usage-page-info').textContent=`${p.date} · 共 ${integer.format(p.total)} 条，显示 ${from}–${to}（最新在前）`;
  $('usage-prev').disabled=usageState.offset===0;$('usage-next').disabled=to>=p.total;
}
for(const id of ['usage-date','usage-key','usage-account','usage-model','usage-result'])$(id).addEventListener('change',()=>{usageState.offset=0;if(id==='usage-date')usageState.date=$('usage-date').value;loadUsage();});
$('usage-prev').addEventListener('click',()=>{usageState.offset=Math.max(0,usageState.offset-usageState.limit);loadUsage();});
$('usage-next').addEventListener('click',()=>{usageState.offset+=usageState.limit;loadUsage();});
$('usage-refresh').addEventListener('click',()=>{usageState.offset=0;loadUsage();});
// ---- Error log: the full scene of failed requests. Everything in a dump is
// arbitrary user or upstream text, so it is only ever rendered through textContent.
const dumpKinds={upstream_status:'上游返回错误',upstream_unreachable:'连不上上游',proxy_error:'账号代理错误',stream_exception:'响应中的异常',parse_error:'响应解析失败',no_account:'没有可用账号',conversion_error:'请求转换失败',build_error:'请求构建失败'};
const dumpState={loading:false,current:null};
function dumpStatus(s){const parts=[];if(s.upstream_status)parts.push('上游 '+s.upstream_status);if(s.client_status)parts.push('返回 '+s.client_status);return parts.join(' → ')||'—';}
async function loadErrorDumps(){
  if(dumpState.loading)return;dumpState.loading=true;
  try{
    const r=await request('error-dumps');const rows=$('error-rows');rows.replaceChildren();
    $('errors-off').hidden=r.enabled;$('errors-empty').hidden=!r.enabled||!!r.dumps.length;
    $('errors-info').textContent=r.enabled?`共 ${r.dumps.length} 条（最新在前，最多保留 ${r.keep} 条）`:'记录已关闭';
    for(const s of r.dumps){
      const row=el('tr');row.append(el('td','mono',time(s.time)));
      const kind=el('td','',dumpKinds[s.kind]||s.kind);kind.title=s.error||'';row.append(kind);
      const status=el('td','',dumpStatus(s));if(s.retried)status.append(el('span','dump-retried','已换号重试 · 第 '+s.attempt+' 次'));else if(s.attempt>1)status.append(el('span','dump-retried','第 '+s.attempt+' 次（换号后）'));row.append(status);
      const acc=el('td','subtext',s.account||(s.account_id?s.account_id.slice(0,8):'—'));acc.title=s.account_id||'';row.append(acc);
      const model=el('td','',s.model||'—');if(s.stream)model.append(el('span','subtext',' 流式'));row.append(model);
      row.append(el('td','mono',s.request_id||'—'));
      const actions=el('td');const group=el('div','row-actions');const view=button('查看',()=>openErrorDump(s.id));view.setAttribute('aria-label','查看错误详情 '+time(s.time));group.append(view);actions.append(group);row.append(actions);
      rows.append(row);
    }
  }catch(e){toast(e.message);}finally{dumpState.loading=false;}
}
// pretty shows JSON indented; anything else is shown as it is.
function pretty(text){try{return JSON.stringify(JSON.parse(text),null,2);}catch{return text;}}
function headerLines(h){return Object.keys(h||{}).sort().flatMap(k=>h[k].map(v=>k+': '+v));}
function bodyNote(p){const notes=[];if(p.body_encoding==='base64')notes.push('以下为 base64，原始内容不是 UTF-8 文本');if(p.body_truncated)notes.push(`已截断，原始大小 ${integer.format(p.body_bytes)} 字节，只保留前 2 MiB`);return notes.length?'（'+notes.join('；')+'）':'';}
// dumpSections returns [title, shown, copied] per section: shown formats JSON, copied is the original text.
function dumpSections(d){
  const out=[];const part=(title,head,p,extra=[])=>{
    const note=bodyNote(p);const lines=[...head,...headerLines(p.headers)];
    const build=body=>[...lines,'',...extra,...(note?[note]:[]),body].join('\n');
    out.push([title,build(p.body_encoding==='base64'?p.body:pretty(p.body)),build(p.body)]);};
  if(d.client)part('客户端请求',[d.client.method+' '+d.client.path],d.client);else out.push(['客户端请求','（无）','（无）']);
  if(d.upstream_request)part('发给 Kiro',['POST '+d.upstream_request.url],d.upstream_request);else out.push(['发给 Kiro','（这次失败没有发给 Kiro）','（这次失败没有发给 Kiro）']);
  const r=d.upstream_response;
  if(r){const extra=[];if(r.event!==undefined){extra.push('异常事件：'+(r.event_truncated?`（已截断，原始大小 ${integer.format(r.event_bytes)} 字节，只保留前 2 MiB）`:''),typeof r.event==='string'?r.event:JSON.stringify(r.event,null,2),'');if(r.read_bytes||r.events)extra.push(`出错前已读取 ${r.read_bytes} 字节、${r.events} 个事件`,'');}part('Kiro 响应',['HTTP '+r.status],r,extra);}
  else out.push(['Kiro 响应',d.upstream_request?'（没有收到 Kiro 的响应）':'（无）',d.upstream_request?'（没有收到 Kiro 的响应）':'（无）']);
  return out;
}
function dumpSummaryLines(s){return [['时间',time(s.time)],['类型',(dumpKinds[s.kind]||s.kind)+' ('+s.kind+')'],['说明',s.error||'—'],['request_id',s.request_id||'—'],['尝试',`第 ${s.attempt} 次`+(s.retried?'，已换号重试':'')],['账号',s.account_id?(s.account||'—')+' · '+s.account_id:'—'],['模型',(s.model||'—')+(s.stream?' · 流式':'')],['上游状态',s.upstream_status||'—'],['返回客户端',s.client_status||(s.retried?'未返回（已换号重试）':'—')],['记录编号',s.id]];}
async function copyText(text,fallback){try{await navigator.clipboard.writeText(text);toast('已复制到剪贴板');}catch{const range=document.createRange();range.selectNodeContents(fallback);const sel=getSelection();sel.removeAllRanges();sel.addRange(range);toast('请按 Ctrl+C 复制');}}
async function openErrorDump(id){
  let d;try{d=await request('error-dumps/'+encodeURIComponent(id));}catch(e){toast(e.message);return;}
  dumpState.current=d;const s=d.summary;
  $('error-title').textContent=(dumpKinds[s.kind]||s.kind)+' · '+time(s.time);
  const meta=$('error-meta');meta.replaceChildren();for(const[label,value]of dumpSummaryLines(s))meta.append(el('dt','',label),el('dd','',String(value)));
  const box=$('error-sections');box.replaceChildren();
  for(const[title,shown,copied]of dumpSections(d)){
    const sec=el('section','dump-section');const head=el('header');const h=el('h3','',title);head.append(h);
    const pre=el('pre','dump-pre',shown);pre.tabIndex=0;pre.setAttribute('aria-label',title+'内容');
    const copy=button('复制',()=>copyText(copied,pre),'button secondary');copy.setAttribute('aria-label','复制'+title);head.append(copy);
    sec.append(head,pre);box.append(sec);
  }
  $('error-dialog').showModal();
}
$('error-copy-all').addEventListener('click',()=>{const d=dumpState.current;if(!d)return;
  const text=[...dumpSummaryLines(d.summary).map(([k,v])=>k+'：'+v),...dumpSections(d).flatMap(([title,,copied])=>['','===== '+title+' =====',copied])].join('\n');
  copyText(text,$('error-sections'));});
$('errors-refresh').addEventListener('click',loadErrorDumps);
const views={overview:'运行总览',accounts:'账号池',proxies:'代理',models:'模型',usage:'使用日志',errors:'错误日志',keys:'API Key'};
function showView(){
  const view=location.hash.slice(1) in views?location.hash.slice(1):'overview';
  for(const n of document.querySelectorAll('[data-page]'))n.hidden=n.dataset.page!==view;
  for(const a of document.querySelectorAll('[data-view]')){const on=a.dataset.view===view;a.classList.toggle('selected',on);if(on)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current');}
  $('crumb').textContent=views[view];document.title='Kiro · '+views[view];
  // Charts size themselves from their container, which is zero-width while hidden.
  if(view==='overview')renderMetrics();
  if(view==='usage')loadUsage();
  if(view==='errors')loadErrorDumps();
}
addEventListener('hashchange',showView);showView();
let resizeTimer,lastWidth=innerWidth;addEventListener('resize',()=>{clearTimeout(resizeTimer);resizeTimer=setTimeout(()=>{if(innerWidth!==lastWidth){lastWidth=innerWidth;renderMetrics();}},150);});
