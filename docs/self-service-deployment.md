# 普通用户页面部署与回滚

本功能包含两个独立页面：`usage.html` 查询本人 Key 的用量和费用，`quota.html` 展示本人 Key 额度和可见号池明细。Plus、Pro 20x 等账号类型复用计费插件已有的配额结果。

页面不需要管理密码。首次登录时，插件验证用户自己的 CPA API Key，并在服务器内存中建立最长 7 天的随机会话；浏览器只接收 `HttpOnly`、`SameSite=Strict` Cookie，不把 API Key 写入浏览器存储。刷新页面、重新进入以及在两个页面间切换时会复用会话；主动退出、API Key 失效、会话到期或 CPA/插件重启后需要重新输入。每个会话请求仍会向 CPA 验证当前 Key，因此撤销立即生效。正式用户入口应通过 HTTPS 提供。

主题提供“系统 / 暗色 / 亮色”三个选项，默认跟随系统。主题偏好保存在浏览器 `localStorage` 中并由两个页面共享；其中只包含显示偏好，不包含 API Key 或登录令牌。

## 构建与交付

在本仓库使用 Go 1.26、C 编译器和 Python 3：

```sh
bash scripts/build-self-service.sh
```

也可以显式指定 Go 可执行文件：

```sh
GO=/absolute/path/to/go bash scripts/build-self-service.sh
```

Linux 产物为 `dist/cpa-key-billing.so`、`dist/usage.html`、`dist/quota.html` 和 `dist/SHA256SUMS`。HTML 与插件嵌入的页面完全一致，不需要前端构建工具或 CDN。

共享库必须与服务器的操作系统、架构及 libc 兼容。异机部署时，在兼容的 Linux 环境构建；可复用仓库发布工作流的 manylinux 构建环境。CPA 使用支持插件的版本，已支持的最低版本为 7.2.143。

## 配置

在已有 `config.yaml` 中合并以下配置，保留原来的数据库路径及其他插件选项：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-key-billing:
      enabled: true
      state_file: "plugins/cpa-key-billing-state-v1.db"
      account_api_base_url: "http://127.0.0.1:18316"
      billing_multiplier: 1
      codex_fast_mode_billing: true
```

`account_api_base_url` 必须是 CPA 自身的数字回环地址，只填写 origin，不添加 `/v1`。端口要与 CPA 实际监听端口一致；CPA 必须监听该回环地址。HTTPS 地址需要系统信任的证书，不跳过证书校验。不支持主机名、认证信息、查询参数或非回环地址。

全部普通用户 JSON 接口会通过本机 `/v1/models` 检查 Key 是否仍有效，并确认 CPA 未关闭入站鉴权。校验不依赖全局上游代理，不使用管理密码，不执行模型生成。不缓存鉴权成功结果。

- 缺失或无效 Key：`401`。
- 有效但没有账单的 Key：空用量、未跟踪状态、默认不限额。
- 地址未配置、连接失败或 CPA 未启用入站鉴权：`503`。HTML 和管理员接口仍可使用。

## 替换插件

当前服务器在 `/data/collab/Programs/cliproxyapi` 运行，计费插件使用带版本号的动态库。先在插件目录之外暂存构建产物，停止 CPA 后再备份数据库、配置和旧动态库；同时核对 `store.version` 与 `store.release-tag` 是否仍固定为旧版。完整的提交、服务器构建、停服替换、验收和回滚命令见[服务器提交与部署流程](server-deployment-workflow.zh-CN.md)。

## 访问与额度更新

```text
http://222.20.99.38:18316/v0/resource/plugins/cpa-key-billing/usage.html
http://222.20.99.38:18316/v0/resource/plugins/cpa-key-billing/quota.html
```

只有计费插件管理员页面的账号额度查询会更新本插件的内存缓存。管理员进入 **API Key Billing → 认证文件**，查询或刷新对应账号。CPA 核心的另一套配额页面没有向本插件共享结果的回调，单独刷新核心配额页面不会填充本插件缓存。

普通用户只能读取缓存，没有上游额度刷新按钮；重复打开页面、访问旧接口或添加刷新参数也不会查询上游。本人 Key 的额度始终从本地计费数据库读取，与上游缓存独立。

- 未查询：显示“待管理员更新”。
- 超过 24 小时：标记为历史快照并显示更新时间，不自动查询上游。
- 插件重启：内存缓存清空，管理员需要重新查询。
- 禁用或不支持查询：明确标记，不按零余额或满额处理。
- 模型适用范围无法确认：不纳入可用汇总，不推断模型与额度组的关系。

上游汇总按提供商、账号类型、额度组、窗口及单位区分。百分比是注明样本数的账号均值，不是可相加的总余额；缺失数值保持未知。多份凭证不代表多份独立额度，不应为同一上游账号重复导入认证文件后把其额度当成新增容量。

## 独立托管 HTML

可由 Nginx/Caddy 托管交付的两个 HTML，但必须将 `/v0/resource/plugins/cpa-key-billing/` 转发到同一个 CPA，以保持同源访问和 `Authorization` 请求头。两个 HTML 应放在同一目录，页面之间通过相对链接导航。

仅将 HTML 放入 CPA 的 `static/` 目录不会创建访问路由。直接使用插件提供的两个地址即可，无需另建静态服务。通过 `file://` 打开 HTML 不属于支持的部署方式。

## 兼容性变化

旧普通用户资源接口也要求配置 `account_api_base_url`。用户的 `/auth-files` 和 `/routing` 不再返回真实邮箱、文件名或凭证预览；`auth_index` 是服务端生成的不透明标识，只用于用户接口，不能与管理员索引互换，也不能跨 Key 使用。插件重启后编号允许改变。

旧用户单账号配额接口只读取缓存。用户请求明细不再返回上游 Account/Source，来源筛选不再生效；错误接口保留状态码和通用说明，移除原始错误正文和类型。管理员响应保持完整。旧前端会清除用户账号额度缓存，以免继续显示此前缓存的身份信息。

## 验收与回滚

验收时使用两个测试 Key，确认各自只能看到自己的账单和允许的匿名账号；通过管理员刷新，确认两个角色看到一致的 Plus/Pro 20x 类型及额度。检查无缓存、过期和禁用状态，并撤销一个 Key 验证下一次请求返回 `401`。测试不需要真实模型生成。

当前插件 v1.3.18 使用数据库结构 v18，并会从支持的旧版本迁移。结构升级保留费用金额；历史金额换算是独立的离线操作，见 [倍率与历史账单换算](billing-multiplier.md)。

旧插件无法打开 v18 数据库。上线前应在隔离副本上完成迁移与验收；尚未接收新请求时，可停服后恢复完整的插件、配置和数据库备份。已经产生新账单后，保留当前数据库修复，不能直接替换旧插件，也不能用旧数据库备份覆盖新增记录。不要手动修改 `PRAGMA user_version` 伪装数据库版本。
