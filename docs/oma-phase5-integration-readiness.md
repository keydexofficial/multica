# OMA Phase 5 联调前置条件

- 日期：2026-11-23
- 范围：`oma_cloud` 的 Phase 5 真实 OMA 联调与验收
- 相关设计：`docs/oma-cloud-runtime.md`（Phase 5、OMA 侧配置要求、验收标准）
- 前提：假设 OMA 全栈已经可以启动，以下记录除 OMA 依赖之外还需要准备的内容

## 1. Multica 服务端配置（硬阻塞）

- 设置 `MULTICA_VCS_SECRET_KEY`
  - 必须是固定不变的 base64 32-byte key。
  - 未设置时 OMA 连接接口返回 503，前端无法保存凭据：
    `server/internal/handler/cloud_agent_connection.go:180-181`。
- Multica server 进程必须能访问 OMA base URL
  - 需要的是 server 到 OMA 的网络可达，不只是浏览器可达。
  - OMA 自托管允许内网 HTTP；地址、端口、反向代理路径必须正确。
- Multica PostgreSQL migration、server、web、Brain worker 正常运行。
- OMA 连接中的 base URL 建议直接指向 API 根路径，例如：
  ```text
  http://oma.internal:38080/v1
  ```

## 2. OMA 侧资源（硬阻塞）

对应 `docs/oma-cloud-runtime.md:512-524` 的配置清单：

- OMA 全栈：PostgreSQL、Redis、NATS、对象存储、sandbox、Brain worker。
- 组织级 LLM provider 与模型目录。
- 一个可用的 OMA workspace API key。
- 至少一个已有 **environment**：
  - 允许仓库挂载；
  - 出网策略允许访问 GitHub；
  - 使用 GitHub MCP 时允许对应 MCP host。
- 至少一个已有 **agent**：
  - 模型来自组织模型目录；
  - 所有工具权限设为 `always_allow`；
  - MCP toolset 默认的 `always_ask` 必须显式改为 `always_allow`；
  - `ask_user_question` 保持关闭。
- 记录以下值：
  - `oma_agent_id`
  - `environment_id`
  - OMA base URL
  - OMA API key

OMA 侧 agent 和 environment 不能由 Multica 或 bridge 创建，必须提前准备。

## 3. GitHub 与 PR 能力

Phase 5 的“仓库挂载 + branch push / 开 PR”还需要：

- 一个 HTTPS `github.com` 仓库，使用 branch，不能使用 tag/commit。
- GitHub token 至少需要：
  - Contents read/write
  - Pull requests read/write
- 两种 PR 方案二选一：
  - **OMA egress proxy 注入 git 凭据**
    - 需要真实验证 `git push` 是否成功。
    - 这是 `docs/oma-cloud-runtime.md:480` 列出的联调风险。
  - **GitHub MCP + vault**
    - 在 OMA agent 上添加 GitHub MCP server；
    - 创建含 `static_bearer` GitHub token 的 vault；
    - environment 允许 `api.githubcopilot.com` 或开启 `allow_mcp_servers`；
    - 在 OMA 连接配置中填写 vault ID。
- 没有 vault 或 egress 注入时，只能尝试 push branch，不能保证创建 PR。

## 4. Phase 5 验收场景资源

`docs/oma-cloud-runtime.md:442-449` 的 7 个场景需要：

- 测试 workspace、project、issue 和仓库。
- 一个已绑定 `oma_agent_id` 的 Multica agent。
- 可取消的长任务或可挂起任务，用于验证 3 秒内取消。
- 可重复触发 follow-up 的 issue，用于验证 idle session 续用。
- 可修改仓库/凭据的测试条件，用于验证指纹变化时新建 session。
- 可暂停、恢复、断开的 OMA 连接，最好使用可替换的测试 API key。
- 可删除的测试 workspace，用于验证 workspace 删除时清理远端 session；该操作会删除数据，建议使用隔离环境。

## 5. 代码与验证侧的缺口

这些不是启动阻塞，但 Phase 5 不能只依赖现有 fake 测试：

- 没有真实 OMA 响应夹具
  - 当前 bridge 测试使用模拟 OMA。
  - 需要验证真实 OMA 的 `?beta=true`、session events、usage 字段和取消行为。
- 没有真实 usage 账单验证
  - 需要确认真实 OMA 的 `list_cost.amount` 单位、累计口径，以及续用会话不会重复计费。
- 没有真实验证 `git push` 凭据注入
  - egress proxy 方案必须实测。
- 没有 standalone `oma-runtime` 二进制
  - Phase 2 中为可选项；server-managed bridge 不需要。
  - 只有想脱离 server supervisor 单独验证协议时才需要补。
- `apps/docs` OMA 页面和专属品牌图标未完成
  - 属于 Phase 6，不阻塞 Phase 5。
- 工具确认仍按失败处理
  - OMA agent 必须配置为 `always_allow`，否则 `requires_action` 会导致任务失败；需要在真实 OMA 上确认配置生效。

## 6. 最小可联调集合

只跑最小 happy path 时至少需要：

1. OMA 全栈和模型 provider。
2. OMA workspace API key。
3. 已有 environment 和 agent（`always_allow`）。
4. Multica `MULTICA_VCS_SECRET_KEY`。
5. Multica server 能访问 OMA。
6. 测试仓库及 GitHub token。
7. 绑定 `oma_agent_id` 的 Multica agent。
8. 测试 issue。

完整 Phase 5 还需要取消、follow-up、usage、pause/resume/disconnect、workspace 删除、PR/vault 等场景资源。

## 7. 明确不需要 / 不属于 Phase 5

- 创建 OMA 侧 agent 或 environment。
- OMA quick-create、local directory。
- session budgets、交互式 tool approval、SSE。
- `apps/docs` OMA 页面与品牌图标（Phase 6）。
- standalone `oma-runtime`（除非要单独验证协议）。
