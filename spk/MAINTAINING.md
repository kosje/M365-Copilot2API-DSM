# 维护手册

本仓库只做一件事：把上游的 M365 Copilot2API 打包成群晖 DSM 套件，并做必要的 DSM 适配。

## 目录

| 路径 | 内容 |
|---|---|
| `spk/` | 打包全部素材：`INFO`、生命周期脚本、`conf/`、安装向导、`ui/`（桌面图标） |
| `spk/build-spk.sh` | 构建 SPK，带一致性自检 |
| `spk/gen-feed.sh` | 从 SPK 生成套件来源 feed |
| `spk/release.sh` | 一条命令走完：构建 → 发 Release → 更新 feed |
| `.gh-pages/` | `gh-pages` 分支的 git worktree，套件来源 feed 就在这里 |

其余目录来自上游，尽量不动，以便合并时无冲突。

## 远程

```
origin    kosje/M365-Copilot2API-DSM      本仓库
upstream  my788525/M365-Copilot2API-FNOS  上游增强分支
```

上游之上还有原始项目 `HEXUXIU/M365-Copilot2API`，核心能力都来自那里。

## 同步上游

```bash
git fetch upstream
git log --oneline HEAD..upstream/main      # 看新增了什么
git merge upstream/main
```

多数合并是干净的——本仓库的改动集中在上游很少触碰的位置。

**冲突时的处置原则：如果上游独立解决了同一个问题，采用上游的实现，撤掉我们的。** 并行维护两套同类逻辑只会制造永久冲突。已发生过一次：我们把工具循环阈值从 2/3 提到 3/5（治标），上游在 v1.6.0 改成了相邻感知 + 进展感知——只有**连续**的相同调用**且返回相同结果**才计入 stuck，重新读取若内容不同就打断计数。那个方案从根本上区分了「原地打转」和「正常复查」，我们的 `loopThresholds` 已整体让位给上游的 `loopSameLimit` / `loopRepeatLimit`。

合并后务必确认这几处还在：

```bash
grep -c 'ID: "auto"'                    internal/web/codex_catalog.go   # auto 进 /v1/models
grep -c 'kosje/M365-Copilot2API-DSM'    internal/web/selfupdate.go      # 更新源
grep -c 'selfUpdateApplyEnabled = false' internal/web/selfupdate.go     # 一键更新停用
grep -c 'validFileTail'                 internal/web/fileproxy.go       # 文件尾校验
grep -c 'kosje/M365-Copilot2API-DSM'    web/index.html                  # 前端链接（应为 2）
grep -c 'u.applyEnabled!==false'        web/index.html                  # 隐藏一键更新按钮
grep -c '设备码登录（推荐）'              web/index.html                  # 授权方式主次
```

## 发版

```bash
# 1. 改 spk/INFO 里的 version=（DSM 格式 X.Y.Z-BBBB，每次必须递增）
# 2. 提交并推送
# 3. 发版
./spk/release.sh 1.5.7
```

`release.sh` 需要在 Linux / WSL 下跑。Windows 的 tar 不保留权限位，打出来的 SPK 里脚本会丢掉可执行位，DSM 装不上。

### 两套版本号

| 编号 | 位置 | 用途 |
|---|---|---|
| `1.5.6-0001` | `spk/INFO` 的 `version=` | DSM 只认这个格式，套件中心显示它，升级时比较它 |
| `1.5.6` / `1.5.6.1` | Go ldflags 注入的 `Version` + git tag | 控制台横幅比较它 |

第四位（`1.5.6.1`）表示**只改了打包、上游没发版**。`semverAtLeast` 已改为比较全部分量，第四位能被正确识别为更新。

## 踩过的坑

**`INFO` 声明了 `dsmuidir` 却没把 `ui/` 放进 `package.tgz`** —— DSM 在安装末尾注册桌面应用时失败，套件进入「无法修复」的损坏状态，且提示信息毫无指向性。`build-spk.sh` 已把这条做成强制自检。

**CRLF** —— Windows 写的脚本带 CRLF，shebang 变成 `#!/bin/sh\r`，DSM 报 bad interpreter。`.gitattributes` 已对 `spk/scripts/*`、`*.sh`、`INFO`、`conf/*` 强制 LF。

**`set -o pipefail` 配 `grep -q`** —— `grep -q` 命中即退出，上游 `tar` 收到 SIGPIPE 返回非零，整条管道被判失败。检查清单要先落盘再 grep。

**套件用户名不要猜** —— `conf/privilege` 的 `run-as: package` 产生什么账号名不可硬编码，猜错则 `chown` 静默跳过，数据目录留在 root 名下、服务写不进去。脚本改为从 DSM 建好的 `var` 目录继承属主。

**向导密码的重置走 `M365_ADMIN_PASSWORD`** —— 不是 `M365_ADMIN_PASSWORD_BOOTSTRAP_FILE`。后者只在「完全没有密码」时才生效，而数据目录跨卸载留存，持久化密码永远优先，于是「重装改密码」形同虚设。应用侧靠 `<data dir>/admin-envhash` 指纹判断值是否变化。

**`git show HEAD:path` 会施加检出过滤** —— 查仓库里真实的行尾要用 `git cat-file -p`，或直接数 CR 字节。

## 套件来源 feed

```
https://kosje.github.io/M365-Copilot2API-DSM/index.json
```

`release.sh` 会自动更新。手动改的话在 `.gh-pages/` 里编辑后推送即可。

`qinst` 和 `qstart` **必须是 `false`**：它们的含义是「无需向导即可安装/启动」，而本套件带安装向导（要设管理员密码），填 `true` 会让套件中心跳过向导，密码就没地方设了。

GitHub Pages 只能提供静态 GET，无法按请求过滤架构——DSM 会退而依据 `INFO` 里的 `arch` / `os_min_ver` 自行判断，对单一 x86-64 包够用。

## 已知限制

**套件中心不会给手动安装的包提示更新** —— 必须添加上面那个套件来源。这是 DSM 机制，不是 bug。

**桌面图标只在套件运行时出现** —— DSM 在启动时把 `ui` 软链到 `/usr/syno/synoman/webman/3rdparty/`，停止时移除。

**生成文件代下载约 40 KB 上限** —— 取回方式是让模型把文件以 base64 重新吐一遍，受输出预算限制。实测 2.4 KB 的 PDF 完美往返；69 KB 的模型会改为把 base64 写进另一个文件再给链接，等于拿不到。更大的文件请让模型直接内联输出内容。

**控制台页脚的 `v0.4.0`** —— 上游 `web/index.html` 里写死的历史遗留字符串，与实际版本无关，以 `/api/version` 为准。

## 待验证

下面两条需要在真机上确认，我没有 NAS 的访问权限：

1. **卸载留存目录是否真的有效** —— `preuninst` 把数据复制到 `/var/packages/m365-copilot2api/keep`。如果 DSM 卸载时整个删除 `/var/packages/<pkg>/`，这个留存就失效了。飞牛版把它放在包目录**之外**（`/vol1/@appdata/...`），显然是刻意的。
   验证：装好后 `sudo touch /var/packages/m365-copilot2api/keep/probe`，卸载（不勾删除数据），看文件是否还在。

2. **「删除数据」是否真的删干净** —— `preuninst` 判断的 `pkgwizard_delete_data` 变量，因为没提供卸载向导，**永远不会被设置**。若 DSM 自带的删除选项走别的机制，勾了之后 `keep` 里的副本可能仍在盘上——M365 token 不会被真正清除，这是隐私问题。
