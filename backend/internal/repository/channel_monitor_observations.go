package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func marshalMonitorMetadata(value any) any {
	data, err := json.Marshal(value)
	if err != nil || string(data) == "null" {
		return nil
	}
	return string(data)
}

func (r *channelMonitorV2Repository) ListMonitorObservationTargets(ctx context.Context, filter service.ChannelMonitorV2Filter) ([]service.ChannelMonitorObservation, error) {
	rows, err := r.db.QueryContext(ctx, `WITH history AS (
 SELECT observation_scope AS scope,model FROM channel_monitor_histories WHERE checked_at >= $1 AND checked_at < $2 AND observation_scope IS NOT NULL
 UNION ALL SELECT observation_scope,model FROM channel_monitor_quality_observations WHERE checked_at >= $1 AND checked_at < $2 AND observation_scope IS NOT NULL
 UNION ALL SELECT observation_scope,model FROM channel_monitor_state_probe_results WHERE checked_at >= $1 AND checked_at < $2
 UNION ALL SELECT observation_scope,COALESCE(pelican_config->>'model_id','') FROM scheduled_test_results WHERE started_at >= $1 AND started_at < $2 AND observation_scope IS NOT NULL AND (jsonb_typeof(pelican_config->'quality')='object' OR pelican_config->>'question_kind'='state_probe')
 ) SELECT m.provider,models.model,m.group_id,COALESCE(g.name,'') FROM channel_monitors m LEFT JOIN groups g ON g.id=m.group_id CROSS JOIN LATERAL jsonb_array_elements_text(jsonb_build_array(m.primary_model)||m.extra_models) AS models(model)
 UNION SELECT g.platform,p->>'model',g.id,g.name FROM channel_monitor_v2_config cfg CROSS JOIN LATERAL jsonb_array_elements(cfg.candy_probes) p JOIN groups g ON g.id=(p->>'group_id')::bigint
 UNION SELECT a.platform,p.model_id,g.id,COALESCE(g.name,'') FROM scheduled_test_plans p JOIN accounts a ON a.id=p.account_id LEFT JOIN account_groups ag ON ag.account_id=a.id LEFT JOIN groups g ON g.id=ag.group_id WHERE a.deleted_at IS NULL AND (jsonb_typeof(p.pelican_config->'quality')='object' OR p.pelican_config->>'question_kind'='state_probe')
 UNION SELECT h.scope->>'platform',h.model,ids.value::bigint,COALESCE(h.scope->'group_names'->>ids.value,'') FROM history h LEFT JOIN LATERAL jsonb_array_elements_text(COALESCE(h.scope->'group_ids','[]'::jsonb)) ids(value) ON true
 UNION SELECT COALESCE(NULLIF(c.platform,''),NULLIF(g.platform,'composite'),'unknown'),c.model,g.id,g.name FROM channel_monitor_v2_candy_results c JOIN groups g ON g.id=c.group_id WHERE c.checked_at >= $1 AND c.checked_at < $2`, filter.Start, filter.End)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []service.ChannelMonitorObservation{}
	for rows.Next() {
		var item service.ChannelMonitorObservation
		if err := rows.Scan(&item.Platform, &item.Model, &item.GroupID, &item.GroupName); err != nil {
			return nil, err
		}
		item.Scope = service.ChannelMonitorObservationScope{Platform: item.Platform, GroupIDs: []int64{}, GroupNames: map[int64]string{}}
		if item.GroupID != nil {
			item.Scope.GroupIDs = []int64{*item.GroupID}
			item.Scope.GroupNames[*item.GroupID] = item.GroupName
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *channelMonitorV2Repository) RecordStateProbe(ctx context.Context, result *service.OpenAICodexStateProbeResult) error {
	if result == nil || result.Scope == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO channel_monitor_state_probe_results(model,verdict,checked_at,latency_ms,reason,metering,observation_scope) VALUES($1,$2,$3,$4,$5,$6,$7)`, result.Model, result.Verdict, result.StartedAt, result.LatencyMs, result.Reason, marshalMonitorMetadata(result.Usage), marshalMonitorMetadata(result.Scope))
	return err
}

func (r *channelMonitorV2Repository) ListMonitorObservations(ctx context.Context, filter service.ChannelMonitorV2Filter) ([]service.ChannelMonitorObservation, error) {
	rows, err := r.db.QueryContext(ctx, channelMonitorObservationsSQL, filter.Start, filter.End, pq.Array(filter.Platforms), pq.Array(filter.GroupIDs), filter.RestrictGroups, pq.Array(filter.AllowedGroupIDs))
	if err != nil {
		return nil, fmt.Errorf("query monitor observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := []service.ChannelMonitorObservation{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var item service.ChannelMonitorObservation
		if err := json.Unmarshal(payload, &item); err != nil {
			return nil, err
		}
		var metadata struct {
			Scope service.ChannelMonitorObservationScope `json:"scope"`
		}
		if err := json.Unmarshal(payload, &metadata); err != nil {
			return nil, err
		}
		item.Scope = metadata.Scope
		items = append(items, item)
	}
	return items, rows.Err()
}

// Aggregate the full window before capping display history, so 100 visible
// points never become the denominator or the consumption total.
const channelMonitorObservationsSQL = `WITH observations AS (
 SELECT 'v1:'||h.id::text AS record_id,'v1:'||m.id::text AS target_id,
 CASE WHEN COALESCE(h.observation_scope->>'check_mode',CASE WHEN h.quota IS NOT NULL AND h.latency_ms IS NULL THEN 'quota' ELSE m.check_mode END)='quota' THEN 'quota' ELSE 'connectivity' END AS type,
 m.name,COALESCE(h.observation_scope->>'platform',m.provider) AS platform,h.model,
 COALESCE(h.observation_scope,jsonb_build_object('platform',m.provider,'group_ids',CASE WHEN m.group_id IS NULL THEN '[]'::jsonb ELSE jsonb_build_array(m.group_id) END,'group_names',CASE WHEN m.group_id IS NULL THEN '{}'::jsonb ELSE jsonb_build_object(m.group_id::text,COALESCE(g.name,m.group_name)) END)) AS scope,
 m.enabled,m.interval_seconds,''::text AS schedule,h.status::text AS verdict,h.checked_at,h.latency_ms::bigint,h.message,h.metering
 FROM channel_monitor_histories h JOIN channel_monitors m ON m.id=h.monitor_id LEFT JOIN groups g ON g.id=m.group_id
 WHERE h.checked_at >= $1 AND h.checked_at < $2
 UNION ALL
 SELECT NULL,'v1:'||m.id::text,CASE WHEN m.check_mode='quota' THEN 'quota' ELSE 'connectivity' END,
 m.name,m.provider,models.model,
 jsonb_build_object('platform',m.provider,'group_ids',CASE WHEN m.group_id IS NULL THEN '[]'::jsonb ELSE jsonb_build_array(m.group_id) END,'group_names',CASE WHEN m.group_id IS NULL THEN '{}'::jsonb ELSE jsonb_build_object(m.group_id::text,COALESCE(g.name,m.group_name)) END),
 m.enabled,m.interval_seconds,'','unknown',NULL,NULL,'',NULL
 FROM channel_monitors m LEFT JOIN groups g ON g.id=m.group_id
 CROSS JOIN LATERAL jsonb_array_elements_text(jsonb_build_array(m.primary_model)||m.extra_models) AS models(model)
 WHERE NOT EXISTS(SELECT 1 FROM channel_monitor_histories h WHERE h.monitor_id=m.id AND h.model=models.model AND h.checked_at >= $1 AND h.checked_at < $2
 AND COALESCE(h.observation_scope->>'platform',m.provider)=m.provider
 AND COALESCE(h.observation_scope->'group_ids',CASE WHEN m.group_id IS NULL THEN '[]'::jsonb ELSE jsonb_build_array(m.group_id) END)=CASE WHEN m.group_id IS NULL THEN '[]'::jsonb ELSE jsonb_build_array(m.group_id) END)
 UNION ALL
 SELECT 'candy:'||c.id::text,'candy:'||c.group_id::text||':'||c.config_key,'candy','candy',
 COALESCE(NULLIF(c.platform,''),NULLIF(g.platform,'composite'),'unknown'),c.model,
 jsonb_build_object('platform',COALESCE(NULLIF(c.platform,''),g.platform),'group_ids',jsonb_build_array(c.group_id),'group_names',jsonb_build_object(c.group_id::text,g.name)),
 COALESCE((probe.config->>'enabled')::boolean,false),COALESCE((probe.config->>'interval_minutes')::int,0)*60,'',c.verdict,c.checked_at,c.latency_ms,c.reason,c.metering
 FROM channel_monitor_v2_candy_results c JOIN groups g ON g.id=c.group_id
 LEFT JOIN channel_monitor_v2_config cfg ON cfg.id=1
 LEFT JOIN LATERAL (SELECT p AS config FROM jsonb_array_elements(cfg.candy_probes) p WHERE (p->>'group_id')::bigint=c.group_id AND c.config_key=encode(sha256(convert_to('candy-v1:'||(p->>'model')||':'||(p->>'reasoning_effort')||':'||(p->>'interval_minutes'),'UTF8')),'hex') LIMIT 1) probe ON true
 WHERE c.checked_at >= $1 AND c.checked_at < $2 AND c.verdict<>'running'
 UNION ALL
 SELECT NULL,'candy:'||g.id::text||':'||encode(sha256(convert_to('candy-v1:'||(p->>'model')||':'||(p->>'reasoning_effort')||':'||(p->>'interval_minutes'),'UTF8')),'hex'),
 'candy','candy',g.platform,p->>'model',jsonb_build_object('platform',g.platform,'group_ids',jsonb_build_array(g.id),'group_names',jsonb_build_object(g.id::text,g.name)),
 COALESCE((p->>'enabled')::boolean,false),COALESCE((p->>'interval_minutes')::int,0)*60,'','unknown',NULL,NULL,'',NULL
 FROM channel_monitor_v2_config cfg CROSS JOIN LATERAL jsonb_array_elements(cfg.candy_probes) p JOIN groups g ON g.id=(p->>'group_id')::bigint
 WHERE NOT EXISTS(SELECT 1 FROM channel_monitor_v2_candy_results c WHERE c.group_id=g.id AND c.config_key=encode(sha256(convert_to('candy-v1:'||(p->>'model')||':'||(p->>'reasoning_effort')||':'||(p->>'interval_minutes'),'UTF8')),'hex') AND c.checked_at >= $1 AND c.checked_at < $2 AND c.verdict<>'running')
 UNION ALL
 SELECT 'quality:'||r.id::text,'quality:'||p.id::text,
 CASE WHEN r.pelican_config->>'question_kind'='state_probe' THEN 'state_probe' ELSE 'quality' END,
 COALESCE(r.observation_scope->>'name',a.name),COALESCE(r.observation_scope->>'platform',a.platform),COALESCE(NULLIF(r.pelican_config->>'model_id',''),p.model_id),
 COALESCE(r.observation_scope,jsonb_build_object('platform',a.platform,'group_ids','[]'::jsonb)),p.enabled,0,p.cron_expression,
 CASE WHEN r.pelican_config->>'question_kind'='state_probe' THEN CASE WHEN r.status='success' THEN 'healthy' WHEN r.error_message='state_degraded' THEN 'degraded' ELSE 'inconclusive' END
 WHEN r.quality_judgment->>'verdict'='correct' AND r.status='success' THEN 'correct'
 WHEN r.quality_judgment->>'verdict'='incorrect' AND r.error_message='answer_mismatch' THEN 'incorrect' ELSE 'inconclusive' END,
 r.started_at,r.latency_ms,r.error_message,r.metering
 FROM scheduled_test_results r JOIN scheduled_test_plans p ON p.id=r.plan_id JOIN accounts a ON a.id=p.account_id
 WHERE r.started_at >= $1 AND r.started_at < $2 AND (jsonb_typeof(r.pelican_config->'quality')='object' OR r.pelican_config->>'question_kind'='state_probe')
 AND NOT EXISTS(SELECT 1 FROM channel_monitor_quality_observations archive WHERE archive.result_id=r.id)
 UNION ALL
 SELECT 'quality:'||r.result_id::text,'quality:'||r.plan_id::text,r.type,COALESCE(r.observation_scope->>'name',r.type),COALESCE(r.observation_scope->>'platform','unknown'),r.model,COALESCE(r.observation_scope,'{}'::jsonb),COALESCE(p.enabled,false),0,r.schedule,r.verdict,r.checked_at,r.latency_ms,r.message,r.metering
 FROM channel_monitor_quality_observations r LEFT JOIN scheduled_test_plans p ON p.id=r.plan_id WHERE r.checked_at >= $1 AND r.checked_at < $2
 UNION ALL
 SELECT NULL,'quality:'||p.id::text,CASE WHEN p.pelican_config->>'question_kind'='state_probe' THEN 'state_probe' ELSE 'quality' END,
 a.name,a.platform,p.model_id,jsonb_build_object('platform',a.platform,'account_id',a.id,'name',a.name,'group_ids',COALESCE((SELECT jsonb_agg(ag.group_id ORDER BY ag.group_id) FROM account_groups ag WHERE ag.account_id=a.id),'[]'::jsonb),'group_names',COALESCE((SELECT jsonb_object_agg(ag.group_id::text,g.name) FROM account_groups ag JOIN groups g ON g.id=ag.group_id WHERE ag.account_id=a.id),'{}'::jsonb)),
 p.enabled,0,p.cron_expression,'unknown',NULL,NULL,'',NULL
 FROM scheduled_test_plans p JOIN accounts a ON a.id=p.account_id WHERE a.deleted_at IS NULL AND (jsonb_typeof(p.pelican_config->'quality')='object' OR p.pelican_config->>'question_kind'='state_probe')
 AND NOT EXISTS(SELECT 1 FROM scheduled_test_results r WHERE r.plan_id=p.id AND COALESCE(r.pelican_config->>'model_id',p.model_id)=p.model_id AND r.started_at >= $1 AND r.started_at < $2
 AND COALESCE(r.observation_scope->'group_ids','[]'::jsonb)=COALESCE((SELECT jsonb_agg(ag.group_id ORDER BY ag.group_id) FROM account_groups ag WHERE ag.account_id=a.id),'[]'::jsonb))
 AND NOT EXISTS(SELECT 1 FROM channel_monitor_quality_observations r WHERE r.plan_id=p.id AND r.model=p.model_id AND r.checked_at >= $1 AND r.checked_at < $2
 AND COALESCE(r.observation_scope->'group_ids','[]'::jsonb)=COALESCE((SELECT jsonb_agg(ag.group_id ORDER BY ag.group_id) FROM account_groups ag WHERE ag.account_id=a.id),'[]'::jsonb))
 UNION ALL
 SELECT 'manual:'||r.id::text,'manual:'||COALESCE(r.observation_scope->>'account_id','unknown'),
 'state_probe',COALESCE(r.observation_scope->>'name','state_probe'),r.observation_scope->>'platform',r.model,r.observation_scope,false,0,'',r.verdict,r.checked_at,r.latency_ms,r.reason,r.metering
 FROM channel_monitor_state_probe_results r WHERE r.checked_at >= $1 AND r.checked_at < $2
), ranked AS (
 SELECT *,md5(scope::text) AS scope_key,
 row_number() OVER(PARTITION BY target_id,type,platform,model,scope ORDER BY checked_at DESC NULLS LAST,record_id DESC) AS history_index
 FROM observations
 WHERE (COALESCE(cardinality($3::text[]),0)=0 OR platform=ANY($3))
 AND (COALESCE(cardinality($4::bigint[]),0)=0 OR EXISTS(SELECT 1 FROM jsonb_array_elements_text(COALESCE(scope->'group_ids','[]'::jsonb)) id WHERE id::bigint=ANY($4)))
 AND (NOT $5 OR EXISTS(SELECT 1 FROM jsonb_array_elements_text(COALESCE(scope->'group_ids','[]'::jsonb)) id WHERE id::bigint=ANY($6::bigint[])))
)
SELECT jsonb_build_object(
 'id',target_id||':'||md5(jsonb_build_array(type,platform,model,scope)::text),'type',type,'name',max(name),'platform',platform,'model',model,'scope',scope,
 'enabled',bool_or(enabled),'interval_seconds',max(interval_seconds),'schedule',max(schedule),
 'verdict',(array_agg(verdict ORDER BY checked_at DESC NULLS LAST))[1],
 'checked_at',max(checked_at),'latency_ms',(array_agg(latency_ms ORDER BY checked_at DESC NULLS LAST))[1],
 'sample_count',count(record_id),
 'passed_count',count(record_id) FILTER(WHERE verdict IN ('operational','degraded') AND type='connectivity' OR verdict='operational' AND type='quota' OR verdict IN ('correct','healthy')),
 'failed_count',count(record_id) FILTER(WHERE verdict IN ('failed','error') AND type='connectivity' OR verdict IN ('failed','error','degraded') AND type='quota' OR verdict='incorrect' OR verdict='degraded' AND type='state_probe'),
 'inconclusive_count',count(record_id) FILTER(WHERE verdict IN ('inconclusive','unknown') OR verdict='error' AND type IN ('candy','quality','state_probe')),
 'usage',jsonb_build_object('source','probe',
 'request_count',COALESCE(sum((metering->>'request_count')::bigint),0),
 'input_tokens',COALESCE(sum((metering->>'input_tokens')::bigint),0),
 'output_tokens',COALESCE(sum((metering->>'output_tokens')::bigint),0),
 'cache_read_tokens',COALESCE(sum((metering->>'cache_read_tokens')::bigint),0),
 'cache_creation_tokens',COALESCE(sum((metering->>'cache_creation_tokens')::bigint),0),
 'cache_creation_5m_tokens',COALESCE(sum((metering->>'cache_creation_5m_tokens')::bigint),0),
 'cache_creation_1h_tokens',COALESCE(sum((metering->>'cache_creation_1h_tokens')::bigint),0),
 'reasoning_tokens',COALESCE(sum((metering->>'reasoning_tokens')::bigint),0),
 'cost_usd',sum((metering->>'cost_usd')::numeric),
 'cost_incomplete',COALESCE(bool_or(metering IS NULL OR COALESCE((metering->>'cost_incomplete')::boolean,true)) FILTER(WHERE record_id IS NOT NULL),false),
 'usage_incomplete',COALESCE(bool_or(metering IS NULL OR COALESCE((metering->>'usage_incomplete')::boolean,true)) FILTER(WHERE record_id IS NOT NULL),false)),
 'history',COALESCE(jsonb_agg(jsonb_build_object('checked_at',checked_at,'verdict',verdict,'latency_ms',latency_ms,'message',message) ORDER BY checked_at DESC) FILTER(WHERE record_id IS NOT NULL AND history_index<=100),'[]'::jsonb)
) FROM ranked GROUP BY target_id,type,platform,model,scope ORDER BY platform,model,target_id`

func (r *channelMonitorV2Repository) pruneMonitorObservationHistory(ctx context.Context, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM channel_monitor_state_probe_results WHERE id IN(SELECT id FROM channel_monitor_state_probe_results WHERE checked_at<$1 LIMIT 10000)`, now.Add(-30*24*time.Hour))
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `DELETE FROM channel_monitor_quality_observations WHERE result_id IN(SELECT result_id FROM channel_monitor_quality_observations WHERE checked_at<$1 LIMIT 10000)`, now.Add(-30*24*time.Hour))
	return err
}
