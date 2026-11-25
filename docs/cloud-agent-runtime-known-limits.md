# 云 Agent Runtime 已知限制

- 日期：2026-11-23
- 范围：`claude_managed`（Claude Managed Agents）、`qoder_cloud`（Qoder Cloud Agents）以及本仓库新增的 `oma_cloud`（OMA Managed Agents）
- 相关文档：`docs/oma-cloud-runtime.md`、`server/cmd/claude-managed-runtime/README.md`、`server/cmd/qoder-runtime/README.md`
- 上游参考：PR #9761 `feat(runtimes): add Claude Managed Agents as a cloud agent provider`，<https://github.com/multica-ai/multica/pull/9761>

## 1. 结论

这些云 runtime 都是**任务执行桥接**，不是远端 agent 管理平台。

- 可以在 Multica 中创建 agent，并把它绑定到云 runtime。
- 不能在 bridge 中创建或编辑云厂商侧的 agent 和 environment。
- 云厂商侧的 agent、environment、模型、工具和 skill 必须提前准备好。
- bridge 实际创建的是远端 session（以及任务所需的文件或消息），不是云厂商侧的 agent/environment。

“不能创建 agent”这句话容易和 Multica 侧创建 agent 混淆。准确边界如下：

| Runtime | 创建云厂商侧 agent | 创建云厂商侧 environment | 创建 Multica agent | 实际执行方式 |
| --- | --- | --- | --- | --- |
| Claude Managed Agents | 不支持 | 不支持 | 支持 | 绑定已有 `claude_agent_id`，在已有 environment 中创建 session |
| Qoder Cloud Agents | 不支持 | 不支持 | 支持 | 绑定已有 `qoder_agent_id`，使用已有 environment 创建 session |
| OMA Managed Agents | 不支持 | 不支持 | 支持 | 绑定已有 `oma_agent_id`，使用已有 environment 创建 session |

## 2. Claude Managed Agents

### 2.1 Agent 与 environment 的创建边界

`server/cmd/claude-managed-runtime/README.md:99-103`：

> `claude_agent_id` is required. `repository` is an optional default repository
> with an optional branch. ... Several Multica agents can share one runtime with
> different Claude agents. Configure the model, tools and skills on the Claude
> agent; the bridge does not create or edit Claude agents or environments.

因此：

- 必须在 Anthropic 侧已有 environment 和 Claude agent。
- Multica agent 通过 `runtime_config.claude_agent_id` 关联已有 Claude agent。
- 模型、工具、skill 均配置在 Claude agent 上，不在 Multica 中配置。
- bridge 不创建、不修改、不删除 Claude agent 或 environment。

PR #9761 的描述也明确写了：

> “Cloud agent bridges run existing issue runs only.”

以及：

> “Can the bridge create agents? No. The bridge runs existing issue tasks only.”

### 2.2 工具确认与 `always_allow`

bridge 无法响应工具确认。Claude agent 上每个工具都需要设置为 `always_allow`；`always_ask`、`auto` 暂停或其他 `requires_action` 都会让本次运行失败，不会隐式批准。

参考：`server/cmd/claude-managed-runtime/README.md:105-109`。

### 2.3 明确不支持的能力

`server/cmd/claude-managed-runtime/README.md:261-281` 的 `Known limits`：

- 仅支持 issue 执行，不支持：
  - chat
  - squad 协作
  - local directory
  - attachments
  - 任意 Git host
  - tag/commit checkout
  - 完整 comment history hydration
  - 交互式 tool approval
  - 自定义 client-side tools
  - file transfer
  - session budgets
  - usage/billing reporting
- events 采用轮询，不是流式。
- 取消时把 `idle` session 当作已停止；`user.interrupt` 不关闭正在运行的 workflow，因此不能依赖 dynamic workflow。
- ambiguous create 后，session 如果很晚才出现在列表中，单次 create retry 仍可能重复创建。
- 判断 API key 是否 workspace-scoped 依赖 Anthropic 错误文案。
- 结果只以文本和 PR 链接返回，不解析成结构化 branch 字段；`end_turn` 不代表 PR 已创建。
- standalone bridge 在 Runtimes 页面显示为 machine；只有 server-managed bridge 显示为 cloud service。

### 2.4 保存时或执行前会被拒绝的场景

`server/cmd/claude-managed-runtime/README.md:177-194` 的 `What is rejected`：

- chat 或 quick-create；
- squad leader task，或未绑定 issue 的任务；
- 带附件的 issue；
- 缺少有效 `claude_agent_id` 的 Multica agent；
- 带 custom skills、custom env、custom arguments、MCP 配置、model/thinking/service tier override 的 agent；
- 使用 Multica connected apps、runtime MCP tools 或 plugin hook tools；
- 项目有 local directory resource，或有多于一个 repository；
- 非 HTTPS `github.com` repository，或使用 tag/commit ref 而不是 branch。

### 2.5 PR #9761 描述中的验证边界

PR 描述还记录了当时未验证的事项，属于交付状态而非产品能力：

- unresolved-create 的故障场景只有两个回归测试，没有真实账号验证；
- revoked Anthropic key/GitHub token、pause/resume、run timeout、concurrency limit、standalone bridge 未真实验证；
- Playwright e2e 和 Qoder connect flow 未端到端运行；
- 打开 PR 依赖 Claude 侧 GitHub MCP server、`always_allow` 和包含凭据的 vault；
- GitHub token 需要 Contents read/write，否则首次 push 可能 403；
- 中文产品文案检查项未完成。

## 3. Qoder Cloud Agents

### 3.1 Agent 与 environment 的创建边界

Qoder bridge 同样只绑定已有资源：

- `server/cmd/qoder-runtime/README.md:42-43` 要求 PAT 能访问 **配置好的 agent/environment**，且 environment 是 **Existing managed Qoder Environment ID**。
- `apps/docs/content/docs/daemon-runtimes.mdx:336` 明确要求先在 Qoder Cloud Agents 创建 PAT、至少一个 environment，以及要使用的 agents。
- 连接流程只从 PAT 拉取已有 environment 列表并选择一个；没有创建 environment 的接口或 UI。
- `apps/docs/content/docs/daemon-runtimes.mdx:360` 明确：

  > “Local directories and creating agents in Qoder Cloud Agents from Multica are not supported.”

- `server/cmd/qoder-runtime/README.md:251-252` 说明快捷添加对话框创建的是 **Multica agent**，它关联 Qoder 中已有的 agent，并使用已配置的 environment。

因此：

- Qoder 侧 agent 不能由 Multica 创建。
- Qoder 侧 environment 不能由 Multica 创建。
- 可以在 Multica 中创建多个 agent，共享同一个 Qoder runtime，但只要分别绑定不同的 `qoder_agent_id`。
- 一个连接对应一个已有 environment；不是每个 Multica agent 创建一个 environment。

### 3.2 支持的能力

`apps/docs/content/docs/daemon-runtimes.mdx:360` 说明首版支持：

- issue runs；
- chat；
- squad leaders；
- 通过 agent 创建 issue（quick-create）；
- 进度上报；
- 取消；
- 从持久化 journal 恢复；
- issue/chat 附件（受大小和类型限制）；
- chat 中 agent 通过 `DeliverArtifacts` 返回文件。

这些能力仍不包含创建 Qoder 侧 agent/environment。

### 3.3 quick-create 的含义

Qoder 的 quick-create 指的是 **Multica 的“用已有 agent 快速创建 issue”**，而不是快速创建 Qoder agent 或 environment。

执行流程：

1. Multica agent 已绑定一个已有的 `qoder_agent_id`。
2. Qoder connection 已绑定一个已有的 environment。
3. 用户输入一段自然语言，Multica 将一个 quick-create 任务排给该 agent。
4. bridge 使用**已有的 Qoder agent + environment** 创建 Qoder session。
5. Qoder agent 只负责草拟 issue 的 JSON：`title`、`description`、`priority`。
6. bridge 使用 task-scoped credential 调 `POST /api/issues`，创建 Multica issue。

`server/internal/qoderruntime/quickcreate.go:11-13` 的注释说明了这个边界：

> “The Qoder agent only drafts the issue; the bridge creates it with the task credential, because a managed session has no Multica CLI.”

因此：

- quick-create 会创建 Qoder session，但 session 只是执行容器，基于已有 Qoder agent + environment。
- 最终产物是 Multica issue，不是 Qoder agent 或 environment。
- quick-create 不创建 Qoder 侧 agent/environment。
- quick-create 附件仍不支持，会直接拒绝（`server/cmd/qoder-runtime/README.md:204`、`server/internal/qoderruntime/prompt.go:42-44`）。
- quick-create 可以由 squad leader 执行并分配给 squad；普通场景分配给对应 agent。
- 没有有效 `qoder_agent_id` 时，quick-create 也会在创建 session 前失败。

Claude Managed Agents 的 v1 仍然拒绝 quick-create；OMA Managed Agents 已支持同样的流程，但 quick-create 附件仍不支持。

### 3.4 其他限制

`server/cmd/qoder-runtime/README.md:196-202` 和 `:286-287`：

- local directory、任意 Git host、tag/commit checkout、完整 comment history、交互式 tool approval、自定义 client-side tools、terminal access、issue run 返回文件、billing/usage conversion 未实现。
- Qoder agent 必须能自主执行；`requires_action` 会让运行失败。
- 结果和 PR/MR 链接只作为文本返回，不解析成结构化 branch 字段。

## 4. OMA Managed Agents

本仓库新增的 `oma_cloud` 采用与 Claude/Qoder 相同的绑定模型：

- 不能创建 OMA 侧 agent 或 environment；必须绑定已有 `oma_agent_id` 和已有 `environment_id`。
- 支持 issue 与 chat：
  - issue 行为与 Qoder 的 issue 路径一致，包括 issue 附件：图片作为消息内容块，其他文件上传到 OMA Files 后挂载到 session；单个附件上限 5 MB，超出或类型被拒时在 prompt 中列为不可用。带文件附件的 issue run 不续用旧 session（仅图片的 issue 仍可续用，图片随消息发送）。
  - chat 支持文本、图片和文件附件：图片作为消息内容块，其他文件上传到 OMA Files 后挂载到 session；单个附件上限 5 MB，超出或类型被拒时在 prompt 中列为不可用。
  - chat 中 agent 写到 `/mnt/user-data/outputs/` 的文件会按 session 文件资源读取、下载并附加到回复；单个文件上限 10 MB，无法附加时回复中带提示。
  - chat 仍要求 OMA agent 能自主执行；`requires_action`（工具确认或未知 custom tool）会让运行失败。已记录的 custom tool 调用会收到一个明确的错误结果，避免会话长期挂起。
- squad leader 与 quick-create 已支持：leader 由桥接层代发 delegation comment 并记录 squad evaluation；quick-create 由 agent 草拟 issue JSON，桥接层用 task credential 创建 issue。quick-create 附件仍不支持。
- local directory 不支持。
- tool 仍必须配置为 `always_allow`；桥接层不执行交互式工具确认。
- 仓库凭证优先使用连接中手工配置的 token；缺失时回退到工作区 GitHub App 的短期 installation token。轮换 token 会在约 45 分钟后重新签发并推入运行中的 session。
- OMA 侧模型、工具、skill 在 OMA agent 上配置。
- 本仓库额外为 OMA profile 增加了 usage 上报；Claude 路径仍保留 PR #9761 中“usage/billing reporting 未实现”的限制。
- chat、issue 附件、squad leader 与 quick-create 能力只在 OMA profile 打开；Claude Managed Agents 仍为 issue-only，且拒绝 issue 附件、leader 与 quick-create。
- OMA 协议本身还支持 interactive tool confirmation、session budget、SSE、multiagent/threads、per-session overrides、custom tools 客户端执行、agent/environment/skill CRUD、commit checkout 与多仓库；这些仍未接入桥接层。custom tool 调用目前只会收到“bridge 不执行”的错误结果，避免会话挂起。

实现与设计细节见 `docs/oma-cloud-runtime.md`。

## 5. 共同限制摘要

适用于上述三个云 runtime：

- bridge 只执行已有任务，不创建云厂商侧 agent/environment。
- Multica agent 的创建和 runtime 绑定是支持的产品流程。
- 云厂商侧模型、工具、skill、environment 必须先准备好。
- 工具确认不可交互处理；需要 `always_allow`。
- 仅支持单个 HTTPS Git 仓库和 branch；tag/commit、本地目录、多仓库不支持。
- 执行完成不等于 PR 已创建；PR 依赖厂商侧 MCP/vault 或对应凭证。
- session 生命周期由 bridge 管理，但云厂商侧 agent/environment 生命周期不由 bridge 管理。
