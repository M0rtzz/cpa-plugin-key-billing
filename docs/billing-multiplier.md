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

全局倍率默认 `1`，必须是有限正数。Fast 计费默认开启，显式 `false` 可关闭。基础费用仍使用插件现有模型定价、缓存和长上下文规则；符合现有条件的 Codex OAuth priority 请求再乘 `2.5`。因此全局 `0.2` 对普通请求产生 `0.2×`，对符合条件的 Fast 请求产生 `0.5×`。

倍率在 `usage.handle` 入账时生效。费用组成、实际应用单价、总金额和套餐金额扣除保持一致；Token、请求数、套餐上限和价格表本身不变。每笔账单保存全局倍率和 Fast 倍率，`cost.multiplier` 是两者的乘积。管理员与用户页面显示当前设置及账单当时的设置。前端和 CSV 直接使用已经计费的金额。

以后从 `0.2` 改成 `0.3` 只影响新入账记录，历史金额不变。历史 Fast 状态也不会被新的默认开关追溯改变。

## 数据库结构升级

这里的 v14/v15 是插件记录在 `PRAGMA user_version` 中的结构版本，不是 SQLite 软件版本。打开旧数据库时，插件会事务化升级结构：新增倍率字段及换算审计表，将旧普通记录补为全局 `1`、Fast `1`，将旧 `:x2.5` 标记补为全局 `1`、Fast `2.5`。此过程不改变历史金额。

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

旧插件不能读取 v15 数据库。接受生产请求前若验收失败，停止候选实例并恢复完整备份。已经产生新账单后应保留现有数据库修复，不能只换回旧 `.so`，也不能恢复旧快照丢弃新账单。

跨平台比较使用同一周期和币种的金额：CPA 已倍率费用对应 sub2api 的 `actual_cost`。统一倍率并不会统一两个平台的模型基价或 Token 计量规则；金额仍为 USD，倍率不是人民币汇率。
