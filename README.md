# M365 Copilot2API — 群晖 DSM 套件版

<p align="center">
  <img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="License">
  <img src="https://img.shields.io/badge/Go-1.23%2B-00ADD8?logo=go" alt="Go Version">
  <img src="https://img.shields.io/badge/DSM-7.0%2B-1a73e8" alt="DSM 7.0+">
  <img src="https://img.shields.io/badge/API-OpenAI%20%2F%20Anthropic%20Compatible-412991" alt="OpenAI Compatible">
</p>

把 Microsoft 365 Copilot 背后的 **ChatHub 私有协议**翻译成标准的 **OpenAI / Anthropic 兼容 API**，打包成可直接在**群晖 DSM 套件中心安装**的 SPK。装好后 Claude Code、Cherry Studio、OpenWebUI、ChatBox 等任意 OpenAI 客户端都能直接调用。

> ### 关于本仓库（群晖 DSM 专版）
>
> 本仓库的定位是**只做一件事：提供群晖 DSM 的 SPK 安装包**。核心能力全部来自上游，本分支在其上做群晖适配。
>
> **传承关系**
>
> | 层级 | 仓库 | 贡献 |
> |---|---|---|
> | 原始项目 | [HEXUXIU/M365-Copilot2API](https://github.com/HEXUXIU/M365-Copilot2API) | ChatHub 协议层、Web 控制台、账号体系、会话复用等全部核心能力 |
> | 增强分支 | [my788525/M365-Copilot2API-FNOS](https://github.com/my788525/M365-Copilot2API-FNOS) | `/chat` 对话端、Chat 账户与额度、账号网页导入导出、存储管理、飞牛 fnOS FPK |
> | **本仓库** | kosje/M365-Copilot2API-DSM | **群晖 DSM SPK 打包**与若干 DSM 适配（见下方「与上游的差异」） |
>
> 其它部署方式（多平台二进制 / Docker / 飞牛 FPK）请前往对应上游仓库。

> ### 免责声明（请务必阅读）
>
> - 本项目**不是微软官方产品**，与 Microsoft、OpenAI、Anthropic 及其关联公司**均无任何从属或合作关系**。
> - 使用第三方账号池、代理转发等方式接入 M365 服务**可能违反服务商服务条款**，由此产生的一切后果由使用者自行承担。
> - 请遵守当地法律法规与目标平台的服务条款（ToS）。
> - 本项目**仅供个人学习与研究**，**禁止用于商业转售或规模化运营**。
> - 账号被封禁、数据丢失等任何损失，本项目维护者与贡献者**概不负责**。

---

## 安装

### 环境要求

| 项 | 要求 |
|---|---|
| DSM 版本 | 7.0 及以上（`os_min_ver=7.0-40000`） |
| CPU 架构 | x86-64。SPK 声明 `arch=x86_64`，Package Center 不会在 ARM 机型上提供安装；二进制为 `linux/amd64` |
| 端口 | 4141（固定，安装时无需填写） |
| 依赖 | 无。静态编译，Web 控制台已 `go:embed` 内嵌 |

已实测：**DS918+ / DSM 7.2.1-69057 Update 12 / Intel J1900**。

### 步骤

1. 到 [Releases](https://github.com/kosje/M365-Copilot2API-DSM/releases) 下载 `m365-copilot2api-*.spk`
2. 套件中心 → 右上角**手动安装** → 选择该 SPK
3. 会提示「由第三方开发者提供，未经 Synology 验证」→ 点**确定**
   > DSM 7 取消了 DSM 6 时代的「任何发布者」信任级别开关，所有社区套件都会弹这个提示，**签名也无法消除**，属正常现象。
4. 在安装向导中设置**管理员密码**（至少 12 位，且需含大写、小写、数字、符号中的至少三类）
5. 安装完成后访问 `http://群晖IP:4141`，或点击桌面上的 **Copilot2API** 图标

> **安全提示：控制台是明文 HTTP。** 端口同时服务 API 与控制台，且不加密，因此管理员密码和会话 Cookie
> 会在局域网上明文传输。接口必须能被局域网客户端访问，所以套件不会默认只监听回环。若你的网络不可信，
> 请在群晖**反向代理**后面套一层 HTTPS 再对外暴露，详见 [SECURITY.md](SECURITY.md)。服务启动时若发现
> 监听在非回环地址上，会在日志里打一条明确告警。

### 首次配置

1. 用向导中设置的密码登录控制台
2. **账号管理** → **设备码登录（推荐）** → 按提示在浏览器完成 Microsoft 登录 → 账号自动加入
3. **API Key** 页创建密钥（前缀 `m365_`）
4. 在客户端填入 `http://群晖IP:4141/v1` 与该密钥

## 升级与卸载

**升级**：直接在套件中心手动安装新版 SPK 即可，**无需卸载**。`preupgrade` 会在替换前把数据目录快照到
**包目录之外**（`<卷>/@appdata/m365-copilot2api/preupgrade-snapshot`），`postupgrade` 在替换后回填并校验
`accounts.json` 确实存在才清理快照。快照复制失败会**中止升级**而不是继续——拒绝升级可以重来，丢账号不能。

**卸载**：默认**保留数据**到 `<卷>/@appdata/m365-copilot2api/keep`，重新安装时自动回填（仅在数据目录里
还没有 `accounts.json` 时回填，否则会把你主动删掉的 API Key 复活）。要彻底清除请在该路径下手动删除；
`preuninst` 也会把路径写进套件日志。

**数据位置**：

```
<卷>/@appdata/m365-copilot2api/keep/                 卸载留存（重装自动回填）
<卷>/@appdata/m365-copilot2api/preupgrade-snapshot/  升级快照（校验通过即删除）
/var/packages/m365-copilot2api/var/data/             账号、API Key、用量、会话缓存
/var/packages/m365-copilot2api/target/bin/           二进制
```

## 与上游 fnOS 版的差异

本分支同步至 [my788525/M365-Copilot2API-FNOS](https://github.com/my788525/M365-Copilot2API-FNOS) `v1.6.29`，改动如下（依 AGPL-3.0 第 5(a) 条标注）：

### 1. 群晖 SPK 打包（新增）

FPK 与 SPK 结构高度对应，按 DSM 规范重写：

| 文件 | 作用 |
|---|---|
| `INFO` | 套件元数据（`arch=x86_64`、`adminport=4141`、`dsmuidir`、`dsmappname`） |
| `scripts/start-stop-status` | 启停与状态。用 **PID 文件 + `/proc/<pid>/exe` 校验**判断存活，避免 PID 复用误判；`stop` 先 TERM 等 20 秒再 KILL |
| `scripts/postinst` | 建数据目录、落盘向导密码、回填卸载留存数据 |
| `scripts/preuninst` | 卸载前保留账号数据 |
| `scripts/preupgrade` / `postupgrade` | 升级前快照、升级后回填 |
| `conf/privilege` | `run-as: package`，符合 DSM 7 禁止 root 的要求 |
| `WIZARD_UIFILES/install_uifile` | 安装向导（设置管理员密码） |
| `ui/config` + `ui/images/icon_{16,24,32,48,64,72,256}.png` | DSM 桌面图标 |

### 2. 应用内一键更新已停用，仅保留新版本检测

`internal/web/selfupdate.go`：`selfUpdateApplyEnabled = false`，`POST /api/admin/update/apply` 返回 403 并提示走套件中心；`GET /api/update` 增加 `applyEnabled` 字段供前端隐藏按钮。检测逻辑保留。

**为什么**：套件目录归 root 所有而服务以套件用户运行，原地替换二进制会失败——除非把应用目录交给服务，那等于允许它改写自己的代码；且换掉的二进制与套件中心记录的版本会脱节，后续任何套件操作都可能把它悄悄回滚。**升级请走套件中心。**

更新源已指向本仓库（`updateRepo = "kosje/M365-Copilot2API-DSM"`）。

### 3. 授权方式的主次调整

`web/index.html`：**设备码登录（推荐）** 置于首位并默认打开，**手动粘贴（备用）** 保留为后备。仪表盘「Add account」快捷入口与新手引导第一步改为直接启动设备码登录。

**为什么保留手动粘贴**：微软**自 2026 年 7 月 1 日起对所有新建 Entra 租户在安全默认值下阻止设备码流程**（原因是设备码钓鱼），现有租户的管理员也可随时通过安全默认值或条件访问策略关闭。设备码一旦不可用，手动粘贴是唯一退路。

> 手动粘贴方式**需要在 Microsoft Entra 应用注册中配置重定向 URI**。建议提前配好，不要等设备码被关了才去弄。

### 4. `auto` 智能路由开放给外部 API 客户端

`internal/web/codex_catalog.go`：把 `auto` 加入 `/v1/models` 目录。

上游 v1.5.2 新增了「按密钥配置 auto 模型池与优先级」，但 `auto` 只出现在内置 `/chat` 页面的模型列表里，**外部客户端的下拉框看不到它**。路由逻辑本身（`requestAutoModel`）已作用于 `/v1/chat/completions`，而 `/v1/responses`、`/v1/messages` 内部都以 `r.Clone(r.Context())` 转发给它，context 完整保留——所以只缺目录里的一行声明。

行为：

- 该密钥**配了** auto 模型池 → 确定性路由到池内最高优先级模型，响应头带 `X-M365-Auto-Model`
- 该密钥**没配** → 回落到上游智能路由（`modelTone` 的 `magic`），与原行为一致

相应调整了两处测试：`public_identity_test.go` 的模型数量守卫 14 → 15；`codex_catalog_test.go` 原先用硬编码下标定位 `gpt-5.5`，改为按 ID 查找（该测试的本意是验证「映射内置模型时原地替换而非追加」，与位置无关）。

### 5. 外部 Agent 客户端的普通模型生图加固

在上游 v1.6.29 的提示词清洗基础上，保留并增强 DSM 分支的自动生图管线：

- Claude Code / WorkBuddy 注入的 `<system-reminder>` 不再污染生图意图判断、提示词和 Markdown alt；选择 `gpt-5.6-sol` 等普通模型也会直接路由到 GPT Image 2，不再先派生通用子任务。
- `empty completion`、429、单账号超时及可重试网络错误会自动轮换到尚未尝试的账号；整个请求共用一个总超时，避免按账号叠加成数十分钟。
- 图片必须下载、验证并在网关本地托管成功后才算成功；按内容去重并剥离 C2PA/EXIF 元数据，不向客户端返回不可达的上游临时地址。
- 生图失败直接返回结构化错误，不再退回普通聊天让文本模型“声称已生成”；OpenAI/Anthropic 流式与非流式响应统一通过标准文本内容返回 Markdown 图片。
- `cite call_<uuid>` 等内部调用标记在所有外部响应中无条件清理，即使未启用身份重写策略也不会泄露。

## 功能概览

生成图片链接由网关持久化保存，重启后仍可访问。保存上限为 72 小时、128 张或 512 MiB，容量满时清理最旧文件；请及时另存需要长期保留的图片。旧版已失效的内存链接无法恢复。图片链接无需登录，持有链接即可下载。

完整功能文档见上游 [README](https://github.com/my788525/M365-Copilot2API-FNOS#readme) 与 [FORK_GUIDE](https://github.com/my788525/M365-Copilot2API-FNOS/blob/main/docs/FORK_GUIDE.md)，此处仅列要点：

- **OpenAI / Anthropic 双兼容**：`/v1/chat/completions`、`/v1/responses`、`/v1/messages`、`/v1/images/generations`
- **普通聊天自动生图**：继续使用普通模型和 `/v1/chat/completions`；明确提出“生成图片/画一张图”等请求时自动调用 GPT Image 2，并以 Markdown 图片返回，无需切换模型或新增生图接口
- **多账号轮询 + 故障自动转移**，账号可网页端导入导出（`accounts.json`）
- **API Key 管理**：每日 / 总量额度、模型白名单、IP 白名单、auto 模型池
- **`/chat` 轻量对话端**：流式对话、图片理解与生成、文件分析（CSV / Excel / PDF / 代码）、每用户额度
- **用量统计、代理池、云端会话清理**

## 客户端对接

**Claude Code** — `~/.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://群晖IP:4141",
    "ANTHROPIC_MODEL": "gpt-5.6-sol",
    "ANTHROPIC_API_KEY": "m365_你的密钥"
  }
}
```

> 注意系统里若已存在 `ANTHROPIC_API_KEY` 或同时配了 `ANTHROPIC_AUTH_TOKEN`，会触发鉴权告警，**只保留一个**。

**通用 OpenAI 客户端**：Base URL 填 `http://群晖IP:4141/v1`，API Key 填 `m365_...`，模型选 `auto` 或具体型号。

## 常见问题

**安装报「无法修复」或直接失败**
先在套件中心把处于错误状态的旧套件**卸载**，再装新包。同时确认下载的是完整 SPK（比对 Release 页的 SHA256）。

**桌面图标不见了**
DSM 的机制是套件**启动时**才把 `ui` 目录软链到 `/usr/syno/synoman/webman/3rdparty/`，**停止时移除**。套件停了图标就消失，属正常行为。

**设备码登录报错 / 无法使用**
多半是租户侧已阻止设备码流程（见上文）。改用「手动粘贴（备用）」，并在 Entra 应用注册里配好重定向 URI。

**控制台左下角显示 v0.4.0 之类的旧版本号**
上游 `web/index.html` 的侧边栏页脚是写死的字符串，历史遗留，与实际版本无关。以 `/api/version` 或套件中心显示为准。

**访问不了 4141 端口**
本包未声明防火墙规则。群晖防火墙默认关闭；若你手动开启过，请自行放行 `4141/tcp`。

## 从源码构建

```bash
git clone https://github.com/kosje/M365-Copilot2API-DSM.git
cd M365-Copilot2API-DSM

# go:embed 读的是 internal/web/web/，构建前先同步前端
cp -f web/*.html internal/web/web/

VER=1.5.2
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -a -trimpath \
  -ldflags="-s -w -X m365-copilot2api/internal/web.Version=${VER}" \
  -o spk/package/bin/m365-copilot2api ./cmd/server
```

SPK 为纯静态二进制，用 `tar` 手工组装即可，无需 Synology `pkgscripts-ng` 工具链：`package.tgz` 打包 `bin/` 与 `ui/`，再把 `INFO`、`package.tgz`、`scripts/`、`conf/`、`WIZARD_UIFILES/`、两个图标一起打成**不压缩**的 tar，扩展名改 `.spk`。

> **打包自检**：务必确认 `INFO` 里声明的 `dsmuidir` 目录真实存在于 `package.tgz` 内、`ui/config` 的主键与 `dsmappname` 一致、`icon_{0}.png` 模板涉及的 7 个尺寸齐全。声明了却没交付，DSM 会在安装末尾失败并把套件置为损坏状态。

## 致谢与维护

- **原作者 / 上游项目**：[HEXUXIU/M365-Copilot2API](https://github.com/HEXUXIU/M365-Copilot2API) —— 本仓库的全部核心能力（ChatHub 协议层、Web 控制台、账号体系、会话复用等）均来自上游的杰出工作，敬请前往原作者仓库 star 支持。
- **增强版 Fork**：[my788525](https://github.com/my788525) —— `/chat` 对话端、账户额度体系、账号导入导出、存储管理等，详见其 [FORK_GUIDE.md](https://github.com/my788525/M365-Copilot2API-FNOS/blob/main/docs/FORK_GUIDE.md)。
- **群晖 DSM 适配**：[kosje](https://github.com/kosje) —— SPK 打包与上述 DSM 适配改动。
- 上游合入记录：issue #93 / PR #68 流式截断修复已包含在本分支中。

## 许可证

[AGPL-3.0 with Non-Commercial API Relay Restriction](LICENSE)。

**本项目禁止作为付费 API 中继服务使用。** 请勿将本项目用于任何形式的商业 API 转售、付费代理、按量计费服务等。如果你有大量生产级需求，请直接订阅 [Azure OpenAI](https://azure.microsoft.com/en-us/products/ai-services/openai-service) —— 那才是正途。

这条限制纯粹是为了项目存活。一旦出现商业转售，极易引来法律风险导致项目被下架。我不想看到这个项目 GG，希望大家理解并遵守。

依 AGPL-3.0 第 13 条：若你修改本程序并将其作为网络服务提供给他人使用，必须向这些使用者提供对应源码。仅自己使用不触发该义务。
