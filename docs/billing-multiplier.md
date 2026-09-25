# 计费倍率与历史账单换算

## 计费口径

在管理中心「插件管理 → CPA Key Billing → 配置」设置 `billing_multiplier`，也可合并到已有 YAML：

```yaml
plugins:
  configs:
    cpa-key-billing:
      billing_multiplier: 0.2
      codex_fast_mode_billing: true
```

全局倍率默认 `1`，必须是有限正数。Codex Fast 计费默认开启，显式 `false` 可关闭。Codex OAuth Priority/Fast 按本站策略计费：GPT-5.6/GPT-6 系列乘 `2`，其他模型乘 `2.5`。响应档位优先，响应为 Default/Standard 时乘 `1`；缺失时按请求估算。GPT-5.6/GPT-6 系列请求 Priority/Fast、响应档位未知时，按请求档位估算并乘 `2`，记录 `unknown_response_tier_request_fallback`；其他未知档位仍按标准档估算。关闭开关只关闭 Priority 加价，Flex 仍为 `0.5`。

模型优先取 `ResponseModel`、执行模型，再取计费模型。去除前缀和思考后缀、忽略大小写后，匹配 `gpt-5.6`、`gpt-5.6-*`、`gpt-6`、`gpt-6-*`；`gpt-60`、`gpt-5.60` 等相似名称不匹配。基础单价的选择规则保持原有行为。

### 上游 API Key 服务档位

API Key 指宿主上报的上游鉴权方式，不是调用代理的下游 Key。OpenAI/Codex API Key、OpenAI 兼容执行器采用以下顺序：

1. 优先使用响应档位；`priority`、`fast` 等价，`default` 表示标准价，包括被上游降级的请求。
2. 缺少响应档位时按请求档位估算，`auto` 或缺省按标准档；未知响应/请求档位也按标准档估算。
3. 模型显式 `service_tiers.priority` / `service_tiers.flex` 配置优先；否则 Priority 按已核实的模型分项价比调整当前基础价，Flex 默认 `0.5×`。
4. 缺少适用 Priority 价格（包括尚未核实的上下文区间或实际使用了无官方价格的缓存分项）时，整笔使用基础价并标记估算。基础价也缺失时，保留用量并沿用原有缺价零金额行为。
5. 按选中的输入、输出、缓存和长上下文价格计算，再应用全局倍率一次。现有基础价及其长上下文配置不被官方规则覆盖。

**宿主兼容性：** 当前服务器 CLIProxyAPI v7.3.17 已在 `usage.handle` 中提供 `ResponseServiceTier` 和 `ResponseModel`。上游缺失响应档位或使用 v7.3.8 等旧宿主时，按请求档位估算并标记 `missing_response_tier`，无法识别实际降级。插件不读取原始响应或关联并发请求补齐字段。

已核对最新发布版 [CLIProxyAPI v7.3.17](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v7.3.17)：其插件 SDK 和 `usage.handle` 已提供 `ResponseServiceTier`、`ResponseModel`。现有插件可直接读取这些字段；仅在上游未上报档位时继续按请求估算。

已核实规则来自 [OpenAI 价格表](https://developers.openai.com/api/docs/pricing) 与 [Fast 指南](https://developers.openai.com/api/docs/guides/fast-mode)，版本 `openai-2026-09-25`。包括 GPT-6、GPT-5.6、GPT-5.5、GPT-5.4、GPT-5.2/5.1/5、GPT-4.1/4o、o3/o4-mini 中列出的支持型号；精确名单及上下文限制见 `internal/billing/service_tiers.go`。不对未知型号、未知快照或未核实的长上下文价格外推。此功能不覆盖区域附加费、工具调用费等宿主用量无法完整表达的费用。

新 API 账单的 `cost.pricing` 保存 `service_tier`、`tier_source`、`method`、`rule_version`、`tier_fallback`、`price_fallback`；实际应用单价与金额直接持久化。档位价已计入单价，因此额外 `service_tier_multiplier=1`，`cost.multiplier` 仍是全局倍率与额外档位倍率的乘积，不代表相对标准价的总加价。新 OAuth 账单也保存 `cost.pricing`，其中 `method=oauth_multiplier`、`rule_version=codex-oauth-family-v2`；未知响应回退到请求 Priority 时，`service_tier=priority`、`tier_source=request`，保留估算标记。实际档位倍率写入 `service_tier_multiplier`，`multiplier` 是全局倍率与档位倍率的乘积。历史账单保留原金额和倍率，不补写推测的定价依据。页面和 CSV 直接使用入账金额。

### 配置档位价格

现有 `PUT /prices` 接口支持可选 `service_tiers`；旧客户端省略它会保留原档位设置，传 `{}` 清除全部档位设置，`null` 无效。每个档位的输入/输出单价必填，零值有效；缓存单价留空继承该档输入价；可选 `long_context` 是该档独立的长上下文价卡。

```json
{
  "model_id": "gpt-6-sol",
  "input_per_1m": 2,
  "output_per_1m": 10,
  "cache_read_per_1m": 0.2,
  "cache_write_per_1m": 2.5,
  "service_tiers": {
    "priority": {
      "input_per_1m": 4,
      "output_per_1m": 20,
      "cache_read_per_1m": 0.4,
      "cache_write_per_1m": 5
    }
  }
}
```

价格查询中的 `service_tier_prices` 提供当前生效价卡、定价方式及已核实的输入长度上限，`service_tiers` 仅包含显式设置。配置在 `usage.handle` 入账时生效；金额额度扣减一致，Token、请求次数及套餐上限不变。

以后从 `0.2` 改成 `0.3` 只影响新入账记录，历史金额不变。历史 Fast 状态也不会被新的默认开关追溯改变。

## 数据库结构升级

这里的 v17/v18/v19 是插件记录在 `PRAGMA user_version` 中的结构版本，不是 SQLite 软件版本。打开旧数据库时，插件会事务化升级结构：新增倍率字段及换算审计表，将旧普通记录补为全局 `1`、Fast `1`，将旧 `:x2.5` 标记补为全局 `1`、Fast `2.5`。此过程不改变历史金额。

v19 新增档位价格和账单定价元数据，v18 及已有受支持旧版本按事务迁移。历史金额、失败记录、零费用记录均保留，历史记录的定价元数据保持为空。迁移失败会回滚；配置写入失败不会发布新内存价格。

升级前备份数据库。在 CPA 完全停止后复制数据库以及仍存在的 `-wal`、`-shm` 文件，或使用 SQLite 一致快照接口；不要在运行中只复制主 `.db` 文件。备份目录应限制为当前用户可访问，并位于插件扫描目录之外。

## 一次性将历史金额换算为 0.2

维护工具与插件一起从相同提交构建：

```bash
go build -o dist/billing-maintenance ./cmd/billing-maintenance
```

先在受限目录中的数据库副本上演练。预览使用一致的临时副本计算结果，不修改原数据库的结构或数据：

```bash
dist/billing-maintenance \
  --db /absolute/path/cpa-key-billing-state-v1.db \
  --operation-id initial-global-multiplier-0.2 \
  --multiplier 0.2 \
  --dry-run
```

确认 CPA 已完全停止并完成配置、插件、数据库备份后，使用相同参数执行：

```bash
dist/billing-maintenance \
  --db /absolute/path/cpa-key-billing-state-v1.db \
  --operation-id initial-global-multiplier-0.2 \
  --multiplier 0.2
```

工具在一个事务中将全部历史请求的金额和实际应用单价乘以 `0.2`，并将全部 Key（包括已删除 Key）的周期 `spent_usd` 乘以 `0.2`。它保留历史 Fast 倍率、记录 ID、失败详情、零费用记录、时间、Token、请求数、套餐限额和价格表。旧日志是审计文本，不改写其中的历史金额。

周期消费直接从已有快照换算，不从请求明细重新求和；明细可能已按保留期清理。损坏数据、非法数值或混有已经应用其他全局倍率的历史记录会使首次换算失败并回滚。

操作 ID 与参数、影响数量和前后总金额一起保存。同一 ID、相同参数再次运行会返回原结果；同一 ID、不同参数会失败。不要更换 ID 重复进行折扣。

换算成功后，确保线上配置同时设置 `billing_multiplier: 0.2`，再启动 CPA。新账单延续相同口径，未来配置热更新不会自动触发历史换算。

## 验收与恢复

核对数据库完整性、记录数量、原有金额乘以 `0.2` 后的总计及各周期已消费金额。检查管理员、用户页面和 CSV 的金额与倍率一致，并确认重启后倍率仍可读取。

只支持 v18 的旧插件不能读取 v19 数据库。接受生产请求前若验收失败，停止候选实例并恢复完整备份。已经产生新账单后应保留现有数据库修复，不能只换回旧 `.so`，也不能恢复旧快照丢弃新账单。

跨平台比较使用同一周期和币种的金额：CPA 已倍率费用对应 sub2api 的 `actual_cost`。统一倍率并不会统一两个平台的模型基价或 Token 计量规则；金额仍为 USD，倍率不是人民币汇率。
