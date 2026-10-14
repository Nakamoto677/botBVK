#!/usr/bin/env python3
"""Install/update only this manifest's objects through the OSS Metabase REST API.
No passwords or session tokens are persisted. Use --dry-run for an API-free preview.
"""
import argparse
import getpass
import json
import os
import re
import sys
import uuid
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlparse
from urllib.request import Request, urlopen
ROOT=Path(__file__).resolve().parent
LABELS={'start_date':'Начало периода','end_date':'Конец периода','department':'Отдел','employee':'Сотрудник','company':'Компания','ticket_id':'Номер заявки'}
TYPES={'start_date':'date','end_date':'date','department':'text','employee':'number','company':'number','ticket_id':'number'}

def tags(sql):
    return {name:{'id':str(uuid.uuid5(uuid.NAMESPACE_URL,'bvk-metabase:'+name)),'name':name,'display-name':LABELS[name],'type':TYPES[name],'required':name=='ticket_id'} for name in dict.fromkeys(re.findall(r'\{\{(\w+)\}\}',sql))}

def params(names,cards=None):
    result=[]
    lookups={'department':'filter_departments','employee':'filter_employees','company':'filter_companies'}
    for name in names:
        param={'id':name,'name':LABELS[name],'slug':name,'type':'date/single' if TYPES[name]=='date' else 'number/=' if TYPES[name]=='number' else 'string/=','sectionId':'date' if TYPES[name]=='date' else 'number' if TYPES[name]=='number' else 'string'}
        if cards and name in lookups:
            param.update({'isMultiSelect':False,'values_query_type':'list','values_source_type':'card','values_source_config':{'card_id':cards[lookups[name]],'value_field':['field','value',{'base-type':'type/Text' if name=='department' else 'type/BigInteger'}],'label_field':['field','label',{'base-type':'type/Text'}]}})
        result.append(param)
    return result

def mapping(source,target,kind='parameter'):
    return {'source':{'type':kind,'id':source},'target':{'type':'parameter','id':target}}

def click_dashboard(target,names):
    return {'type':'link','linkType':'dashboard','targetId':target,'parameterMapping':{n:mapping(n,n) for n in names}}

class API:
    def __init__(self,url,token,key=False):
        self.url=url.rstrip('/')
        self.header={'X-API-Key' if key else 'X-Metabase-Session':token}
    def call(self,method,path,body=None):
        req=Request(self.url+'/api/'+path,data=None if body is None else json.dumps(body).encode(),method=method,headers={**self.header,'Content-Type':'application/json'})
        try:
            with urlopen(req,timeout=90) as r:
                raw=r.read()
                return json.loads(raw) if raw else None
        except HTTPError as e:
            # Do not print arbitrary server bodies which can contain connection details.
            raise RuntimeError(f'Metabase {method} /api/{path}: HTTP {e.code}; inspect server logs and /api/docs') from None

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--url',default=os.environ.get('METABASE_URL','http://127.0.0.1:3000'))
    p.add_argument('--database-id',type=int,required=True,help='ID of the read-only analytics PostgreSQL connection in Metabase')
    p.add_argument('--state',type=Path,default=ROOT/'import-state.json',help='Local numeric IDs only; retain for repeat imports')
    p.add_argument('--dry-run',action='store_true')
    p.add_argument('--set-homepage',action='store_true',help='Make the support dashboard the native Metabase homepage')
    p.add_argument('--validate-queries',action='store_true',help='Run all questions through Metabase after import; requires refreshed analytics')
    args=p.parse_args()
    manifest=json.loads((ROOT/'manifest.json').read_text())
    cards=manifest['cards']
    questions={c['key']:{'name':c['name'],'description':c['description'] or None,'display':c['display'],'collection_id':None,'visualization_settings':c.get('settings',{}),'dataset_query':{'type':'native','database':args.database_id,'native':{'query':(ROOT/c['sql']).read_text(),'template-tags':tags((ROOT/c['sql']).read_text())}}} for c in cards}
    if args.dry_run:
        print(json.dumps({'compatibility_version':manifest['compatibility_version'],'collection':manifest['collection'],'dashboards':manifest['dashboards'],'questions':questions},ensure_ascii=False,indent=2))
        return
    parsed=urlparse(args.url)
    if parsed.scheme!='https' and not(parsed.scheme=='http' and parsed.hostname in ('localhost','127.0.0.1','::1')):
        p.error('Use HTTPS for remote Metabase; HTTP is allowed only on loopback')
    state=json.loads(args.state.read_text()) if args.state.exists() else {'url':args.url.rstrip('/'),'database_id':args.database_id,'cards':{},'dashboards':{}}
    if state['url']!=args.url.rstrip('/') or state['database_id']!=args.database_id:
        p.error('State belongs to a different instance or database. Use a different --state file.')
    token=os.environ.get('METABASE_API_KEY') or os.environ.get('METABASE_SESSION')
    api=API(args.url,token or '',bool(os.environ.get('METABASE_API_KEY')))
    if not token:
        username=os.environ.get('METABASE_USERNAME') or input('Metabase admin email: ')
        token=api.call('POST','session',{'username':username,'password':getpass.getpass('Metabase password: ')})['id']
        api=API(args.url,token)
    def save():
        args.state.parent.mkdir(parents=True,exist_ok=True)
        temp=args.state.with_suffix('.tmp')
        temp.write_text(json.dumps(state,ensure_ascii=False,indent=2)+'\n')
        temp.replace(args.state)
    if not state.get('collection_id'):
        state['collection_id']=api.call('POST','collection',{'name':manifest['collection'],'description':'Управляется analytics/metabase/manifest.json. Изменения импортера заменяют раскладку.'})['id']
        save()
    for c in cards:
        key=c['key']; body=questions[key]; body['collection_id']=state['collection_id']
        if key in state['cards']: api.call('PUT',f"card/{state['cards'][key]}",body)
        else:
            state['cards'][key]=api.call('POST','card',body)['id']; save()
    specs=dict(manifest['dashboards'])
    for c in cards:
        if c['key'].endswith('_list'):
            specs[c['key']]={'name':c['name'],'tabs':{},'list_card':c['key']}
    for key,spec in specs.items():
        if key not in state['dashboards']:
            state['dashboards'][key]=api.call('POST','dashboard',{'name':spec['name'],'collection_id':state['collection_id']})['id']; save()
    common=['start_date','end_date','department','employee','company']
    for key,spec in specs.items():
        did=state['dashboards'][key]
        existing=api.call('GET',f'dashboard/{did}')
        tab_by_name={t['name']:t['id'] for t in existing.get('tabs',[])}
        tabids={k:tab_by_name.get(name,-(i+1)) for i,(k,name) in enumerate(spec['tabs'].items())}
        existing_cards={d.get('card_id'):d['id'] for d in existing.get('dashcards',[])}
        dc=[]; positions={}
        selected=[c for c in cards if c['dashboard']==key or c['key']==spec.get('list_card')]
        # Explicit, task-oriented layout. Every tab retains a compact freshness strip.
        fresh=next(c for c in cards if c['key']=='status')
        if key=='support':
            by_key={c['key']:c for c in cards}
            selected=[]
            for tab,items in spec['layout'].items():
                selected.append(dict(fresh,tab=tab))
                for item in items:
                    if 'card' in item:
                        selected.append(dict(by_key[item['card']],tab=tab,width=item['width'],height=item['height']))
                    else:
                        selected.append(dict(item,tab=tab))
        else:
            back={'key':'back','text':f"[← Обзор поддержки](/dashboard/{state['dashboards']['support']}#refresh=60) · Чтобы вернуться к прежним фильтрам, используйте «Назад» в браузере.",'tab':None,'width':24,'height':2}
            selected=[back,fresh]+selected
        used=set()
        for i,c in enumerate(selected):
            tab=c['tab'] if key=='support' else None
            row,col,rowheight=positions.get(tab,(0,0,0))
            width=c['width'] if key=='support' else 24
            if col+width>24: row+=rowheight; col=0; rowheight=0
            if 'text' in c:
                candidates=[d for d in existing.get('dashcards',[]) if d.get('card_id') is None and d.get('visualization_settings',{}).get('bvk.key')==c['key'] and d.get('dashboard_tab_id')==tabids.get(tab)]
                identifier=candidates[0]['id'] if candidates else -(i+100)
                used.add(identifier)
                dc.append({'id':identifier,'card_id':None,'row':row,'col':col,'size_x':width,'size_y':c['height'],'dashboard_tab_id':tabids.get(tab),'series':[],'parameter_mappings':[],'visualization_settings':{'virtual_card':{'display':'text'},'text':c['text'],'bvk.key':c['key'],'card.background':False,'text.align_vertical':'top'}})
                positions[tab]=(row,col+width,max(rowheight,c['height']))
                continue
            cardid=state['cards'][c['key']]
            old=existing_cards.get(cardid)
            # Duplicate freshness cards occur on different tabs; keep their existing IDs when possible.
            candidates=[d for d in existing.get('dashcards',[]) if d.get('card_id')==cardid and d.get('dashboard_tab_id')==tabids.get(tab)]
            old=candidates[0]['id'] if candidates else old
            if old in used: old=None
            identifier=old if old is not None else -(i+100)
            used.add(identifier)
            available=questions[c['key']]['dataset_query']['native']['template-tags']
            names=['ticket_id'] if key=='ticket' else common
            settings=dict(c.get('settings',{}))
            if c['key'] in ('status','attention_summary'): settings['card.title']=None
            if c['target'] and c['target'].endswith('_list'):
                settings['click_behavior']=click_dashboard(state['dashboards'][c['target']],common)
            if c['target']=='ticket':
                settings['column_settings']={json.dumps(['name','Номер'],ensure_ascii=False):{'click_behavior':{'type':'link','linkType':'dashboard','targetId':state['dashboards']['ticket'],'parameterMapping':{'ticket_id':mapping('Номер','ticket_id','column')}}}}
            if c['key']=='employees':
                settings.setdefault('column_settings',{})
                for column,target in {'Закрыл за период':'closed_list','Назначено сейчас':'active_now_list','Ожидают клиента':'waiting_now_list','Ответ: выборка':'response_list','Возвраты после его закрытий':'returns_list','Передачи':'transfer_list'}.items():
                    click=click_dashboard(state['dashboards'][target],common)
                    click['parameterMapping']['employee']=mapping('ID сотрудника','employee','column')
                    settings['column_settings'].setdefault(json.dumps(['name',column],ensure_ascii=False),{}).update({'click_behavior':click})
            dc.append({'id':identifier,'card_id':cardid,'row':row,'col':col,'size_x':width,'size_y':c['height'],'dashboard_tab_id':tabids.get(tab),'series':[],'visualization_settings':settings,'parameter_mappings':[{'parameter_id':n,'card_id':cardid,'target':['variable',['template-tag',n]]} for n in names if n in available]})
            positions[tab]=(row,col+width,max(rowheight,c['height']))
        names=['ticket_id'] if key=='ticket' else common
        api.call('PUT',f'dashboard/{did}',{'name':spec['name'],'description':'Все даты — Asia/Irkutsk. Период: оба календарных дня включительно; пустые даты — текущий месяц. «Сейчас» не ограничивается периодом. Исторические показатели используют участников события; неизвестные сведения не заменяются текущими. В меню обновления выберите 1 минуту.','parameters':params(names,state['cards']),'tabs':[{'id':tabids[k],'name':name} for k,name in spec['tabs'].items()],'dashcards':dc})
        print(f'{spec["name"]}: {args.url.rstrip("/")}/dashboard/{did}#refresh=60')
    if args.set_homepage:
        api.call('PUT','setting/custom-homepage-dashboard',{'value':state['dashboards']['support']})
        api.call('PUT','setting/custom-homepage',{'value':True})
    if args.validate_queries:
        for c in cards:
            body={'parameters':[]}
            if 'ticket_id' in questions[c['key']]['dataset_query']['native']['template-tags']:
                body={'parameters':[{'id':questions[c['key']]['dataset_query']['native']['template-tags']['ticket_id']['id'],'type':'number/=','target':['variable',['template-tag','ticket_id']],'value':-1}]}
            result=api.call('POST',f"card/{state['cards'][c['key']]}/query",body)
            if result.get('status')=='failed': raise RuntimeError(f'Query failed: {c["key"]}; inspect Metabase logs')
        print(f'Validated {len(cards)} queries through Metabase')
if __name__=='__main__':
    try: main()
    except (RuntimeError,OSError,ValueError,KeyError) as e:
        print(str(e),file=sys.stderr);sys.exit(1)
