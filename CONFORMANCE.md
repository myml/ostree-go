# OSTree Go 重写 — 上游测试分诊（Conformance Triage）

> 生成时间：2026-10-19T14:00Z（UTC）  
> 被测二进制快照 **T2**：`sha256=a273e4ab9dd85f5ba6bd5bd21106eabde1c7718f64b03c0e0b3b9c078667dd26`（2026-10-19T13:53Z 构建，源码 HEAD `1bc34f37`）  
> 基线快照 **T1**：`sha256=0d79551d7656b4e0b635184393928ebf0de4fc093bd092242dd2116aeec4fdff`（13:42Z 构建）；两次快照差异见附录 A。  
> 环境：Debian GNU/Linux 13 (trixie)，x86_64，Linux 7.0.2-6-pve，4 vCPU / 8 GiB，root；Go 1.26.8；无 SELinux；/tmp 为 tmpfs（无 reflink）；本机可访问 `127.0.0.1` 但**不访问外网**。  
> 清单：`./hack/conformance.sh --list --all` 报告 111 个 `tests/test-*.sh`（默认排除 10 个）。
>
> 范围：共 **111** 个脚本，逐个用 `go/hack/conformance.sh --test NAME --timeout 120` 运行（含 `conformance.sh` 默认排除的 10 个脚本，为了全面分诊）。所有结论来自实际运行日志（`/tmp/conf-run2/<name>/builddir/logs/`），未修改 `tests/` 或 Go 实现。admin/fuse 类按任务要求只记录首个失败即停止，未做深度调试。

## 1. 结论摘要

| 指标 | 数量 | 说明 |
|---|---|---|
| 测试脚本总数 | **111** | 上游 `tests/test-*.sh` |
| PASS | **5** | TAP 全部通过且退出码 0 |
| FAIL | **88** | 至少有 TAP `not ok` 或脚本非 0 退出 |
| SKIP | **18** | `1..0 # SKIP` 或唯一一条 `ok … # SKIP` |
| harness 视为通过 | **23 / 111** | PASS + SKIP（`conformance.sh` 的判定口径） |
| 其中实现缺口 | **86** | 88 个 FAIL 中 2 个是 harness/外部工具问题 |
| 需要外网 | **0** | 所有 remote 都指向本机 `ostree-trivial-httpd`；`metalink/mirrorlist` 也是本机 |

**T2 通过的测试（5 个）**：`test-config`、`test-help`、`test-init-collections`、`test-backcompat-fsck`、`test-reset-nonlinear`。  
**按 T2 实现，最多能通过的脚本数 = 5/111**；若把跳过也计为 harness 通过，则为 **23/111**。

### 1.1 分诊快照说明（重要）

本目录的实现正在快速演进：T1（13:42Z）→ T2（13:53Z）之间，`fsck`/`reset`/`commit --bootable` 已落地（多 2 个 PASS），同时 `--version` 开始声明 `libarchive`，使 `test-export`/`test-libarchive` 由 SKIP 变为 FAIL。T2 之后源码仍在变化（`summary.go`、`show.go`、`log.go`、`checkout` 等命令陆续出现），因此：

- 本报告以 **T2 快照**为准，所有表格均为该快照的实际运行结果；
- 复查命令：`cd go && ./hack/conformance.sh --builddir /tmp/cf --test test-config --timeout 120`，整轮扫描见 §7；
- 写作时（14:00Z）再次尝试构建当时的最新源码失败（`error: failed to build ./cmd/ostree`，源码正处于编辑中），因此 **T2 是最近一个“可构建且完成全量测量”的快照**；实现落定后应重跑本报告。
- 若需要与实现进度对齐的持续看板，建议在固定 commit 上重跑并 diff 本报告。

## 2. 完整结果表（111 个脚本，按状态→优先级排序）

| 测试名 | 状态 | 阻塞原因（首条关键错误） | 依赖的命令/能力 | 优先级 |
|---|---|---|---|---|
| `test-help` | PASS | — | help/usage 渲染（命令表完整） | P0 |
| `test-backcompat-fsck` | PASS | — | `fsck`（旧仓库、user xattr） | P1 |
| `test-config` | PASS | — | `config get/set/unset`（含 `--group`） | P1 |
| `test-init-collections` | PASS | — | `init --collection-id` + config 写入 | P1 |
| `test-reset-nonlinear` | PASS | — | `fsck`、`reset`、非线性 commit 图 | P1 |
| `test-basic` | SKIP | 需要 root：`SKIP this test requires SELinux relabeling support` | SELinux 策略/restorecon（本机无 SELinux） | P1 |
| `test-basic-bare-split-xattrs` | SKIP | 需要 root：`SKIP this test needs sudo, skipping without OSTREE_TEST_SUDO being set` | bare-split-xattrs + sudo | P1 |
| `test-commit-sign` | SKIP | 需要外部工具：`SKIP no gpgme support compiled in` | `commit --gpg-sign` + GPG | P1 |
| `test-delta-ed25519` | SKIP | 需要外部工具：`SKIP no sign-ed25519 support compiled in` | ed25519 签名 | P1 |
| `test-demo-buildsystem` | SKIP | 需要外部工具：`SKIP no fusermount` | rofiles-fuse + 构建系统集成 | P1 |
| `test-gpg-signed-commit` | SKIP | 需要外部工具：`SKIP no gpgme support compiled in` | `commit --gpg-sign` + GPG 校验 | P1 |
| `test-parent` | SKIP | 需要外部工具：`SKIP no gpgme support compiled in` | `commit --parent/--skip-if-unchanged` + GPG | P1 |
| `test-payload-link` | SKIP | 环境限制：`SKIP no reflinks`（/tmp tmpfs 不支持 reflink） | payload-link + reflink | P1 |
| `test-xattrs` | SKIP | 环境限制：上游无条件 SKIP（不提交 `user.*` xattr） | xattr 处理 | P1 |
| `test-find-remotes` | SKIP | 需要外部工具：`SKIP no gpgme support compiled in` | `find-remotes` + GPG | P2 |
| `test-pre-signed-pull` | SKIP | 需要外部工具：`ok pre-signed pull # SKIP due ed25519 unavailability` | ed25519 校验 + pull | P2 |
| `test-pull-corruption` | SKIP | 需要外部工具：`SKIP no gjs` | gjs + 损坏注入 | P2 |
| `test-pull-summary-caching` | SKIP | 需要外部工具：`SKIP no gpgme support compiled in` | summary 缓存 + GPG | P2 |
| `test-create-usb` | SKIP | 需要外部工具：`SKIP no gpgme support compiled in` | `create-usb` + USB/mount | P3 |
| `test-repo-finder-mount-integration` | SKIP | 需要 root：`SKIP this test needs sudo, skipping without OSTREE_TEST_SUDO being set` | repo-finder-mount helper + mount | P3 |
| `test-rofiles-fuse` | SKIP | 需要外部工具：`SKIP no fusermount` | rofiles-fuse（不重写该 helper） | P3 |
| `test-admin-deploy-composefs` | SKIP | 范围外：`1..0 # SKIP no composefs support compiled in` | composefs（本阶段不做） | — |
| `test-composefs` | SKIP | 范围外：`SKIP no composefs support compiled in` | composefs（本阶段不做） | — |
| `test-cli-extensions` | FAIL | 缺命令：`ostree env` → `error: Unknown command 'env'` | `env` 内置命令 + PATH 中 `ostree-<cmd>` 扩展分发 | P0 |
| `test-refs-collections` | FAIL | 输出不一致：`refs --collections` 仍输出裸 ref 名，期望 `(org.example.Collection, test-1)` | `refs --collections` 输出/过滤；测试后段还依赖 `pull` | P0 |
| `test-summary-view` | FAIL | 缺选项：`summary -u` → `error: Unknown option -u` | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P0 |
| `test-archivez` | FAIL | 缺命令：`error: pull-local: not implemented` | `pull-local`（本地对象/引用复制） | P1 |
| `test-auto-summary` | FAIL | 功能缺陷：`fsck -q` 误报 `error: Repository corruption encountered`（content object 按 `.file` 查找失败） | `fsck` 各仓库模式的对象查找/校验 | P1 |
| `test-basic-root` | FAIL | 功能缺陷：`fsck -q` 误报 `error: Repository corruption encountered`（content object 按 `.file` 查找失败） | `fsck` 各仓库模式的对象查找/校验 | P1 |
| `test-basic-user` | FAIL | 功能缺陷：`checkout -H` 在 bare-user 仓库上本应失败却成功（`assert_not_reached 'checkout -H worked?'`） | `checkout -H`/`-U` 模式校验 | P1 |
| `test-basic-user-only` | FAIL | 功能缺陷：`fsck -q` 误报 `error: Repository corruption encountered`（content object 按 `.file` 查找失败） | `fsck` 各仓库模式的对象查找/校验 | P1 |
| `test-commit-timestamp` | FAIL | 缺命令：`ostree show cli` → `error: show: not implemented` | `show`（commit 元数据与 Date 格式化）；15 个脚本使用 show | P1 |
| `test-corruption` | FAIL | 功能缺陷：`fsck -q` 误报 `error: Repository corruption encountered`（content object 按 `.file` 查找失败） | `fsck` 各仓库模式的对象查找/校验 | P1 |
| `test-delta` | FAIL | 缺选项：`static-delta generate --empty --to=…` → `error: Unknown option --empty` | `static-delta generate/apply`（编译、索引） | P1 |
| `test-delta-sign` | FAIL | 缺选项：`static-delta generate --from=… --to=…` → `error: Unknown option --from=…` | `static-delta generate/apply`（编译、索引） | P1 |
| `test-export` | FAIL | 功能缺陷：export→解包后 `diff --no-xattrs` 仍把 `/baz/alink`、`/somelink` 标为 M | `export`/`import` 的符号链接往返 + `diff` | P1 |
| `test-fsck-collections` | FAIL | 功能缺陷：删 commit 后 `fsck` 报 `error: Object missing: …commit` 但退出码 0，测试 `assert_not_reached` | `fsck` 的错误累积与退出码（collection refs） | P1 |
| `test-fsck-delete` | FAIL | 缺命令：`error: pull-local: not implemented` | `pull-local`（本地对象/引用复制） | P1 |
| `test-libarchive` | FAIL | 功能缺陷：`fsck -q` 误报 `error: Repository corruption encountered`（content object 按 `.file` 查找失败） | `fsck` 各仓库模式的对象查找/校验 | P1 |
| `test-local-pull` | FAIL | 缺命令：`error: pull-local: not implemented` | `pull-local`（本地对象/引用复制） | P1 |
| `test-local-pull-depth` | FAIL | 缺命令：`error: pull-local: not implemented` | `pull-local`（本地对象/引用复制） | P1 |
| `test-prune-collections` | FAIL | 缺选项：`commit --timestamp='1985-10-25 00:00:00 +0000'` → `error: Invalid timestamp` | `commit --timestamp` 的日期解析（空格/时区） | P1 |
| `test-refs` | FAIL | 缺选项：`ostree summary -u` → `error: Unknown option -u`（refspec 已通，已推进到 summary） | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P1 |
| `test-remote-refs` | FAIL | 缺选项：源仓 `summary -u` → `error: Unknown option -u` | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P1 |
| `test-signed-commit-dummy` | FAIL | 缺选项：`sign --sign-type=dummy` → `error: Unknown option --sign-type=dummy` | GPG 双后端、`commit --gpg-sign`/`--sign`、`sign`、`remote gpg-import`/`gpg-list-keys` | P1 |
| `test-signed-commit-ed25519` | FAIL | 缺选项：`commit --sign=… --sign-type=ed25519` → `error: Unknown option --sign=…` | GPG 双后端、`commit --gpg-sign`/`--sign`、`sign`、`remote gpg-import`/`gpg-list-keys` | P1 |
| `test-signed-commit-spki` | FAIL | 缺选项：`commit --sign=… --sign-type=spki` → `error: Unknown option --sign=…` | GPG 双后端、`commit --gpg-sign`/`--sign`、`sign`、`remote gpg-import`/`gpg-list-keys` | P1 |
| `test-summary-collections` | FAIL | 缺选项：`summary --update` → `error: Unknown option --update` | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P1 |
| `test-summary-update` | FAIL | 缺选项：`summary --update` → `error: Unknown option --update` | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P1 |
| `test-oldstyle-partial` | FAIL | 缺选项：`pull origin main --subpath /baz` → `error: Unknown option --subpath` | `pull --depth/--subpath/--mirror/--commit-metadata-only/--http-header/--append-user-agent` | P2 |
| `test-prune` | FAIL | 缺选项：`pull --depth=-1` → `error: Unknown option --depth=-1` | `pull --depth/--subpath/--mirror/--commit-metadata-only/--http-header/--append-user-agent` | P2 |
| `test-pull-bare` | FAIL | 缺命令：`error: pull: not implemented` | `pull`（http/file transport、summary/objects 下载、错误文案） | P2 |
| `test-pull-bareuser` | FAIL | 缺命令：`error: pull: not implemented` | `pull`（http/file transport、summary/objects 下载、错误文案） | P2 |
| `test-pull-bareuseronly` | FAIL | 缺命令：`error: pull: not implemented` | `pull`（http/file transport、summary/objects 下载、错误文案） | P2 |
| `test-pull-basicauth` | FAIL | 缺命令：`error: pull: not implemented`（断言期望 401 文案） | `pull`（http/file transport、summary/objects 下载、错误文案） | P2 |
| `test-pull-collections` | FAIL | 缺选项：源仓 `summary -u` / `--update` → `error: Unknown option` | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P2 |
| `test-pull-commit-only` | FAIL | 缺选项：`pull --commit-metadata-only` → `error: Unknown option --commit-metadata-only` | `pull --depth/--subpath/--mirror/--commit-metadata-only/--http-header/--append-user-agent` | P2 |
| `test-pull-contenturl` | FAIL | 缺选项：源仓 `summary -u` / `--update` → `error: Unknown option` | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P2 |
| `test-pull-depth` | FAIL | 缺选项：`pull --depth=0` → `error: Unknown option --depth=0` | `pull --depth/--subpath/--mirror/--commit-metadata-only/--http-header/--append-user-agent` | P2 |
| `test-pull-large-metadata` | FAIL | 缺命令：`error: pull: not implemented`（断言期望 `exceeded maximum`） | `pull`（http/file transport、summary/objects 下载、错误文案） | P2 |
| `test-pull-localcache` | FAIL | 缺命令：`error: pull: not implemented` | `pull`（http/file transport、summary/objects 下载、错误文案） | P2 |
| `test-pull-metalink` | FAIL | 缺选项：源仓 `summary -u` / `--update` → `error: Unknown option` | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P2 |
| `test-pull-mirror-summary` | FAIL | 缺选项：源仓 `summary -u` / `--update` → `error: Unknown option` | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P2 |
| `test-pull-mirrorlist` | FAIL | 缺命令：`error: pull: not implemented` | `pull`（http/file transport、summary/objects 下载、错误文案） | P2 |
| `test-pull-override-url` | FAIL | 缺选项：`pull --mirror --depth=-1` → `error: Unknown option --mirror` | `pull --depth/--subpath/--mirror/--commit-metadata-only/--http-header/--append-user-agent` | P2 |
| `test-pull-repeated` | FAIL | 缺选项：`pull --mirror` → `error: Unknown option --mirror`（断言期望 HTTP 500） | `pull --depth/--subpath/--mirror/--commit-metadata-only/--http-header/--append-user-agent` | P2 |
| `test-pull-resume` | FAIL | 缺命令：`error: pull: not implemented`（断言期望 `Download incomplete`） | `pull`（http/file transport、summary/objects 下载、错误文案） | P2 |
| `test-pull-sizes` | FAIL | 缺选项：`pull --commit-metadata-only` → `error: Unknown option --commit-metadata-only` | `pull --depth/--subpath/--mirror/--commit-metadata-only/--http-header/--append-user-agent` | P2 |
| `test-pull-subpath` | FAIL | 缺选项：`pull --subpath=…` → `error: Unknown option --subpath=…` | `pull --depth/--subpath/--mirror/--commit-metadata-only/--http-header/--append-user-agent` | P2 |
| `test-pull-summary-sigs` | FAIL | 缺选项：源仓 `summary -u` / `--update` → `error: Unknown option` | `summary --update`/`-u`、summary 文件、refs→summary 联动 | P2 |
| `test-pull-untrusted` | FAIL | 功能缺陷：`fsck -q` 误报 `error: Repository corruption encountered`（content object 按 `.file` 查找失败） | `fsck` 各仓库模式的对象查找/校验 | P2 |
| `test-pull2-bareuseronly` | FAIL | 缺命令：`rev-parse exampleos/42/x86_64/main^` → `error: Ref …^ not found` | `rev-parse` 的 `^`/`~` 父提交语法 | P2 |
| `test-remote-add` | FAIL | 功能缺陷：`fsck -q` 误报 `error: Repository corruption encountered`（content object 按 `.file` 查找失败） | `fsck` 各仓库模式的对象查找/校验 | P2 |
| `test-remote-add-collections` | FAIL | 缺选项：`remote add --gpg-import=…` → `error: Unknown option --gpg-import=…` | GPG 双后端、`commit --gpg-sign`/`--sign`、`sign`、`remote gpg-import`/`gpg-list-keys` | P2 |
| `test-remote-cookies` | FAIL | 缺命令：`remote add-cookie` → `not implemented`（`pull` 亦未实现） | `remote add-cookie`/`delete-cookie`、cookie 传输 | P2 |
| `test-remote-gpg-import` | FAIL | 缺选项：`remote gpg-import --keyring …` → `error: Unknown option --keyring` | GPG 双后端、`commit --gpg-sign`/`--sign`、`sign`、`remote gpg-import`/`gpg-list-keys` | P2 |
| `test-remote-gpg-list-keys` | FAIL | 缺命令：`remote gpg-list-keys: not implemented` | GPG 双后端、`commit --gpg-sign`/`--sign`、`sign`、`remote gpg-import`/`gpg-list-keys` | P2 |
| `test-remote-headers` | FAIL | 缺选项：`pull --http-header … --append-user-agent …` → `error: Unknown option --http-header` | `pull --depth/--subpath/--mirror/--commit-metadata-only/--http-header/--append-user-agent` | P2 |
| `test-signed-pull` | FAIL | 缺选项：`commit --sign=dummysign` → `error: Unknown option --sign=dummysign` | GPG 双后端、`commit --gpg-sign`/`--sign`、`sign`、`remote gpg-import`/`gpg-list-keys` | P2 |
| `test-signed-pull-summary` | FAIL | 缺选项：`commit --sign-type=dummy` → `error: Unknown option --sign-type=dummy` | GPG 双后端、`commit --gpg-sign`/`--sign`、`sign`、`remote gpg-import`/`gpg-list-keys` | P2 |
| `test-admin-boot-counting-tries` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-2` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-bootid-gc` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-bootprefix` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-clean` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-emptyetc` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-etcmerge-cornercases` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-grub2` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-karg` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-nomerge` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-none` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-switch` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-syslinux` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-uboot` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-deploy-var` | FAIL | 输出不一致：`has_ostree_feature initial-var` 失败 → `fatal missing initial-var`（`--version` Features 未声明） | `--version` Features + admin 的 initial-var 逻辑 | P3 |
| `test-admin-deploy-whiteouts` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-gpg` | FAIL | 缺选项：`commit --gpg-sign=…` → `error: Unknown option --gpg-sign=…` | GPG 双后端、`commit --gpg-sign`/`--sign`、`sign`、`remote gpg-import`/`gpg-list-keys` | P3 |
| `test-admin-instutil-set-kargs` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-kargs` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-locking` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-pull-deploy-commit` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-pull-deploy-split` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-stateroot` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-upgrade-endoflife` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-upgrade-not-backwards` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-upgrade-systemd-update` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-admin-upgrade-unconfigured` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-no-initramfs` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-osupdate-dtb` | FAIL | 缺命令：`error: admin init-fs: not implemented`（--bootable/commit/fsck 已通，止步于 init-fs） | `admin init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/`cleanup` + bootloader | P3 |
| `test-delta-fallocate` | FAIL | 需要外部工具：脚本要求 `OSTREE_UNINSTALLED`，harness 显式 unset → `OSTREE_UNINSTALLED: unbound variable` | harness/C helper（非实现缺口） | — |
| `test-symbols` | FAIL | 需要外部工具：需要 C 共享库 `.libs/libostree-1.so` + `eu-readelf`（未安装） | harness/C helper（非实现缺口） | — |

## 3. 阻塞归因汇总（按能力聚合）

| 能力/缺口 | 解锁脚本数 | 分类 | 优先级 | 典型首错 |
|---|---:|---|---|---|
| admin 部署栈 | 27 | 缺命令 | P3 | `error: admin init-fs: not implemented` |
| pull 全栈（核心 8 + 选项 9 + pull-local 4） | 21 | 缺命令/缺选项 | P1–P2 | `error: pull: not implemented` / `Unknown option --depth` |
| `summary --update`/`-u` | 10 | 缺选项 | P0–P2 | `error: Unknown option --update` |
| GPG / 签名 | 9 | 缺选项 | P1–P3 | `error: Unknown option --gpg-sign=…` |
| `fsck` 正确性 | 7 | 输出不一致（功能缺陷） | P1–P2 | `error: Repository corruption encountered` |
| `static-delta` | 2 | 缺选项 | P1 | `Unknown option --empty/--from` |
| 单点缺陷（10 项） | 10 | 混合 | P0–P3 | 见 §2 表 |
| harness/外部工具 | 2 | 需要外部工具 | — | `OSTREE_UNINSTALLED: unbound variable`、缺 `eu-readelf` |

### 3.1 按任务给定分类统计

| 分类 | 数量 | 备注 |
|---|---:|---|
| 缺命令 | 44 | admin、pull、pull-local、env、show、remote cookie 等 |
| 缺选项 | 30 | summary、pull 选项、GPG/签名、delta、timestamp |
| 输出不一致/功能缺陷 | 12 | fsck 误报 corruption（7）、checkout -H、export/diff、fsck 退出码、refs --collections、timestamp 解析 |
| 需要 root | 3 | 均为 SKIP（CONFORMANCE 容器内非特权口径） |
| 需要外部工具 | 13 | gpgme/ed25519/libarchive/gjs/fusermount/eu-readelf/C helper |
| 环境限制 | 2 | reflink、`user.*` xattr |
| 范围外 | 2 | composefs（2） |

## 4. 按当前实现，最多能通过哪些测试

1. **真实通过上限 = 5 个脚本**（5 PASS；TAP 断言 8 条、失败断言 0）。
   - `test-config`（4 TAP）、`test-help`（1）、`test-init-collections`（1）、`test-backcompat-fsck`（2）、`test-reset-nonlinear`（1）。
2. **再加 SKIP 后 harness 上限 = 23/111**。18 个 SKIP 中：范围外 2、需要 root 3、需要外部工具 11、环境限制 2。
3. **T2 距「单点修复即可转 PASS」最近的脚本**（都已推进到很靠后的断言，修复一处即可，按代价从低到高）：
   - `test-cli-extensions`：只缺 `ostree env`（1 个命令 + 扩展分发），修好即 PASS；
   - `test-commit-timestamp`：只缺 `show`（T2 之后源码已出现 `show.go`；但报告写作时新构建失败，未复核）；
   - `test-prune-collections`：只缺 `commit --timestamp` 的日期解析；
   - `test-pull2-bareuseronly`：只缺 `rev-parse <ref>^`；
   - `test-fsck-collections`：只缺 fsck 退出码/错误累积；
   - `test-basic-user`：只缺 `checkout -H` 的模式校验；
   - `test-export`：只缺 export/import 的符号链接往返（diff 误报 M）。
   - （`test-refs-collections`、`test-admin-deploy-var` 虽只差一个断言，但后续还会撞 `pull`/`admin`，不计入。）
4. **不要用 SKIP 冒充通过**：`conformance.sh` 把 SKIP 计为成功（仅 SKIP 的测试仍算通过、脚本可 `exit 0`）；T2 全量 88 FAIL，但“PASS+SKIP=23/111”很容易被误读成没问题。验收必须解析 `1..0 # SKIP`，只统计真 PASS。

## 5. 下一批最值得实现的 5 个功能（按解锁脚本数排序）

| 排名 | 功能 | 解锁脚本数 | 优先级 | 说明 |
|---|---|---:|---|---|
| 1 | admin 部署栈（`init-fs`/`stateroot-init`/`deploy`/`status`/`upgrade`/bootloader） | 27 | P3 | 数量最大但工程量也最大；`commit --bootable`/`fsck` 已通，现在 27 个脚本统一卡在 `admin init-fs` |
| 2 | pull 全栈（`pull` + 选项 + `pull-local`） | 21 | P1–P2 | 其中 `pull-local` 4 个是最便宜的（无 transport）；pull 核心 8 个；选项 9 个 |
| 3 | `summary --update`/`-u` | 10 | P0–P2 | 10 个脚本（含 refs/remote-refs）现在都卡在同一个选项，是**单点收益最高**的一项 |
| 4 | GPG / 签名（`--gpg-sign`、`--sign`、`sign --sign-type`、`gpg-import`） | 9 | P1–P3 | 9 个脚本；纯 Go OpenPGP 可先覆盖 `--gpg-sign`/`gpg-import` |
| 5 | `fsck` 正确性（消除 corruption 误报） | 7 | P1 | 7 个脚本；**不是缺功能而是 bug**，修复成本最低，且挡住 bare-user-only/基础仓库 |

**推荐执行顺序（P0/P1 优先，而非纯数量）**：

1. `summary --update`/`-u`（10 个脚本，单点；同时让 refs/remote-refs 继续前进）
2. `fsck` 误报修复（7 个脚本，纯 bugfix；T2 已能跑 fsck，收益立竿见影）
3. `pull-local`（4 个脚本，无 transport，是 pull 全栈的热身）
4. `pull` 核心 + 选项（8 + 9 个脚本，工具链大头）
5. GPG/签名（9 个脚本，可与 pull 并行）
6. admin 部署栈（27 个脚本，P3，等前面稳定后再启动）

## 6. 关键发现（供实现方/排期参考）

1. **`--version` 的 Features 会改变测试是否 SKIP**：T2 声明了 `libarchive` 后，`test-export`/`test-libarchive` 立刻从 SKIP 变成 FAIL（export 的 diff 误报、libarchive 的 fsck corruption）。在子系统真正可用前不要声明 feature；`internal/cli/app.go` 的注释已经写明这一点。
2. **`fsck` 是当前最大的“已实现但不正确”项**：T2 的 `fsck` 让 `test-backcompat-fsck`/`test-reset-nonlinear` 转 PASS，但在 bare-user-only/bare 仓库上报 `fsck content object …: object not found: …file: no such file or directory` 并最终 `Repository corruption encountered`，一次修复可解锁 7 个脚本。
3. **`summary -u` 是 refs 类测试的新瓶颈**：T2 修好 refspec 后，`test-refs`/`test-remote-refs` 都推进到 `summary -u`，说明 refs 主体已可用，只差 summary。
4. **pull 测试并不需要外网**：`test-pull-metalink`/`mirrorlist`/`basicauth`/`resume` 全部用本机 httpd；P2“网络”实际是“HTTP transport 能力”，可在无网环境完整验证。
5. **`test-delta-fallocate` 与 `test-symbols` 不是实现缺口**：前者要求 `OSTREE_UNINSTALLED`（harness 显式 unset），后者要求 C `.so` + `eu-readelf`。`conformance.sh --all` 已把它们排除；本报告为完整性用 `--test` 单独跑了它们。
6. **SELinux/root 类 SKIP 是环境限制**：`test-basic` 需要 SELinux relabel；`test-basic-bare-split-xattrs`、`test-repo-finder-mount-integration` 需要 sudo/USB。这些不是 Go 实现能单独解决的，验收需要在特权容器/VM 中跑。
7. **`test-xattrs` 与 `test-payload-link` 是上游/环境无条件 SKIP**，不应计入“实现未覆盖”。

## 7. 复现方法

```bash
# 单测（约 1–60s；失败日志在 builddir/logs/<name>.log）
cd /root/Src/ostree/go
./hack/conformance.sh --test test-refs --timeout 120

# 全量（111 个，含默认排除项；每个 120s 超时）
for t in $(ls ../tests/test-*.sh | xargs -n1 basename | sed 's/\.sh$//'); do
  ./hack/conformance.sh --test "$t" --timeout 120 >/dev/null 2>&1
  echo "$t rc=$?"
done

# 本次分诊使用的逐测隔离跑法（每个测试独立 builddir，避免 httpd 端口冲突）
# 见 /tmp/run-sweep.sh（cp -a 预构建 builddir，--no-build）
```

日志目录：`/tmp/conf-run2/<test>/builddir/logs/<test>.log`（T2 全量）与 `/tmp/conf-run/<test>/…`（T1 基线）。  
首错提取方法：对每个 FAIL 用 `bash -x` 重跑（`/tmp/xtrace.sh`），取 `run_exit_cmds` 之前最后一条失败命令与其后第一条 `error:`。

## 附录 A：T1 → T2 快照差异

| 项 | T1（13:42Z，`0d79551d7656…`） | T2（13:53Z，`a273e4ab9dd8…`） |
|---|---|---|
| PASS | 3（config、help、init-collections） | **5**（+backcompat-fsck、+reset-nonlinear） |
| SKIP | 20 | 18（export、libarchive 转为 FAIL） |
| FAIL | 88 | 88 |
| 明显推进 | — | `commit --bootable` 可用；`fsck`/`reset` 落地；refspec 可用；admin 组统一推进到 `admin init-fs`；refs 组推进到 `summary -u` |
| 新暴露问题 | — | `--version` 声明 `libarchive` 导致 2 个测试转 FAIL；`fsck` 误报 corruption（7 个测试，含 bare-user-only/bare 仓库） |

> T2 之后源码继续变化（`summary.go`/`show.go`/`log.go`/`ls.go`/`checkout.go` 等），因此上表只代表 T2 构建时刻。若这些命令已接线，`test-commit-timestamp`、`test-cli-extensions`、若干 `summary` 测试的结论会变好；请以新构建重跑为准。

## 附录 B：SKIP 清单与原因（18）

| 测试 | SKIP 原文 | 归类 |
|---|---|---|
| `test-basic` | this test requires SELinux relabeling support | 需要 root/SELinux |
| `test-basic-bare-split-xattrs` | this test needs sudo, skipping without OSTREE_TEST_SUDO | 需要 root |
| `test-repo-finder-mount-integration` | this test needs sudo … + MOUNT_INTEGRATION_DEV | 需要 root/USB |
| `test-commit-sign` | no gpgme support compiled in | 需要外部工具 |
| `test-gpg-signed-commit` | no gpgme support compiled in | 需要外部工具 |
| `test-create-usb` | no gpgme support compiled in | 需要外部工具 |
| `test-find-remotes` | no gpgme support compiled in | 需要外部工具 |
| `test-parent` | no gpgme support compiled in | 需要外部工具 |
| `test-pull-summary-caching` | no gpgme support compiled in | 需要外部工具 |
| `test-delta-ed25519` | no sign-ed25519 support compiled in | 需要外部工具 |
| `test-pre-signed-pull` | `ok … # SKIP due ed25519 unavailability` | 需要外部工具 |
| `test-demo-buildsystem` | no fusermount | 需要外部工具 |
| `test-rofiles-fuse` | no fusermount | 需要外部工具 |
| `test-pull-corruption` | no gjs | 需要外部工具 |
| `test-payload-link` | no reflinks | 环境限制 |
| `test-xattrs` | 上游无条件 SKIP（user.* xattr 无使用场景） | 环境限制 |
| `test-composefs` | no composefs support compiled in | 范围外 |
| `test-admin-deploy-composefs` | no composefs support compiled in | 范围外 |

## 附录 C：与 `GO-REWRITE.md` 验收定义的对照

- `GO-REWRITE.md` 要求「111 个中除 2 个 composefs 外 109 个全部 pass，不允许 skip 冒充」。T2：PASS 5、FAIL 88、SKIP 18，距 109/109 还差 **104 个脚本**（不含 2 个 composefs SKIP）。
- 按 §5 推荐顺序推进时，预估解锁节奏：summary(10) → fsck 修复(7) → pull-local(4) → pull(17) → GPG(9) → admin(27)；其中单点缺陷（env/show/timestamp/rev-parse^/checkout -H/export/fsck-collections）约 7 个，穿插修复成本最低。
- 需要 root/VM/FUSE/SELinux 的 3 个 SKIP（`test-basic`、`test-basic-bare-split-xattrs`、`test-repo-finder-mount-integration`）必须在特权环境单独验收，本报告的 PASS 统计不含它们。
