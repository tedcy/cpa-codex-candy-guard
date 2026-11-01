(() => {
  'use strict';
  const base = '/v0/management/plugins/cpa-codex-candy-guard';
  const keyStore = 'cpa-codex-candy-guard.key';
  const $ = id => document.getElementById(id);
  let key = '', authRevision = 0, state = null, dirty = false, pending = false, selected = new Set();
  const labels = { completed:'完成',partial:'部分结果',failed:'请求失败',cancelled:'已停止',skipped:'已跳过',unknown:'未确认',error:'异常',interrupted:'已中断',queued:'排队',running:'检测中',stopped:'已停止',paused:'已暂停' };
  const reasons = { disabled:'账号已禁用',cooldown:'冷却中',busy:'其他测试占用',unsupported_model:'不支持所选模型',identity_changed:'账号身份已变化',model_mismatch:'模型不一致',final_overload:'最终仍过载',ambiguous_records:'无法关联检测记录',record_timeout:'未取得本轮记录',dependency_error:'检测依赖异常',model_catalog_error:'模型目录读取失败',start_failed:'启动检测失败',start_not_confirmed:'启动未确认',record_read_error:'检测记录读取失败',invalid_record:'检测记录格式变化',invalid_record_status:'未知记录状态',stopped_before_action:'已停止自动关闭',stopped_before_start:'停止前未开始' };
  const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const checkIndex = item => Number(item?.check_index) === 2 ? 2 : 1;
  const mode = () => document.querySelector('[name=mode]:checked')?.value || 'normal';
  function updateModeControls() {
    const selectedMode = mode();
    $('schedule-interval-field').hidden = selectedMode !== 'scheduled';
    $('recheck-delay-field').hidden = selectedMode !== 'recheck';
    $('schedule-interval').disabled = selectedMode !== 'scheduled';
    $('recheck-delay').disabled = selectedMode !== 'recheck';
  }
  function panelValue(name) {
    try {
      let raw = localStorage.getItem(name); if (!raw) return null;
      const prefix = /^enc::v([12])::/.exec(raw);
      if (prefix) {
        const scope = location.host + (prefix[1] === '1' ? '|' + navigator.userAgent : '');
        const secret = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + scope);
        raw = new TextDecoder().decode(Uint8Array.from(atob(raw.slice(prefix[0].length)), (c,i) => c.charCodeAt(0) ^ secret[i % secret.length]));
      }
      return JSON.parse(raw);
    } catch { return null; }
  }
  function notice(message) { $('notice').textContent = message || ''; $('notice').hidden = !message; }
  async function api(path, method='GET', body) {
    if (!key) throw new Error('管理密钥无效或未登录');
    const requestKey = key, revision = authRevision;
    const response = await fetch(base + path, { method,cache:'no-store',signal:AbortSignal.timeout(120000),headers:{ Authorization:'Bearer '+requestKey, ...(body === undefined ? {} : {'Content-Type':'application/json'}) },body:body === undefined ? undefined : JSON.stringify(body) });
    if (response.status===401) {
      if (revision===authRevision && requestKey===key) {
        key='';
        try { sessionStorage.removeItem(keyStore); } catch {}
        $('login').hidden=false;$('connection').textContent='未连接';$('connection').className='badge';
      }
      throw new Error('管理密钥无效或未登录');
    }
    const data = await response.json(); if (!response.ok) throw new Error(data.error || '请求失败'); return data;
  }
  function values() { const selectedMode=mode(); return { model:$('model').value,per_credential_concurrency:Number($('per').value),credential_concurrency:Number($('global').value),interval_minutes:Number($('schedule-interval').value),recheck_delay_minutes:Number($('recheck-delay').value),schedule_enabled:selectedMode==='scheduled',recheck_enabled:selectedMode==='recheck',scope:document.querySelector('[name=scope]:checked').value,credential_ids:[...selected] }; }
  function statusBadge(status,action) {
    const bad = action==='disabled' || action==='disable_failed';
    return `<span class="badge ${bad?'bad':status==='completed'?'ok':['failed','partial','error','unknown','paused'].includes(status)?'warn':''}">${escape(labels[status] || status || '未检测')}</span>`;
  }
  function actionText(item) { return ({disabled:'已关闭',disable_pending:'关闭中',disable_failed:'关闭未确认'})[item?.action] || (item?.reason ? (reasons[item.reason] || item.reason) : '—'); }
  function itemCell(item) {
    if (!item) return '—';
    const due = checkIndex(item) === 2 && item.status === 'queued' && item.due_at ? `<small class="reason">到 ${escape(new Date(item.due_at).toLocaleString('zh-CN',{hour12:false}))}</small>` : '';
    return `${statusBadge(item.status,item.action)}${due}`;
  }
  function itemAction(items) { return items.filter(Boolean).map(item => actionText(item)).filter(value => value !== '—').join('；') || '—'; }
  function render() {
    const s=state.settings, creds=state.credentials || [];
    if (!dirty) {
      const choices = [...new Set([...(state.models || []),...(s.model ? [s.model] : [])])];
      $('model').innerHTML='<option value="">选择模型</option>'+choices.map(m=>`<option value="${escape(m)}">${escape(m)}</option>`).join('');
      $('model').value=s.model; $('per').value=s.per_credential_concurrency; $('global').value=s.credential_concurrency; $('schedule-interval').value=s.interval_minutes; $('recheck-delay').value=s.recheck_delay_minutes || 10;
      document.querySelector(`[name=mode][value="${s.recheck_enabled?'recheck':s.schedule_enabled?'scheduled':'normal'}"]`).checked=true;
      document.querySelector(`[name=scope][value="${s.scope}"]`).checked=true; selected=new Set(s.credential_ids || []);
    }
    updateModeControls();
    $('connection').textContent=state.error?(state.ready?'已暂停':'依赖异常'):'已连接'; $('connection').className='badge '+(state.error?'warn':'ok');
    $('account-count').textContent=`账号 ${creds.length}`; $('enabled-count').textContent=`启用 ${creds.filter(c=>!c.disabled).length}`;
    $('round-state').textContent=state.stopping?'停止中':state.running?'巡检进行中':'待命';
    const active=state.running?state.rounds?.at(-1):null;
    $('next').textContent=state.next_run?(active?.settings.recheck_enabled?'下次复查 ':'下次定时 ')+new Date(state.next_run).toLocaleString('zh-CN',{hour12:false}):active?.settings.recheck_enabled?'首检进行中':s.recheck_enabled?'二次检查待命':s.schedule_enabled?(state.running?'本轮结束后计时':'定时待命'):'定时关闭';
    $('run').disabled=pending || state.running || !state.ready; $('stop').disabled=pending || !state.running || state.stopping;
    $('selected-count').textContent=selected.size;
    const latest=state.rounds?.at(-1), items=new Map((latest?.items || []).map(i=>[`${i.credential_id}:${checkIndex(i)}`,i]));
    $('accounts').innerHTML=creds.length?creds.map(c=>{
      const first=items.get(`${c.id}:1`), recheck=items.get(`${c.id}:2`), accountStatus=c.disabled?'禁用':c.unavailable?'冷却':c.running?'测试中':'启用';
      return `<tr><td><input type="checkbox" data-id="${escape(c.id)}" aria-label="选择 ${escape(c.name)}" ${selected.has(c.id)?'checked':''} ${c.disabled?'disabled':''}></td><td class="account-name">${escape(c.name)}</td><td><span class="badge ${c.disabled?'':'ok'}">${accountStatus}</span></td><td>${itemCell(first)}</td><td>${itemCell(recheck)}</td><td>${escape(recheck?.prediction || first?.prediction || '—')}</td><td>${escape(itemAction([first,recheck]))}</td></tr>`;
    }).join(''):'<tr><td colspan="7" class="empty">暂无 Codex 账号</td></tr>';
    $('select-all').checked=creds.some(c=>!c.disabled)&&creds.filter(c=>!c.disabled).every(c=>selected.has(c.id));
    $('history').className=state.rounds?.length?'':'empty';
    const opened=new Set([...document.querySelectorAll('details[open]')].map(el=>el.dataset.round));
    $('history').innerHTML=state.rounds?.length?[...state.rounds].reverse().map(r=>`<details data-round="${escape(r.id)}" ${opened.has(r.id)?'open':''}><summary>${statusBadge(r.status)}<span>${escape(r.settings.model)}</span><small>${new Date(r.started).toLocaleString('zh-CN',{hour12:false})} · ${r.trigger==='timer'?'定时':'手动'} · ${r.items.filter(i=>i.action==='disabled').length} 已关闭</small></summary><div class="round-items"><table><thead><tr><th>检查</th><th>账号</th><th>状态</th><th>归因模型</th><th>处理</th></tr></thead><tbody>${r.items.map(i=>`<tr><td>${checkIndex(i)===2?'二次检查':'首检'}</td><td class="account-name">${escape(i.name)}</td><td>${itemCell(i)}</td><td>${escape(i.prediction || '—')}</td><td>${escape(actionText(i))}</td></tr>`).join('')}</tbody></table></div></details>`).join(''):'暂无运行记录';
    if (state.error) notice(state.error);
  }
  async function refresh() {
    if (!key) return false;
    const revision = authRevision;
    try {
      const updated = await api('/state');
      if (revision!==authRevision || !key) return false;
      state=updated;$('login').hidden=true;render();return true;
    } catch(e) {
      if (revision===authRevision) { $('connection').textContent='未连接';$('connection').className='badge';notice(e.message); }
      return false;
    }
  }
  async function save() { await api('/settings','PUT',values());dirty=false;notice('设置已保存'); }
  $('settings').addEventListener('input',()=>{dirty=true;});
  $('settings').addEventListener('submit',async e=>{e.preventDefault();if(pending)return;pending=true;try{await save();await refresh();}catch(e){notice(e.message);}finally{pending=false;if(state)render();}});
  $('run').addEventListener('click',async()=>{if(pending)return;pending=true;$('run').disabled=true;try{await save();await api('/run','POST');await refresh();}catch(e){notice(e.message);}finally{pending=false;if(state)render();}});
  $('stop').addEventListener('click',async()=>{if(pending)return;pending=true;try{await api('/stop','POST');await refresh();}catch(e){notice(e.message);}finally{pending=false;if(state)render();}});
  $('accounts').addEventListener('change',e=>{const id=e.target.dataset.id;if(!id)return;e.target.checked?selected.add(id):selected.delete(id);dirty=true;$('selected-count').textContent=selected.size;});
  $('select-all').addEventListener('change',e=>{for(const c of state?.credentials || [])if(!c.disabled)e.target.checked?selected.add(c.id):selected.delete(c.id);dirty=true;render();});
  $('refresh').addEventListener('click',refresh);
  document.querySelectorAll('[name=mode]').forEach(input=>input.addEventListener('change',()=>{dirty=true;updateModeControls();}));
  for(const id of ['accounts','history'])$(id+'-tab').addEventListener('click',()=>{for(const tab of ['accounts','history']){$(tab+'-tab').setAttribute('aria-selected',String(id===tab));$(tab+'-view').hidden=id!==tab;}});
  $('login').addEventListener('submit',async e=>{
    e.preventDefault();key=$('key').value.trim();const revision=++authRevision;$('key').value='';
    if (await refresh() && revision===authRevision && key) {
      try { sessionStorage.setItem(keyStore,key); } catch {}
      notice(state.error || '');
    }
  });
  const panelKey=panelValue('cli-proxy-auth')?.state?.managementKey;
  key=typeof panelKey==='string' ? panelKey.trim() : '';
  if (!key) { try { key=(sessionStorage.getItem(keyStore) || '').trim(); } catch {} }
  if (!key)$('login').hidden=false;
  window.guardIcons();refresh();setInterval(()=>{if(!document.hidden&&!pending)refresh();},4000);
})();
