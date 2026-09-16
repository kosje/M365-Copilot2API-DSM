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

**向导密码走 `M365_ADMIN_PASSWORD_RESET_FILE`，不是 `M365_ADMIN_PASSWORD`** —— `postinst` 把向导口令写进
`var/admin-password-reset`（0600），`start-stop-status` 只导出**文件路径**；服务启动时读取、**立即删除**该文件，
并把口令的 sha256 指纹写进 `<data dir>/admin-envhash`。指纹一致就不再应用，所以「重装填新密码 = 重置」成立，
日常重启不会覆盖你在网页端改过的密码。改用文件而非环境变量的原因：常驻进程的环境变量对**同 uid 的任何进程**
可读（`/proc/<pid>/environ`），而旧实现把明文一直留在 `var/admin-password-wizard` 里从不删除。

`M365_ADMIN_PASSWORD_BOOTSTRAP_FILE` 仍然刻意不用：它只在「完全没有密码」时才生效，而数据目录跨卸载留存，
持久化密码永远优先，于是「重装改密码」形同虚设。

**在包目录里放快照等于没放** —— `preupgrade` 快照与 `preuninst` 留存都放在
`<卷>/@appdata/m365-copilot2api/` 下，**不在** `/var/packages/<pkg>/` 里。如果 DSM 升级时清掉包目录（这正是
快照存在的理由），放在里面的副本会跟原件一起消失。保留旧位置的回填逻辑，便于跨版本升级。

**复制失败必须让升级失败** —— `cp` 的退出码要检查，且**校验通过之前不能删唯一副本**。旧实现把 `cp` 的错误
重定向到 `/dev/null` 再无条件 `rm -rf`，磁盘满时会从「有备份」直接变成「没备份」，而套件中心仍显示升级成功。

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

## 脚本的可执行位

`spk/build-spk.sh`、`spk/release.sh`、`spk/gen-feed.sh`、`build_linux.sh`、
`spk/tests/lifecycle.sh`、`migrate/migrate.sh` 在仓库里是 100755，因为文档就是让
你直接 `./spk/build-spk.sh` 跑的。

**Windows 上 `core.fileMode` 默认是 false**，`chmod +x` 不会被 git 记录，整个仓库
的文件模式都会是 100644——照 README 在 WSL 里跑就会 `Permission denied`。要给某个
文件加上可执行位，用：

```bash
git update-index --chmod=+x spk/build-spk.sh
```

CI 里的「assemble the SPK」这一步就是按文档用 `./spk/build-spk.sh` 调用的，所以
掉权限会立刻变红。

`spk/scripts/*` 保持 100644：DSM 用 `sh <脚本>` 调用它们，套件归档内的权限由
`build-spk.sh` 打包时统一设置成 755。

## 生命周期脚本的回归测试

`spk/tests/lifecycle.sh` 会**真的执行**这些钩子（不是语法检查），在临时目录里搭一套模拟的套件树，
模拟「DSM 把包目录清空」这一悲观场景，断言快照、回填、留存、口令交付的结果。

```bash
bash spk/tests/lifecycle.sh     # 34 项断言
```

历史教训是：这些脚本看着没问题，但 `cp` 失败被吞、postupgrade 恒 `exit 0`、留存目录放在包目录里，
任何一条都能静默丢掉全部账号。改动这几个脚本后务必跑一遍。

## 待验证

下面几条需要在真机上确认，我没有 NAS 的访问权限：

1. **`@appdata` 是否可写、路径是否如预期** —— 快照与留存现在放在
   `<卷>/@appdata/m365-copilot2api/`。`retain_base()` 会依次探测 `$SYNOPKG_PKGDEST_VOL`、`/volume1`…
   `/volume5` 里的 `@appdata`，取第一个存在的；全都不存在时回退到包目录的上一级。选中的路径会写进套件日志。
   验证：`grep -i '@appdata' /var/packages/m365-copilot2api/var/data/app.log` 或套件日志。

2. **卸载后留存目录是否真的还在** —— 现在它在包目录之外，**理论上**不受卸载影响，这正是改动的目的。
   验证：装好 → 卸载（不勾删除数据）→ 看
   `/volume1/@appdata/m365-copilot2api/keep/accounts.json` 是否还在。

3. **「删除数据」是否真的删干净** —— `preuninst` 会响应 `pkgwizard_delete_data=true` 与
   `SYNOPKG_PKG_STATUS=UNINSTALL_DELDATA` 两种信号。因为本套件没有卸载向导，这两个变量可能永远不会被设置，
   此时留存副本会保留（M365 token 不会被清除）。验证：勾选删除数据后卸载，看
   `/volume1/@appdata/m365-copilot2api/keep/` 是否还在；若还在，需要手工删除，或补一个卸载向导。

4. **`admin-password-reset` 的权限位** —— `postinst` 用 `umask 077` + `chmod 600` 写入。Windows 不映射
   POSIX 权限位，本地测试只能跳过这一项。验证：安装后 `stat -c '%a' /var/packages/m365-copilot2api/var/admin-password-reset`
   应为 600；并确认首次启动后该文件已被服务删除。
