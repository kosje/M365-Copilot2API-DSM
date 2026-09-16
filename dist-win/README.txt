M365-Copilot2API —— Windows 本机版 (v1.6.14)
================================================

与 fnOS / Linux 版共用同一套内核代码，仅编译目标不同。
API Key 仅作为「引入的自定义模型」使用（OpenAI 兼容接口），
模型被强制告知：它不是任何服务器端托管助手，而是运行在你
本机 Windows PC 上的编码 Agent 推理核心。

【在线更新（双版本均支持）】
  本版本与 fnOS/Linux 版都支持管理台「一键更新」：
  管理台检测到新版本后，点 ⚡ 即自动从 GitHub Release 下载
  对应平台的最新二进制并原地替换重启。Windows 端由于系统锁定
  运行中的 exe，更新时会先拉起新副本、由新副本覆盖规范 exe
  后再退出旧进程，全过程无需手动干预。
  （GitHub Release 同时发布 Windows 与 fnOS/Linux 两个版本，
   各自携带可自更新的原生二进制。）

【启动】
  双击 run.bat 即可开始运行。
  - 自动在本机监听 http://127.0.0.1:4141
  - 自动打开浏览器到管理/聊天页
  - 数据/配置保存在同目录的 data\ 下
  - 按 Ctrl+C 停止

【自定义模型接入】(WorkBuddy / Trae / ChatBox / OpenWebUI 等)
  base_url : http://127.0.0.1:4141/v1
  api_key  : 在网页管理台创建的密钥
  model    : auto

【重新编译】(可选，首次需联网下载 Go 工具链一次)
  双击 build.bat

【可选配置】
  在同目录新建 m365.env（参考 m365.env.example）可覆盖：
  M365_LISTEN  监听地址（默认 127.0.0.1:4141；同局域网访问改 0.0.0.0:4141）
  M365_DATA_DIR 数据目录
  HTTPS_PROXY / HTTP_PROXY  上游出网代理
