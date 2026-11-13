# kiro4cpa-plugin

中文 | [English](./README.en.md)

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）v8 的 Kiro provider 插件。以原生 c-shared 插件的形式加载，把 Kiro 账号接入 CPA，对外提供 Claude Messages、OpenAI Chat Completions / Responses 接口。

> 非官方项目，与 AWS / Kiro 无关联。请遵守 Kiro 的服务条款。

## 功能

- 浏览器内选择登录方式：Builder ID、Google / GitHub、IAM Identity Center，或导入 Refresh Token / API Key
- 凭据类型：Social（Google / GitHub）、IdC（Builder ID / IAM Identity Center）、External IdP（Entra ID 等）、API Key
- 登录后读取账号邮箱，凭据文件按账号命名；同一账号再次登录会覆盖原文件，不会出现重复账号
- 自动刷新 access token；请求遇到 token 失效时强制刷新一次并重试，刷新结果写回凭据文件
- 刷新时从 Kiro 拉取账号可用的模型列表并缓存；拉取失败时使用内置列表
- 流式 / 非流式、extended thinking、tool use、图片输入（jpeg / png / gif / webp）
- 通过 CPA QuotaProvider 查询 Kiro 用量额度；免费试用和赠送额度只统计生效中的部分
- 全局 `oauth-excluded-models` 对每个账号都生效，账号自己的禁用列表在此基础上追加
- Kiro 以 403 `User is not authorized to make this call` 拒绝账号时，自动禁用该账号并写入备注
- 上游错误按状态码交给 CPA：401 / 402 / 403 / 429 触发冷却和换号，5xx 报为可重试的 502
- 按凭据限速，并记住 Kiro 报出的月度额度和账号暂停，避免反复打上游

插件只实现 Claude Messages 执行器，OpenAI 格式由 CPA 自行转换。

## 构建

需要 Go 1.26 和 cgo。

```bash
make test                # 单元测试
make build-linux-docker  # 用 Docker 交叉编译 bin/linux/amd64/kiro4cpa.so
make build-darwin        # 本机编译 bin/darwin/arm64/kiro4cpa.dylib（需要可用的 Xcode Command Line Tools）
```

`build-linux-docker` 在 `golang:1.26-bookworm` 中用 `gcc-x86-64-linux-gnu` 交叉编译，产物要求 glibc ≥ 2.34。

部署到 CPA 时，动态库文件名决定插件 ID 和版本：必须是 `kiro4cpa-v<版本>.so`（或 `.dylib` / `.dll`），命名成 `kiro4cpa.so` 会让宿主注册成不带版本的 `kiro4cpa`。开发、测试和发布流程见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 安装

### 从插件商店安装

在 CPA 管理面板的插件商店搜索 **Kiro OAuth**（ID `kiro4cpa`）安装即可，商店会按当前系统和架构下载对应的发布包。

插件商店未收录时，可以在 CPA 的 `config.yaml` 里加上本仓库的商店源：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/AkkunYo/kiro4cpa-plugin/main/registry.json"
```

### 手动安装

从 [Releases](https://github.com/AkkunYo/kiro4cpa-plugin/releases) 下载对应平台的 `kiro4cpa_<版本>_<系统>_<架构>.zip`，按 `checksums.txt` 校验后解压。

1. 把动态库放进 CPA 的插件目录（默认是 CPA 工作目录下的 `plugins/`），按平台分子目录：

   ```
   plugins/linux/amd64/kiro4cpa.so
   ```

2. 在 CPA 的 `config.yaml` 中启用插件：

   ```yaml
   plugins:
     enabled: true
     configs:
       kiro4cpa:
         enabled: true
         # 以下均为可选，括号内为默认值
         # default_region: us-east-1   # 凭据未指定 apiRegion、profileArn 中也没有区域时使用
         # client_version: 1.4.5       # User-Agent 中上报的 Kiro IDE 版本
         # os_version: darwin#24.6.0
         # node_version: 22.22.0
         # min_token_interval: 1s
         # max_token_interval: 2s
         # suspend_cooldown: 1h
   ```

3. 重启 CPA，日志中出现 `plugin registered plugin_id=kiro4cpa` 即加载成功。

## 手动放置凭据文件

一般直接用下方「Web 登录授权」里的登录或导入即可。需要批量迁移或脚本化时，也可以在 CPA 的 `auth-dir` 下为每个账号放一个 JSON 文件，顶层必须有 `"type": "kiro4cpa"`：

```json
{
  "type": "kiro4cpa",
  "authMethod": "social",
  "refreshToken": "<refresh-token>",
  "region": "us-east-1"
}
```

| authMethod | 必填字段 |
|---|---|
| `social` | `refreshToken` |
| `idc`（也接受 `builder-id`、`iam`） | `refreshToken`、`clientId`、`clientSecret` |
| `external_idp`（也接受 `azuread`、`entra`） | `refreshToken`、`clientId`、`tokenEndpoint`（https） |
| `api_key` | `kiroApiKey` |

可选字段：`accessToken`、`expiresAt`、`profileArn`、`authRegion`、`apiRegion`、`machineId`、`email`。没有 `accessToken` 时，CPA 加载后会立即刷新一次。

- kiro.rs 的 `credentials.json` 是数组，CPA 不支持数组文件。请把每个元素单独存成一个文件，并补上 `type` 字段。
- 刷新后的 token 和模型列表会写回同一个文件。这个文件等同于账号密码，请限制文件权限，不要提交到版本库。

## 模型

`/v1/models` 中列出的是 Kiro 自己的模型 ID，例如 `claude-sonnet-4.5`、`claude-haiku-4.5`、`auto`。需要其他写法时，用 CPA 的 `oauth-model-alias` 配置别名，通道名为 `kiro4cpa`：

```yaml
oauth-model-alias:
  kiro4cpa:
    - name: claude-sonnet-4.5
      alias: claude-sonnet-4-5
      fork: true   # 同时保留原 ID
```

账号的「排除模型」支持原始 ID 和 CPA 当前配置的别名，也支持 `*` 通配符。例如 Pro 账号从官方面板选择排除 `claude-sonnet-4-5`，插件会在注册模型前排除该账号对应的 `claude-sonnet-4.5`，其他账号不受影响。禁用的是该账号上的底层模型，因此它的原始 ID 和其他别名也一起禁用；账号级同名别名覆盖通道级映射。全局禁用规则与账号规则叠加，清空账号规则后会恢复未被全局禁用的模型。

## 用量额度

插件实现了 CPA 的 QuotaProvider，可以通过 CPA 管理接口查询某个账号的 Kiro 额度：

```
GET /v0/management/plugins/kiro4cpa/quota?auth_index=<auth-index>
```

管理面板能否展示额度，取决于面板是否支持插件 QuotaProvider。官方 [Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) 支持；部分第三方面板只认内置 provider，不会显示。

## 请求限速

多账号场景下 CPA 自己会换号，但同一个凭据被并发打到 Kiro 时仍会触发上游限流。插件按凭据做节流，并把 Kiro 报出的持续性状态记下来，避免反复打上游：

```yaml
plugins:
  configs:
    kiro4cpa:
      min_token_interval: 1s   # 同一凭据两次请求的最小间隔
      max_token_interval: 2s   # 间隔上限，也是排队等待的上限
      suspend_cooldown: 1h     # 被 Kiro 判定暂停后退出轮换的时长
```

行为说明：

- 间隔在 `min_token_interval` 与 `max_token_interval` 之间随机取值，再叠加 ±30% 抖动，避免多实例同时发起
- 并发请求在 `max_token_interval` 内排队；超过上限时返回 429，交给 CPA 换号，而不是无限等待
- 上游返回 402（月度额度）时，该凭据退出轮换到下一个 UTC 零点
- 上游返回 403 且带 `SUSPENDED` 或 `User is not authorized to make this call` 时，退出轮换 `suspend_cooldown`
- 单个凭据文件加 `"disable_cooling": true` 可退出上述全部逻辑，CPA 自身的冷却不受影响

凭据选择、换号和 429 退避仍由 CPA 负责，插件不重复实现。

## Web 登录授权

在管理面板「登录授权」页面点击 Kiro 登录，会在新标签页打开插件的登录页 `/v0/resource/plugins/kiro4cpa/connect`，在页面里选择登录方式：

| 方式 | 说明 |
|---|---|
| Builder ID | AWS 设备授权。打开 AWS 页面输入邮箱，确认授权码 |
| Google / GitHub | 打开 Kiro 登录页选择 Google 或 GitHub。登录后浏览器会跳到打不开的 `127.0.0.1:3128` 页面，把地址栏里的完整地址粘贴回登录页 |
| IAM Identity Center | 填写 `https://<目录>.awsapps.com/start` 和 Region，再走 AWS 设备授权 |
| 导入凭据 | 已有 Refresh Token 或 API Key 时直接导入，见下表 |

完成授权后，由 CPA 保存凭据。登录页通过官方宿主状态接口确认保存结果，仅在宿主返回成功后显示“凭据已保存”；保存失败会显示具体错误。原管理面板关闭后，登录页仍可完成保存确认。

Google / GitHub 的完整回调地址必须提交到 **Kiro 登录页**。当前官方面板的通用“提交回调 URL”会丢弃 Kiro 所需的回调路径和 `login_option`，且宿主不会把完整 URL 传给插件，因此暂不支持在该通用输入框中完成 Kiro 回调。

刷新登录页可恢复当前方式、授权链接和设备授权码，并显示剩余时间。管理密钥失效时会停止轮询，重新输入并点击“验证密钥并继续”后恢复；连续三次网络失败也会暂停，可手动重新查询。授权失败或过期时，返回管理面板重新发起登录，以建立新的宿主登录会话。

「导入凭据」支持三种类型：

| 凭据类型 | 需要填写 | 从哪里取 |
|---|---|---|
| Social Refresh Token（Google / GitHub） | Refresh Token、Region | Kiro IDE 或 kiro.rs 凭据里的 `refreshToken` |
| IdC Refresh Token（Builder ID / 企业 SSO） | Refresh Token、Client ID、Client Secret、Region | AWS SSO 缓存（`~/.aws/sso/cache/`）里的 `refreshToken`，以及同目录 client 注册文件里的 `clientId`、`clientSecret` |
| Kiro API Key | API Key | Kiro 后台生成的 API Key |

Region 默认 `us-east-1`。导入时会校验必填字段，并尝试刷新 OAuth Token 或查询 API Key 的额度。页面分别提示验证成功、凭据失效或暂时无法验证；验证失败仍可将凭据交给 CPA，最终保存结果以管理面板为准。读取账号邮箱失败不阻止导入。

登录页的操作走 CPA 管理接口，需要管理密钥。如果面板登录时勾选了「记住密码」，并且面板和 CPA 用同一个地址打开，登录页会直接读取面板保存的密钥；否则会提示手动输入，输入的密钥不会保存。仅读取官方当前 `enc::v2::` 格式且勾选记住密码的凭据，旧版或明文凭据需重新输入。

登录页沿用官方面板的暖灰、纯白、深色和自动主题，并跟随面板语言选择。当前提供简体中文、繁体中文和英文，其他语言回退到英文；服务端返回的错误详情保留原文。“返回管理面板”会打开官方登录授权页，不依赖新标签页的 `window.opener`。

凭据文件按 `kiro4cpa-<登录方式>-<邮箱>.json` 命名，例如 `kiro4cpa-google-user@example.com.json`。拿不到邮箱时用账号 ID 的 hash，同一账号再次登录会覆盖原文件。

## 可靠性与兼容性

- 执行请求、额度查询和定时刷新共用账号刷新协调器，插件写回按账号串行，定时刷新结果由 CPA 单独保存。插件保存失败时保留最新 Token，后续请求重试保存；每个账号最多保留 64 份旧凭据指纹，超出记录范围的过旧快照返回错误，不重新消耗旧 Token。缓存仅存在于当前进程，无法替代宿主持久化。
- 损坏、截断或无法解析的上游事件会返回错误，流式回答不会以正常 `message_stop` 结束。工具参数按调用 ID 累积，支持交错分片及迟到工具名，输出工具块按顺序排列以兼容 CPA Responses 转换器；非法 JSON、非对象参数或缺少结束标记的非空参数会报错，不替换成空对象。无参数调用允许上游省略结束标记。
- 流式和非流式共用结束原因规则，保留上游截断、上下文溢出和拒答信号，不把 thinking-only 自动判为截断。工具输出途中发生拒答时终止响应。分块到达的 HTTP 错误体在大小限制内读取后再分类，避免漏触发 Token 刷新。
- 末尾连续 user 消息合并为当前轮次，保留图片和工具结果，不插入虚构的 assistant「OK」。工具描述保留客户端要求，不再默认注入 Write/Edit 强制分块及禁止进度说明的提示；仍执行 Kiro 必需的名称长度、描述长度和 Schema 规范化。
- 额度的单位为 Credit；月度、免费试用和赠送额度分别显示用量、上限及官方重置／到期时间。官方面板把桶描述用作行标题，因此不在每行追加查询时间，避免标签过长。认证失败、查询失败与额度耗尽分别处理，失败不伪装成零额度。
- 当前协议转换处理消息、工具定义及结果、Base64 图片和 thinking。`max_tokens`、`temperature`、`top_p`、`top_k`、`stop_sequences`、`tool_choice` 和结构化输出 schema 尚未转发或强制执行，不应依赖这些参数限制输出或强制选用工具。
- reasoning/effort 适配参考 `ZyphrZero/kiro.rs` v0.9.9：已知支持的 Claude 模型发送 `additionalModelRequestFields.output_config.effort`，GPT 5.6 sol/terra/luna 和 Grok 4.7 发送 `reasoning.effort`；Grok 的 `max` 映射为 `xhigh`；未知模型保持提示词适配。显式 effort 优先，否则由 thinking budget 推导；Opus 4.6 仅对显式 adaptive 请求发送原生字段。`thinking: disabled` 优先。上游真实 token usage 优先，本地输出估算按累计字符计算，避免分片数量影响结果。
- 实际发送原生 effort 时不再重复注入 thinking 提示；旧模型和 Opus 4.6 enabled 模式保留提示路径。关闭 thinking 后，原生推理不会转为普通正文，即使回复只有推理内容。
- 历史推理使用独立 `reasoningContent` 字段：一个合并的 assistant 轮次仅回传单个带有效非占位签名的 thinking 块，或单个有效 Base64 redacted 块；多块、不完整或无签名内容省略，正文和工具保留。仅在 HTTP 400 明确返回 `THINKING_SIGNATURE_INVALID` 时移除该字段重试一次，与 Token 刷新共用原有一次重试预算。历史签名回传已通过本地协议测试，尚未用真实 Kiro 凭据验证。
- Kiro 不支持 assistant prefill，末尾的 assistant 预填消息会被丢弃；图片 URL、document 等未实现的内容类型也不会传给上游。

## 已知限制

- Builder ID、Google、GitHub 登录已实测；IAM Identity Center、External IdP、API Key 按协议实现
- 发布产物覆盖 linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64；实际跑通过的是 linux/amd64 与 darwin/arm64

## 致谢

协议细节参考了以下 MIT 项目，版权声明见 [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES)：

- [hank9999/kiro.rs](https://github.com/hank9999/kiro.rs)、[ZyphrZero/kiro.rs](https://github.com/ZyphrZero/kiro.rs)
- [xiaokui-dev/cliproxyapi-kiro-plugin](https://github.com/xiaokui-dev/cliproxyapi-kiro-plugin)
- [nguyenha935/kiro-cpa-plugin](https://github.com/nguyenha935/kiro-cpa-plugin)

## License

[MIT](LICENSE)
