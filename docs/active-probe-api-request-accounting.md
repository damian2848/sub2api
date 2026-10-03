# 主动探测按真实 API 请求计入渠道监控：调查与改造方案

状态：已完成本地实现与回归验证，目标发布版本 v0.2.21；生产上线结果以部署回执为准。调查与实现日期：2026-10-03，Asia/Shanghai。

## 结论

主动探测的生成调用本来就是真实 API 请求。`source=probe` 应表示请求来源，不应表示“不是请求”。渠道总览、可用率、时间线、模型统计及上游消耗可以按真实调用统一统计，同时保留业务/探测来源；用户账单、扣费和用户排行必须使用各自明确的口径。

不再采用“只补一个独立探测可用率”的 UI 草案。先修正探测实际路由，再统一请求统计。不能把 `source` 改成 `business` 来绕过过滤，也不能把一次探测的汇总记录重复加入已经存在的网关用量日志。

## 本地实现落地

- 新迁移 264 为渠道 1m / rollup / 直方图 / 错误增加来源维度，并通过版本化水位触发有界历史重算；不改已发布迁移。
- 渠道默认口径为 business + probe，接口和页面均支持 all / business / probe；来源显示 business / probe / mixed。用户排行只包含业务请求，账单与扣费不因渠道重算而改变。
- 网关逐请求记录显式 `api_success`（true / false / NULL），免费成功与收费失败不再仅由金额判定；NULL 历史记录保守按可证实结果归类。
- 本站探测在创建、更新及运行时校验实际鉴权 Key 分组，包含 extra_headers 覆盖；允许停用旧的错误配置，但不允许它继续生成错误渠道的成功证据。
- 新迁移 265 与逐请求 recorder 保存内部文本探测的真实上游尝试。当前分组糖果链路、常用 HTTP / BPS / WSv2 生成、手工/定时账号文本检测、状态探测与判题均有记录。按实际角色区分；账号检查不投影为其所有分组都成功，判题不算被测渠道成功。
- 一次逻辑检测最多一个最终结果，各物理重试仍保存 Token / 成本。遗漏 usage / TTFT 保留 NULL 与完整性标记；从原始直方图合并延迟，不平均 P50。
- observation 的质量/额度/HEAD 结果仅是检测诊断，不再补造 API 可用率。合计 Token 取去重请求事实，不叠加同一次检测的任务 metering。
- 旧网关 source=probe 可以后台重算；旧内部任务只有汇总 metering，不能补造逐请求历史。内部账本从新版本实际执行时开始积累。

覆盖边界：原生 TypeSafe System One 以及手工图片/音频/视频测试尚无专门的内部账本 adapter；其正常网关 usage_logs 仍走统一渠道统计。未自动改生产 Key、部署或重启。

## v0.2.21 升级与回滚注意事项

- 新增迁移 264 / 265；已有迁移校验和不变。迁移 264 会改变六张渠道派生统计表的主键，使来源成为统计维度，因此必须先停止旧应用，再由新应用启动执行迁移；不要让旧聚合 worker 与新 schema 并行运行。
- 升级前保存完整数据库备份、原 Compose 配置、正在运行的二进制及 Prism 数据目录。保留现有环境变量、数据挂载与 Prism 运行时调优；不自动替换监控凭据。
- 回滚不能仅替换旧二进制：旧 worker 的 ON CONFLICT 不含 source。需要先停止新应用，恢复六张渠道派生表的旧唯一键并清空这些可重算的派生指标、重置水位，再启动原二进制。原始 usage_logs、ops_error_logs、客户账单和 probe_request_facts 必须保留。
- 如回滚恢复旧主键，应清除仅迁移 264 的版本记录，保证再次升级时它可以幂等重建新主键；不要修改已发布的 SQL 文件。迁移 265 的内部事实账本可以保留。

以下保留最初调查与方案，以便核对口径和线上配置问题。

## 一、线上只读检查发现

### 1. 已经存在完整网关请求，但聚合主动排除了它们

2026-10-03 12:21:53 +08:00，检查当时最近 90 分钟：

- `gpt-pro` 有 67 条 `usage_logs.source=probe`，67 个不同请求 ID。
- 这 67 条均有 `duration_ms`；有输入、输出及缓存 Token 数据。
- 这批探测没有 `first_token_ms`。当前 OpenAI 探测适配器默认 `stream=false`，不能把整次请求耗时当成首 Token 耗时。
- 三个相关分组的同一窗口没有查到 `ops_error_logs` 记录。这只描述该次查询窗口，不代表历史上从未失败。

代码路径：

- `backend/internal/service/channel_monitor_service.go` 的 `RunCheck`：对连通性探测设置来源签名上下文。
- `backend/internal/service/channel_monitor_probe_origin.go`：校验签名后标记 `source=probe`，保留签名、请求内容/鉴权绑定及防重放措施。
- `backend/internal/service/openai_gateway_usage.go`：正常网关调用写入真实请求 ID、账号、Key、分组、Token、耗时和计费字段。
- `backend/internal/repository/channel_monitor_v2_aggregation.go`：usage、histogram、error 聚合明确限定 `source='business'`。
- `backend/internal/repository/channel_monitor_v2_repo.go`：管理员错误详情也限定业务来源。
- `backend/internal/repository/channel_monitor_observations_integration_test.go`：`TestChannelMonitorBusinessAggregationExcludesProbes` 把“排除探测”锁定成现有测试契约。

因此这不是只缺一个前端展示字段，而是统计定义需要改变。

### 2. 两个连通性任务的展示分组与实际路由不一致

在服务器内存中解密并比较凭据，仅输出分组/Key ID，没有输出或保存明文 Key：

- 监控 `gpt-pro`：声明分组 3，使用本地 API Key 11，该 Key 实际绑定分组 3，匹配。
- 监控 `gpt-qy`：声明分组 24（gpt-企业专线），也使用 API Key 11；实际仍走分组 3，不匹配。
- 监控 `gpt-prism`：声明分组 43（prism），也使用 API Key 11；实际仍走分组 3，不匹配。

三个目标主机均为本站。当前监控的 `group_id` 进入 observation scope，却没有改变 Key 的实际调度分组。因此两个任务的“连通正常”不能证明企业专线或 prism 连通正常。

这项发现只针对上述三个连通性任务。糖果检测通过 `PelicanGroupTestService.runSample` 按配置的分组调度，是另一条执行路径，不能把同一问题直接套用到糖果检测。

## 二、目标统计契约

### 统一渠道请求视图

- 默认统计真实业务生成请求 + 真实主动探测生成请求，均进入已有时间窗口。
- 保留 `source=business/probe`，可按来源筛选、展示构成及定位问题。
- 请求成功率：实际生成调用成功数 / 实际生成调用完成数；失效 HTTP 状态、传输错误、终止流错误按同一分类规则处理。
- 按实际请求时间、真实被调度分组及实际账号归属，不根据监控卡片上的名称或声明分组重贴标签。
- 一次逻辑请求的最终状态、上游尝试次数分别统计。跨账号重试失败一次最终成功，不能同时成为一个渠道成功和一个最终失败；账号级尝试失败另行保留。
- 只有真实 LLM 生成调用进入生成请求数。HEAD ping、纯额度查询、未发起网络调用的跳过任务不进入此分母。
- 糖果/质量检测的“答错”和状态探针的“降智”属于质量结论，不自动等同于 API 调用失败。HTTP/传输失败也不能只因为质量结论为 inconclusive 就从请求失败数中消失。
- 不延伸一次成功探测填满未执行时段；历史时段无请求仍是未知。

### 性能和消耗

- Request count、RPM、实际 Token、缓存分子/分母及 duration 按统一请求视图汇总。
- TTFT 只使用实际采集到的首个有效输出时间。非流式请求缺失 TTFT 时保持 NULL，不使用 duration 替代。
- P50/P90 必须从原始样本或可合并的直方图重新计算，不能平均业务/探测两个 P50。
- 对缺失 usage/cost 的请求保留完整性标识，不能用 0 冒充已知消耗。
- 短探测的低缓存命中率是实际缓存指标，但不宜直接解释成“渠道不可用”；可用性和综合性能应明确区分。

### 账单、权限及来源

- 统计合并不触发新扣费、不重放已有账单。
- 通过真实 API Key 发起的现有网关探测继续遵守该 Key 的鉴权、限流、所属用户及既有计费规则。当前代码没有“只要 source=probe 就免单”的分支，不能从来源标记推导免单。
- 内部糖果/账号检测目前保存的是上游成本快照，不是客户扣费；不能为了写入 `usage_logs` 虚构一个用户/Key 并调用通用扣费函数。
- 用户使用记录/账单、业务用量面板和排行榜保持明确边界；渠道运营统计的“所有请求”不能被误命名为 `business_usage`。
- 不增加公开接口中的账号、Key、凭据、内部错误详情或未经授权的分组数据。

## 三、推荐分两阶段实现

### 阶段 0：先保证探测真实目标正确

1. 对本站 endpoint 的连通性任务，校验有效鉴权 Key 的真实分组与声明分组一致；校验必须考虑 `extra_headers` 对鉴权头的覆盖。
2. 创建/更新时给出明确错误；执行时再次校验，防止 Key 后续改绑。不能拿一把属于 gpt-pro 的 Key 证明另外两个渠道可用。
3. 为企业专线和 prism 选择各自分组的已有专用 Key，或经确认创建受限的监控 Key。不要默认共享管理员通用 Key，也不要为了“强制分组”绕过正常鉴权。
4. 增加目标类型：本站分组请求 / 外部 endpoint 请求 / 直接账号检测。外部结果不能仅凭展示绑定冒充本站分组端到端结果。
5. 未来结果保存实际分组、账号、关联请求 ID 和目标校验结论。现有错误绑定的记录保留审计信息，不能批量改成“企业专线/prism 的真实成功”。

### 阶段 1：现有连通性请求进入同一条渠道统计链路

这是对当前截图问题最小、最直接的修复：这类请求已经进入网关和用量日志，不需要再造请求。

1. 明确区分渠道统计和用户业务统计。在渠道级 usage/error/histogram 聚合中纳入 `business` 与 `probe`；用户业务/排行聚合保持业务口径，另有明确选项才展示全部。
2. 聚合层保留来源维度，使“所有/业务/探测”均能计算，而不是只删除一个 WHERE 条件后永久丢失来源。
3. 统一修改错误指标和错误详情；不能只扩大成功请求来源，让探测失败仍被 SQL 排除。
4. 更新 `availability_source` 的语义。混合来源应能表达 `mixed`，不把混合请求标成 business；已有主动探测补充可用率不再重复计入已经纳入的网关调用。
5. 前端现有的 18 段时间线读取统一 buckets。无业务流量但存在真实探测请求时，缓存/耗时/可用率使用已观测字段；未观测的 TTFT 仍为空。
6. 聚合统计版本变更后安排有界重算。当前 aggregator 常规只回看最近 10 分钟，单改 SQL 不会自动修复所有已有 90m/24h/7d/30d rollup。先重算最近 90 分钟及 24 小时，再分块重算更早保留区间，避免在线全表扫描。

主要改动范围：

- `backend/internal/repository/channel_monitor_v2_aggregation.go`
- `backend/internal/repository/channel_monitor_v2_repo.go`
- `backend/internal/service/channel_monitor_v2.go`
- `backend/internal/service/channel_monitor_v2_probe_availability.go`
- `backend/internal/service/channel_monitor_v2_aggregator.go`
- 新迁移：来源维度/聚合版本（不改已发布迁移）
- `frontend/src/api/channelMonitorV2.ts`
- `frontend/src/features/channel-monitor-v2/monitorCards.ts`
- `frontend/src/features/channel-monitor-v2/MonitorStatusCards.vue`
- 模型/详细趋势/翻译及测试同步修改

### 阶段 2：内部糖果/状态/质量检测的实际调用也统一记账

这些测试真实请求上游，但当前大多直接走账号测试路径，只有探测结果和 metering 汇总；不能把阶段 1 的 SQL 改动误当成已经覆盖它们。

推荐新增不与客户扣费耦合的、逐请求 `probe_request_facts`，通过标准化渠道请求视图与已有网关日志联合聚合：

- 保存稳定的 run/request/attempt ID、开始/结束时间、协议、请求模型/上游模型、实际组/账号、HTTP/传输结果、真实 TTFT、Token、上游成本及完整性。
- `pelicanTestUsageCollector` 从只有 Token 累积的 collector 扩展为逐调用观测。在发出请求前登记，在 HTTP 失败/超时/解析失败时也结束记录；现有 collector 的请求数不能直接当成所有网络尝试的完整计数。
- 本站 HTTP 探测关联已有网关 request ID，只把网关日志作为统计事实；monitor history 只用于质量/任务详情，不再重复加一次 metering。
- 独立判题模型调用标为 `quality_judge`，消耗归实际判题账号，不能记成被测模型的渠道成功或被测渠道 Token。
- 多次门票探针、跨账号重试分别保留物理尝试；按逻辑探测任务的最终 API 结果计算渠道可用率，另行统计实际上游请求数和成本。
- 直接账号检测不是完整分组路由测试，不应投影为账号所属的每一个分组都获得一次端到端成功。
- 不向现有 `usage_logs` 插入伪造 user_id/api_key_id；其 schema 要求真实用户、Key 和账号关联，写入流程部分与扣费耦合。

主要接入点：`channel_monitor_checker.go`、`pelican_group_tests.go`、`pelican_test_usage.go`、`pelican_scheduled.go`、`account_test_service.go` 的 HTTP/SSE 路径，以及状态/质量测试的请求执行位置。

## 四、必须覆盖的回归验证

- 本站 Key/声明分组匹配可以执行；不匹配在保存/执行时报错，不能给错误渠道增加成功样本。
- 无业务请求，只有探测：出现真实时间线、请求数、Token 和 duration；缺失 TTFT 保持 NULL。
- 业务与探测混合：成功率按真实计数加权，P50 从合并样本计算；来源筛选一致。
- 探测 HTTP 429/5xx、超时、终止流缺失进入正确失败分类，错误列表与概要分母一致。
- 返回有效回答但糖果答错：API 成功仍是成功，质量结果单独异常。
- 同一本站探测的 usage log + monitor history + metering 只计一次。
- 重试两个账号后成功：逻辑请求成功一次，上游尝试/成本计两次，不重复扣费。
- 判题调用、HEAD、quota 查询、未执行/跳过、未知 usage 均不被当成被测渠道成功。
- 停用任务后旧的真实 API 请求保留在历史窗口，而“当前正在探测”的状态改变；不因停用而抹掉真实历史请求。
- 新聚合版本分块回填，重复回填幂等，旧 rollup 与当前数据不混用旧统计语义。
- 用户无分组权限时不泄漏该分组样本；用户账单、扣费和排行榜不被运营统计重算改写。

## 五、本次操作边界

只读检查了生产配置、请求日志与 Key 分组归属，没有触发手动生成、修改线上 Key、修复线上配置或重启服务。

此前独立探测展示的本地草案已经撤回，不作为最终改造。应先确认真实目标，再按上述统一请求契约实现与部署。
