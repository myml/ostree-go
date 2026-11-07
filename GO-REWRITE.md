# OSTree Go 重写方案

> 目标上游：OSTree 2025.2（`1bc34f37`）。
> 目标产物：纯 Go、无 cgo、可直接替换 `/usr/bin/ostree` 的单一二进制。
> 状态：v1.3（10 个子系统调研 + 7 项决策已确认）。

## 0. 结论先行

这是一次**引擎级重写**：CLI 约 1.5 万行 C，底层 libostree 约 7.1 万行 C。可行，但必须按层推进、以「与 C 版对拍」为硬门槛。

已确认的边界：

- 代码放在仓库的 `go/` 子目录，独立 Go module；
- **不做 composefs**（本阶段）；启用 composefs 的仓库必须明确报错，不能生成坏镜像；
- GPG **双后端**：纯 Go OpenPGP 与调用 `gpg` 命令行都支持，可切换；
- **不提供安装集成**（不写 `make install`），测试通过 PATH 使用构建产物；
- 验收要求 **100%**，不接受「90%/85% 逐步收敛」的门槛。

建议按 **M0 基础设施 → M1 只读 → M2 本地写 → M3 网络 → M4 部署 → M5 高级** 六个阶段交付。完整覆盖常用命令并跑通上游非特权 shell 测试，约需 **10–16 人月**；并行可压缩日历时间，但 M0 接口冻结与 M2 格式对拍是关键路径。

### 「100%」的明确定义

1. 上游 111 个 `tests/test-*.sh` 中，**除 2 个 composefs 测试外，109 个必须全部通过**（pass，不允许用 skip 冒充）；composefs 两个测试本阶段明确排除，若日后要求包含，则必须重新把 composefs 纳入范围。
2. 需要 root / mount / FUSE / 特定 bootloader 的用例，在对应容器或 VM 环境中同样要求 100% 通过，不能以「本机不支持」长期跳过。
3. SELinux：代码按全量实现（§5.9），但当前只有 Debian 环境（无 SELinux），无法做标签/重启验证；VM 验证列为已知缺口，不计入本次 109/109。
3. 双向互操作矩阵全绿：C 建仓 Go 读写、Go 建仓 C `fsck --all` 干净。
4. 同一 tree + metadata 下，Go 生成的 commit checksum 与 C 版逐字节相同。
5. help / version / 错误文案 / 退出码 golden diff 为 0。
6. C 单元测试可作为「反向验证器」读取并检查 Go 生成的仓库。

## 1. 范围与边界

### 做

- 26 个顶层命令、`remote` 11 个子命令、`admin` 23 个子命令、`static-delta` 子命令；
- 五种仓库模式：`bare`、`bare-user`、`bare-user-only`、`archive`（别名 `archive-z2`）、`bare-split-xattrs`；
- 对象格式、refs、config、summary、static delta、GPG（双后端）/ed25519、pull（file/http）、checkout/commit、sysroot 部署、bootloader 配置生成；
- **完整 SELinux 支持**：策略查询、`--selinux-policy` 打标、`restorecon`、`setfscreatecon`、deploy relabel（详见 §5.9）；
- 上游 shell 测试可直接以 Go 二进制运行。

### 不做 / 明确降级

- **不实现 libostree C ABI**（不产出 `libostree-1.so`）；`tests/test-*.c` 不作为 Go 二进制的测试，只作为格式反向校验工具。
- 不重写 `rofiles-fuse`、`ostree-trivial-httpd`、`ostree-prepare-root` 等独立程序（测试用 httpd 用 Go 写等价实现）。
- 非 Linux 平台不支持。
- **composefs 本阶段不做**：repo 配置启用 composefs 时返回明确错误；不影响普通（非 composefs）部署。
- `avahi`/LAN finder 可后置；`instutil` 隐藏子命令、soft-reboot、state-overlay 可后置，未实现时返回清晰错误。
- 性能不作为验收项，但不得出现 O(n²) 或整文件入内存的实现。
- 不提供 `make install` / 打包集成；产物在 `go/bin/ostree`，测试通过 PATH 或软链使用。

## 2. 架构

```
┌──────────────────────────────────────────────────────────────┐
│ L1 go/cmd/ostree + go/internal/cli                           │
│    命令表 / GOption 兼容解析 / help 渲染 / 分发 / 退出码      │
├──────────────────────────────────────────────────────────────┤
│ L2 go/internal/cmd{,/admin,/remote,/delta}                   │
│    每个命令的薄适配：选项 → 服务调用 → 输出格式化              │
├──────────────────────────────────────────────────────────────┤
│ L3 go/pkg/{commit,checkout,diff,fsck,prune,pull,deploy, │
│                  summary,findremotes,createusb}              │
│    用例编排；不直接做 I/O 细节                                │
├──────────────────────────────────────────────────────────────┤
│ L4 子系统                                                    │
│  repo（布局/mode/config/refs/lock/对象存储/summary）          │
│  object（commit/dirtree/dirmeta/file header/xattrs 编解码）   │
│  transport（file/http、重试、cookie、metalink、mirrorlist）   │
│  staticdelta（superblock/part/rollsum/bsdiff）                │
│  sysroot（deployments/origin/BLS/bootloader）                 │
│  bootloader（grub2/syslinux/uboot/zipl 适配）                 │
│  sepolicy（SELinux 策略查询与打标）                           │
├──────────────────────────────────────────────────────────────┤
│ L5 基础库                                                    │
│  gvariant / checksum / compress / crypto / xattr / lock /    │
│  util（openat、原子写、staging、fsync、目录遍历）             │
└──────────────────────────────────────────────────────────────┘
```

**依赖规则**：仅允许上层 import 下层；同层不互相 import（跨子系统通过参数/接口）。CI 用 `go list -deps` 做规则检查，禁止 `cmd` 直接 import `gvariant`。

## 3. 包结构（`go/` 子目录）

```
go/
  go.mod                          module github.com/myml/ostree-go
  cmd/ostree/main.go              仅 os.Exit(cli.Run(os.Args))
  internal/
    cli/                          app.go option.go help.go context.go errors.go
    cmd/                          cat.go ls.go log.go show.go refs.go ...
      admin/                      deploy.go status.go switch.go upgrade.go ...
      remote/                     add.go list.go refs.go summary.go ...
      delta/                      list.go show.go generate.go apply.go ...
  pkg/
    commit/                       mtree.go modifier.go devino.go sizes.go write.go
    checkout/                     checkout.go whiteout.go hardlink.go
    diff/  fsck/  prune/  traverse/
    pull/                         pull.go local.go
    findremotes/  createusb/
    deploy/                       deploy.go status.go upgrade.go switch.go
    repo/
      layout.go modes.go open.go create.go config.go refs.go lock.go
      loose.go staging.go summary.go cache.go
    object/
      commit.go dirtree.go dirmeta.go file.go xattrs.go validate.go checksum.go
    gvariant/                     type.go decode.go encode.go value.go
    staticdelta/                  format.go compile.go apply.go rollsum.go bsdiff.go
    transport/                    fetcher.go file.go http.go cookies.go metalink.go
    sysroot/                      sysroot.go deployment.go origin.go bls.go initfs.go
    bootloader/                   bootloader.go grub2.go syslinux.go uboot.go zipl.go
    sepolicy/                     config.go filecontexts.go label.go restorecon.go
    compress/                     deflate.go lzma.go zstd.go
    crypto/                       verifier.go openpgp.go gpg.go ed25519.go
    checksum/  xattr/  lock/  util/
  testdata/
    fixtures/                     C 版生成的仓库/对象（tar.zst + SHA256SUMS）
    golden/                       按 C 版 git describe 归档的输出
    fuzz/                         gvariant 语料
  hack/
    gen-fixtures.sh  gen-golden.sh  conformance.sh  interop.sh
```

## 4. 格式与兼容性要点（实现前的硬约束）

### 4.1 GVariant

| 对象 | 类型字符串 |
|---|---|
| commit | `(a{sv}aya(say)sstayay)` |
| dirtree | `(a(say)a(sayay))` |
| dirmeta / filemeta | `(uuua(ayay))` |
| file header（流式） | `(uuuusa(ayay))` |
| file header（archive） | `(tuuuusa(ayay))` |
| summary | `(a(s(taya{sv}))a{sv})` |
| summary sig | `a{sv}` |
| static delta superblock | `(a{sv}tayay` + commit + `aya(uayttay)a(yaytt))` |
| delta part | `(yay)`；payload `(a(uuu)aa(ayay)ayay)` |
| `ostree.sizes` 条目 | `ay` |

- 默认小端，但下列字段显式大端：`uid/gid/mode`、file header 的 `t`、commit timestamp、summary 的 `t`、delta 的 version/size、signed magic。
- 长度前缀 = **4 字节大端长度 + 4 字节 0 填充**，使数据 8 字节对齐；读取时先跳 8 字节。
- 变长容器 framing：尾部逆序写 u32 offset，最后一个变长元素不写；数组 stride 为对齐后元素大小。
- `v` 编码：子值 + 类型串 + NUL + 1 字节类型串长度。
- 字符串/字节串均带结尾 NUL；xattr 的 name 是含 NUL 的 `ay`。
- 只接受 normal form：非零 padding、越界/未对齐 offset、bool>1、dict 乱序、内嵌 NUL 一律拒绝。
- 核心格式不用 `maybe`；解码器遇到时明确报 unsupported，禁止 panic。

### 4.2 对象与校验和

- 类型：file(1)、dirtree(2)、dirmeta(3)、commit(4)、tombstone-commit(5)、commitmeta(6)、payload-link(7)、file-xattrs(8)、file-xattrs-link(9)。
- **meta 对象**：`sha256(磁盘上的 variant 字节)`（无长度前缀）。
- **目录**：内容 = dirmeta 序列化字节；校验和即其 sha256。
- **文件对象**：`sha256(len-prefixed header ‖ 未压缩内容)`（symlink 只算 header）；与仓库模式无关。
- **commit content checksum**：`sha256(root dirtree 校验和字节 ‖ root dirmeta 校验和字节)`，用于 `--skip-if-unchanged` 等。
- 路径：`objects/<csum[0:2]>/<csum[2:]>.<type>`；仅当**非 meta 且 archive 模式**追加 `z`，即 `.filez`（不是 `.file.z`）。
- 对象先写 `tmp/staging-<bootid>-*`，再 `linkat(O_TMPFILE, NOREPLACE)` 落位；EEXIST 视为已存在。

### 4.3 五种仓库模式

| 模式 | 内容存储 | 元数据 | 典型用途 |
|---|---|---|---|
| `bare` | 原样 | 文件系统 uid/gid/mode/xattr | 系统仓库 `/ostree/repo` |
| `bare-user` | 原样 | `user.ostreemeta` xattr（`(uuua(ayay))`，全 BE）；symlink 存为普通文件（target+NUL） | 非 root 用户仓库 |
| `bare-user-only` | 原样 | 无 xattr；uid/gid=0；mode 仅 0775/0664 位 | 只读/无特权 |
| `archive`（`archive-z2`） | **raw deflate** + file header（`.filez`） | 正常元数据 | 发布/传输 |
| `bare-split-xattrs` | 内容 `.file` | `.file-xattrs`（`a(ayay)`）+ `.file-xattrs-link` 硬链接 | 特殊文件系统 |

- `bare` 写对象时 mode 掩为 `(mode & (S_IFREG|0775)) | S_IRUSR`，mtime 固定 `OSTREE_TIMESTAMP=0`。
- `bare-user-only` 强制 canonical permissions、跳过 xattrs。
- `archive` 用 raw deflate（**无 zlib 头**），级别取 `[archive] zlib-level`，默认 6。
- 仓库「已存在」的唯一判据是 `objects/` 存在；init 顺序为 `tmp, extensions, state, refs, refs/heads, refs/mirrors, refs/remotes, objects`（objects 最后）。`deltas/`、`summary`、`summary.sig`、`.lock` 均懒创建。
- 目录 0775、文件 0660、`objects/xx` 0777（受 umask）。

### 4.4 config / refs / 锁

- config 是 GKeyFile 方言：`[core]` 与 `[remote "name"]`；列表用 `;`；注释在读写中丢失；组/键保序；同名 remote 不得与 `remotes.d/*.conf` 冲突。
- 本版本**没有** `core.commit-gpg-verify`，也**没有** `http-*` remote 键；有 `sign-verify`、`verification-<engine>-{key,file}`。
- `ostree config` 只有 `get`/`set`/`unset`（**没有 `list`**），选项只有 `--group`。
- refs：`refs/heads/<ref>`、`refs/remotes/<remote>/<ref>`、`refs/mirrors/<collection-id>/<ref>`；内容 `<sha256>\n`；别名是相对符号链接；写入用 tmp+rename（DATASYNC）。
- 锁：`<repo>/.lock`（0660，懒创建）。C 版优先 `F_OFD_SETLK`，`EINVAL` 才回退 `flock`。**Go 必须实现同一策略**，只调 `flock(2)` 会与 C 版不互斥。
- `core.locking=false` 完全不加锁；`lock-timeout-secs` 控制轮询。

## 5. 子系统设计

### 5.1 gvariant（L5）

签名驱动：`ParseType(string) -> Type`，`Decode(Type, []byte) -> Value`，`Encode(Value) -> []byte`。两遍编码（先算布局，再写数据并逆序回填 offset）。Value 模型覆盖 `y n q i u x t b d s o g ay a* (…) {…} v`；`a{sv}` 归一为 `a(sv)`。所有读取带边界检查，错误而非 panic。

测试：C 版 fixture 字节级 round-trip；Go 原生 fuzz（截断/篡改/offset 越界/嵌套 variant）。

### 5.2 repo / object（L4）

- `repo` 用 `*os.Root` + `openat` 风格接口（`dfd + relpath`），避免 chdir 竞态。
- 对象读写：`LoadFile(csum) (io.ReadCloser, FileInfo, Xattrs)`、`WriteFile(fi, xattrs, io.Reader)`；写侧 `TeeReader` 算 sha256，先写 header 再流式复制。
- 事务：staging 目录 → 批量 rename → `syncfs` → fsync objdirs → 更新 refs/summary。
- 压缩：archive 用 raw deflate（`compress/flate`），不用 `compress/zlib` 包装。
- xattr：按 name 字节序排序、去重、非空；`user.*` 与 `security.*` 权限差异要处理。

### 5.3 commit / checkout / diff / fsck / prune（L3+L4）

commit：
- 自底向上：目录先写 dirmeta，子目录递归，dirtree 按名字排序，最后组 commit；
- 过滤/statoverride/owner-uid/gid/timestamp（`SOURCE_DATE_EPOCH`）/sizes/ref-binding；
- devino 复用：扫描 loose `*.file` 建 `(dev,ino) -> csum`，命中直接写 mtree；
- `--consume`、`--skip-if-unchanged`、`--link-checkout-speedup`、`--fsync`、`--selinux-policy`。

checkout：
- 目录 0700 创建，末尾 chmod/chown，mtime=0；`--force-copy` 跳过 mtime；
- 普通文件优先硬链接（`bare` + checkout none、`bare-user` + user），EMLINK/EXDEV/EPERM 回退复制，0 字节必复制；
- 符号链接 `symlinkat` + `fchownat(NOFOLLOW)` + xattr；bare-user 的 symlink 是普通文件，走复制；
- whiteout：`.wh.N` 删除、`.wh..wh..opq` 清空、`.ostree-wh.` + `--process-passthrough-whiteouts` 转 `mknod c 0:0`；
- `--user-mode` 不 chown/不写 xattr/清 suid；SELinux 标签隐含 force-copy。

diff：类型不同即 modified；同类型比 csum，目录 csum 相同剪枝；输出 `"%c    %s\n"`（M/D/A + 4 空格 + 相对路径）。`--stats` 需复刻 `g_format_size_full` 的单位/舍入。

fsck：校验 refs（可选 binding）→ 枚举 commit → 遍历对象；meta 重算 sha256 并校验结构，file 重算 header+内容；输出 `Object missing: <csum>.<type>`；成功固定句 `object fsck of %d commits completed successfully - no errors found.`，失败 `Repository corruption encountered`。

prune：默认遍历全部 commit 求可达集，`--refs-only` 只走 refs+depth；exclusive 锁；输出 `Total objects: N` / `Would delete: N objects, freeing X` / `Deleted N objects, X freed`；tombstone 与 payload-link 规则按 C 版。

### 5.4 pull / transport（L4）

- 流程：remote 配置 → 先取 `summary.sig` 再 `summary`（条件请求 + 本地缓存）→ 无 summary 回退 `refs/heads/<ref>` → 按 depth 追 commit 闭包 → 优先 indexed/static delta，否则 loose → 校验后落盘 → 删除 `.commitpartial`。
- 传输：URL 裸拼；UA `ostree/<ver>`；**不支持 Range**，「断点续传」= 跳过已存在对象/partial；重试 5 次（仅瞬态错）；连接 30s、低速 1000B/30s；并发 8（delta part ≤2）。
- TLS：`tls-permissive`、CA、客户端证书；代理单键 `proxy`；HTTP/2 默认开。
- cookie：`<repo>/<remote>.cookies.txt`，Netscape 7 列格式。
- summary 缓存：`<repo>/cache/summaries/<remote>[.sig]`，etag 存 `user.etag` xattr。
- metalink/mirrorlist：mirrorlist 逐行、仅 http(s)，用 `config` 探测；metalink XML + sha256/sha512 校验 + 多 URL 回退。
- `pull-local`：转 `file://`；trusted 时优先 reflink/hardlink，否则校验复制；默认禁 delta。
- `create-usb`：在目标 FAT 上建 archive repo，用 collection-refs/mirror 拉取。
- `find-remotes`：config/mount 必须做；avahi/LAN 后置。

进度文案必须逐字对齐：`Receiving objects: N% (a/b) x/s y`、`N metadata, M content objects fetched; ...`。

### 5.5 static delta（L4）

布局与格式：

- 路径：`deltas/<to[0:2]>/<from[2:]>-<to[2:]>`；无 from 时省略前半段；文件名中的 `/` 以 `_` 替代。
- superblock：`(a{sv}tayay` + commit + `aya` + meta 数组 + `a` + fallback 数组 + `)`；`t` 为 BE 时间戳。
- meta entry：`(uayttay)` = version(0)、part 文件 SHA256、压缩后大小、原大小、对象列表（1 字节 objtype + 32 字节 csum）。
- part：`(yay)`，首字节 `x` 表示 xz（preset 8、CRC64），`0` 表示未压缩。
- 校验和：part 文件校验和 = 整份 part 文件 sha256；superblock digest = 整份 superblock sha256，写入 summary 的 `ostree.static-deltas`（`a{sv}`：delta 名 → digest）与 `delta-indexes/<to>.index`。
- 端序：version/size 等字段可被 `--set`/`swap-endianness` 反转，读取侧需按 metadata `ostree.endianness` 或启发式判断，不能只认一种。

生成：

- 计算 commit 闭包差集，按 metadata / 常规文件 / symlink 分类；
- 相似文件按大小 + basename 交集（±30%）匹配，rollsum 命中率 ≥50% 才用 delta，否则整文件；>4 MiB 的不可 delta 大文件进 fallback；part >32 MiB 切分；
- mode/xattr 去重后写入；
- bsdiff 使用 mendsley 实现（pin `b817e94`）的**自有容器**（内含 bzip2 块），不是 BSDIFF40；`bsdiff/` 子模块当前未检出，落地前必须从上游确认容器细节。生成侧可先提供 `--disable-bsdiff` 等价开关，应用侧必须先实现 bspatch。

应用：

- 已有全部对象则跳过；逐 part 校验 SHA256 → 解压 → 执行 opcode（`S` 整体、`o/w/c` 增量、`r/R` 读 from、`B` bspatch）；
- 关闭时重算 SHA256 与 objects 比对；无 per-part 回滚；
- offline 应用遇到 fallback 直接报错（不下载完整对象）。

rollsum / 压缩：

- 窗口 64；`s1 = 64*31`，`s2 = 64*63*31`；add 时 `s1 += add - drop`，`s2 += s1 - 64*(drop+31)`；digest = `(s1<<16) | (s2 & 0xffff)`；切点条件 `s2 & 8191 == 8191`；块上限 32768，另用 crc32 分桶。Go 实现必须用 uint32 回绕。
- xz 用 `ulikunitz/xz`（32 MiB 字典，纯 Go 约为 C 的 1/3 速度）；part 级 `io.Pipe` → xz → 临时文件，同步算 SHA256；解码必须设字典/输出上限（C 侧 100 MiB memlimit，CVE 防护）。

CLI 与输出：

- `static-delta list` 每行一个名字；`show` 输出 `Delta:`/`From:`/`To:`/`Timestamp:`、`PartMeta`、`Total Size`，需逐字对齐。
- `static-delta generate/apply-offline/verify/indexes/reindex/delete` 一并实现。

接口建议：`ObjectSource`（按 csum 取对象/大小）、`PartBuilder`（写入并返回 checksum）、`Apply(repo, delta, opts)`；大文件用 `os.File`+`WriteAt`，避免整文件入内存。

### 5.6 sysroot / deploy（L4）

- 布局：`/ostree/repo`（bare）、`/ostree/deploy/<os>/{deploy,var,backing}`；`/ostree/boot.{0,1}` 链接农场指向 `deploy/<csum>.<serial>`；`/boot/loader` → `loader.{0,1}`，BLS 条目 `ostree-<n>-<index>.conf`；内核在 `/boot/ostree/<os>-<bootcsum>/`；`/run/ostree-booted` 是 GVariant vardict。
- deployment：目录名 `^[0-9a-f]+\.\d+$`；origin 文件 `[origin] refspec/override-commit/unlocked`、`[libostree-transient] pinned`；列表按 version 降序，index 0 默认，staged 置顶。
- BLS：固定键序，`ostree=`/`init=` 强制改写；`+left-done` 后缀；overlay initrd 用重复 `initrd=`。
- bootloader：`[sysroot] bootloader=auto|none|grub2|syslinux|uboot|zipl|aboot`；auto 探测 syslinux→grub2→uboot。grub2 需 `grub2-mkconfig`（或内置 generator），zipl 需 `zipl --secure`；其余可纯 Go 生成文本。
- admin：`deploy` 无 `--not-a-default`（是 `--not-as-default`）；`--lock-finalization` 隐含 `--stage`；`status` 行格式 `* <os> <csum>.<serial> (pending|rollback|staged|finalization locked|soft-reboot)`；`status --json` 字段必须逐字一致。
- staged：`/run/ostree/staged-deployment` 为 `a{sv}`（target/merge-deployment/kargs/overlay-initrds/bootconfig-extra/locked）；锁语义是文件存在性；`finalize-staged` 关机路径与 `boot-complete` 的 stamp 行为要复刻。
- 特权：booted + uid0 时 `unshare(CLONE_NEWNS)` 并 remount `/sysroot`、`/boot` rw；`/ostree/lock` flock 独占。

### 5.7 fsverity 与 composefs

- fsverity：`FS_IOC_ENABLE_VERITY`（sha256/4096，O_RDONLY fd）、`FS_IOC_MEASURE_VERITY`；`x/sys/unix` 足够。放在 M5。
- **composefs 不做**：不生成 `.ostree.cfs`，不实现 EROFS。repo 配置 `[ex-integrity] composefs=true` 或运行时启用时返回明确错误（如 `composefs support is not implemented`），普通部署路径不含该文件。
- 因此上游 `tests/test-composefs.sh`、`tests/test-admin-deploy-composefs.sh` 本阶段排除；其余测试不得因 composefs 缺失而失败。

### 5.8 CLI（L1）

- **自研解析器**（`flag`/`pflag` 均不兼容）：无长选项缩写、短选项聚合、`--opt=value`、`--` 语义、逐字错误文案。
- 两段式：先摘第一个非 `-` 参数作命令（命令专属选项可在命令前），再解析选项；`--repo PATH` 放命令前会把 PATH 当命令名（必须保留）。
- 命令表按 C 源序；`HIDDEN` 不进 help；支持外部扩展 `ostree-<verb>`（PATH/`$PKGLIBEXECDIR/ext`）。
- help：GLib 两列布局，列宽按最长选项串，空 `arg_description` 不显示 `=VALUE`，末尾 `\n\n`；`--version` 输出 YAML（Package/Version/Git/Features）。
- 错误：`error:` 到 stderr，颜色取决于 **stdout** tty；退出码仅 0/1；未知命令打 usage、解析失败不打、缺 repo 打命令 help + `Command requires a --repo argument`。
- 默认 repo：cwd 含 `objects/`+`config` → cwd；否则 `$OSTREE_REPO`；否则 `<sysroot>/ostree/repo`（实际 `/ostree/repo`）。

### 5.9 SELinux（全量实现）

C 版依赖 libselinux 的五项能力，Go 版逐项对等实现，不使用 cgo：

1. **策略根解析**：读 `/etc/selinux/config` 的 `SELINUXTYPE`，兼容 `/usr/etc/selinux`；`selinux_set_policy_root` 对应设置包内 policy root。
2. **标签查询**：`selabel_open(SELABEL_CTX_FILE)` + `selabel_lookup_raw` 对应解析 `contexts/files/file_contexts`（以及 `file_contexts.local`、`file_contexts.homedirs`），按相对路径 + `unix_mode` 返回 context；无匹配返回空（ENOENT 语义）。
3. **标签应用**：`lsetfilecon`/`lgetfilecon` 对应 `unix.Lsetxattr`/`Lgetxattr("security.selinux")`（NOFOLLOW），含 `KEEP_EXISTING`、`ALLOW_NOLABEL` 标志与 `/proc`→`/mnt` 特例。
4. **创建上下文**：`setfscreatecon_raw` 对应 `runtime.LockOSThread` + 写 `/proc/thread-self/attr/fscreate`，创建后清空；host 未启用 SELinux 时与 C 一致地静默忽略。
5. **策略校验和**：`sha256(/etc/selinux/<type>/policy/policy.<ver>)`，用于 summary/deploy 的 policy checksum。

匹配语义：

- file_contexts 是 PCRE 方言，Go `regexp` 是 RE2。先统计实际策略用到的语法（绝大多数是 `(/.*)?`、`[^/]+`、`\.` 等，RE2 兼容）；若遇到不兼容构造，实现一个小型兼容层，或解析 `file_contexts.bin`（编译后的状态机，工作量大但与 txt 等价）。
- 必须确认并复刻 libselinux 的**匹配优先级**（最后匹配 vs 最具体），用 Fedora fixture 逐条对拍 `matchpathcon`/`restorecon` 结果。
- 文件类型过滤 `-d/-f/-l/-c/-b/-p/-s`、`<<none>>`、MLS/MCS range 原样保留。

难点（也是之前建议「只做 xattr 传播」的原因）：

- 本机 Debian **没有启用 SELinux**（无 `/sys/fs/selinux`，`/etc/selinux` 仅有 `semanage.conf`），无法本地验证；必须准备 Fedora/SELinux VM 作为验收环境。
- `setfscreatecon` 是**线程级**状态，Go goroutine 会跨线程迁移，必须 `LockOSThread` 且不能在创建前让出；这是最容易写出隐蔽 bug 的点。
- file_contexts 的 PCRE 语法与匹配顺序、策略版本差异、`.bin` 格式，都需要真实策略 fixture 才能确认。
- 标签错误会导致目标系统无法启动，属于安全敏感路径，测试必须在 VM 中做「部署后重启」验证。
- deploy 阶段还要对 `/etc`、`/var` 做 relabel 与三方合并，逻辑与 `ostree-sysroot-deploy.c` 强耦合。

原方案建议「只做 xattr 传播」的理由是：仓库互操作只需要搬运已有 `security.selinux`，不依赖策略查询；而 `commit/checkout --selinux-policy` 与 `admin deploy` relabel 才需要真正的 selabel。既然验收要求完整兼容，本节按**全量**纳入 M5。

当前环境限制：只有 Debian（无 SELinux），因此：

- 策略解析、标签查询、`restorecon` 逻辑用**离线 fixture**（Fedora 策略文件 + 期望 `matchpathcon` 结果）做单测对拍；
- `setfscreatecon` 的线程行为在无 SELinux 环境下只能验证「静默忽略」路径，真正的标签写入需要 Fedora VM；
- Fedora VM 的部署+重启验证列为**已知缺口**，待环境具备后补门禁；在此之前不宣称「Fedora SELinux 行为已验证」。

### 5.10 GPG：双后端

统一接口：

```go
type Verifier interface { Verify(data, sig []byte, keyring KeyringRef) error }
type Signer   interface { Sign(data []byte, key KeyRef) ([]byte, error) }
```

- **gpg 后端**：`gpg --homedir <dir> --status-fd 1 --verify` / `--detach-sign`；完整兼容 keybox、agent、trustdb、gpgme 语义；`gpg-import`/`gpg-list-keys` 优先走它。
- **纯 Go 后端**：`github.com/ProtonMail/go-crypto/openpgp`，自行解析导出的公钥/私钥；用于无 gpg 环境与单元测试。
- **选择策略**：环境变量 `OSTREE_GPG_BACKEND=auto|gpg|go`（默认 `auto`：有 gpg 且 remote 使用 gpg 密钥环时用 gpg，否则 Go）；也允许 repo/remote 配置显式指定。
- **一致性测试**：同一密钥下 Go 签名 → gpg 验签、gpg 签名 → Go 验签、篡改后双方都拒绝；summary 签名与 commit 签名都覆盖。
- 纯 Go 后端的已知限制：keybox/trustdb/agent 语义不完整，遇到时明确报错并提示切换 `gpg` 后端，而不是静默失败。

## 6. 命令矩阵与阶段

| 阶段 | 命令 | 关键验收（100%） |
|---|---|---|
| **M1 只读** | `cat ls log show rev-parse refs diff export checksum config get fsck --quiet summary --view static-delta list/show remote list/show-url` | 同仓库输出逐字节一致；只读子集全部通过 |
| **M2 本地写** | `init commit checkout config set/unset refs 增删 reset prune fsck --add-tombstones summary --update static-delta generate/apply-offline/verify pull-local sign` | 双向 fsck 干净；同输入 commit csum 一致；本地写子集全部通过 |
| **M3 网络** | `pull remote 全部 find-remotes(config/mount) summary -u cookie metalink create-usb` | file/http 拉取一致；pull 子集全部通过 |
| **M4 部署** | `admin init-fs/os-init/deploy/status/switch/upgrade/undeploy/cleanup/pin/set-origin/set-default/config-diff/unlock/lock-finalization/kargs/post-copy/finalize-staged/boot-complete` | `status --json` 逐字一致；deploy 子集全部通过 |
| **M5 高级** | GPG 双后端、SELinux 全量、fsverity、spki、gpg-sign、soft-reboot、state-overlay、avahi | 签名互验；Fedora VM 中 SELinux 标签/重启通过 |

顶层命令共 26 个；`remote` 11 个子命令；`admin` 23 个子命令（含隐藏）。composefs 两个测试为明确排除项。

## 7. 测试与对拍

### 7.1 三层测试

1. **Go 单元测试**：每个 L4/L5 包；gvariant 覆盖率 ≥90%，核心包 ≥75%，总体 ≥70%；`go test -race`。
2. **Golden/黑盒**：编译 `ostree` 后 exec，断言 stdout/stderr/exit；用真实 C 版采集 golden（`LC_ALL=C`，记录版本），逐字节 diff；路径/时间做归一化。
3. **上游 shell 测试**：构造伪 builddir，把 Go 二进制命名为 `ostree` 放 `PATH` 首位（或软链 `tests/ostree`），设置 `G_TEST_SRCDIR`/`G_TEST_BUILDDIR`，逐脚本以 TAP 运行。
   - **不要用 `make check`**（会重编 C 并覆盖 PATH）；直接 `bash tests/test-X.sh`。
   - 必须实现 `--version` 的 YAML/Features，`has_ostree_feature` 据此决定 skip。
   - 不设置 `OSTREE_UNINSTALLED`，让依赖 C helper 的用例自然 skip；`test-delta-fallocate.sh` 需单独排除。
   - httpd 用 C 产物或 Go 等价实现；gpg home 复用上游数据。
   - 每例独立 `mktemp -d`，避免全局状态；`TEST_TMPDIR` 放非 overlay 卷（xattr 门控）。

### 7.2 双向对拍矩阵

| 方向 | 内容 | 断言 |
|---|---|---|
| C→Go | C 建五种模式仓库、commit、delta、summary | Go `refs/ls -R/cat/show/summary/fsck` 输出一致；对象 round-trip 字节相等 |
| Go→C | Go init/commit/checkout/pull-local/summary/prune | C `fsck --all` 返回 0；C `checkout` 成功 |
| 同输入 | 固定 `SOURCE_DATE_EPOCH`、uid/gid、TZ | commit checksum 逐字节相同 |
| 并发 | Go 持锁 ↔ C 阻塞，反之亦然 | 锁互认，无竞态损坏 |
| SELinux | Fedora VM 中 `commit/checkout --selinux-policy`、deploy | 标签与 C 版 `matchpathcon`/`restorecon` 一致 |
| GPG | Go/gpg 双后端交叉签名验签 | 双向通过，篡改失败 |

### 7.3 CI 与 100% 门禁

- `go-build`：fmt/vet/test/race、CGO_ENABLED=0。
- `go-interop`：容器内构建 C ostree（含 `ostree-trivial-httpd`），设 `OSTREE_C_BIN`，跑双向对拍。
- `go-conformance-readonly` / `go-conformance-write` / `go-conformance-deploy`：每个 job 必须 **0 fail / 0 error**；skip 不计入通过，且每个 skip 必须在另一个具备该能力的 job 中实际跑过。
- `go-selinux`（环境具备时）：Fedora SELinux VM，覆盖标签/重启；当前 Debian 无 SELinux，相关单测以 fixture 为主，不作为 109/109 的阻塞项。
- 最终汇总：109 个非 composefs 测试 **109/109 通过**；需要特权的用例在特权 job 中同样 100%。
- 指标仅用于展示：round-trip 率、C fsck 通过率、golden diff 数、子命令覆盖数；不作为放行依据。

### 7.4 不可复用 / 需 patch 的点

- C 单元测试链接 libostree，不能直接跑 Go；但可反向读取 Go 仓库做格式验证。
- Rust 绑定测试、installed tests、kola/TMT 需要 C 库或已启动宿主，后置。
- 上游脚本可能硬编码 `OSTREE_UNINSTALLED`（`test-delta-fallocate`）；用 denylist 而非改脚本。
- 未实现 feature 不要写进 `--version` Features，让相关用例自动 skip；但 composefs 的 2 个用例是明确排除，不是「靠 skip 混过」。
- help/错误文案差异用 expected-diff 白名单，逐条收敛，不做「顺手优化」。

## 8. 依赖

| 用途 | 依赖 | 说明 |
|---|---|---|
| syscall/xattr/ioctl | `golang.org/x/sys/unix` | OFD 锁、mount、fsverity、SELinux xattr |
| zstd | `github.com/klauspost/compress/zstd` | archive/增量 |
| xz/lzma | `github.com/ulikunitz/xz` | delta 压缩 |
| OpenPGP | `github.com/ProtonMail/go-crypto/openpgp` | 纯 Go 后端 |
| SELinux 辅助 | `github.com/opencontainers/selinux`（可选） | 仅用于 `GetEnabled`/xattr 辅助；策略查询自行实现 |
| bsdiff | 自行实现 | 需与 C 版分块兼容 |
| 其他 | 标准库 | flate、sha256、ed25519、net/http、os/exec（gpg） |

原则：零 cgo；依赖最小化；新增依赖需说明必要性与许可证。

## 9. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| GVariant 字节级细节 | 全盘不通 | 签名驱动 + C fixture + fuzz；M0 硬门槛 |
| OFD/flock 不互斥 | 并发损坏 | 实现 OFD + 回退；Go↔C 双进程锁测试 |
| GKeyFile 方言/注释丢失 | config 测试失败 | 按 C 行为实现，注释丢失可接受 |
| archive raw deflate 误解 | 对象不可读 | 明确不用 zlib 包装；fixture 校验 |
| GPG keybox/gpgme | 签名测试失败 | gpg 子进程后端兜底；Go 后端明确限制并提示切换 |
| SELinux 策略/线程级 fscreate | 标签错误、系统不可启动 | 全量实现；策略 fixture 对拍 + `LockOSThread` 单测；当前无 Fedora VM，重启验证后置 |
| composefs 不支持 | 2 个测试无法跑 | 明确排除并报错；日后需要时重新纳入（EROFS 写入器） |
| bsdiff 子模块未检出 | delta 生成/应用不全 | 先做 bspatch；生成侧 `--disable-bsdiff`；确认容器格式后补齐 |
| delta 端序反转 | 读不了跨端序 delta | 按 `ostree.endianness` metadata + 启发式双路径解析 |
| bootloader/系统工具 | deploy 不稳 | 与 C 版调用同一工具；固定容器版本 |
| 上游测试需 root/VM | 可测性差 | 分层 job；特权用例在特权环境必须 100% |
| 范围过大 | 长期不可交付 | 严格阶段门禁；每阶段可运行、可回归 |

## 10. 工作量估算（单人）

| 阶段 | 估算 | 说明 |
|---|---|---|
| M0 | 3–4 周 | gvariant/对象/仓库/CLI 骨架/对拍工具 |
| M1 | 3–4 周 | 只读命令 + golden |
| M2 | 5–7 周 | commit/checkout/fsck/prune/delta 基础 |
| M3 | 4–6 周 | pull/remote/缓存/重试 |
| M4 | 6–9 周 | sysroot/deploy/bootloader |
| M5 | 7–10 周 | GPG 双后端/SELinux 全量/fsverity/长尾 |

合计约 **10–16 人月**；并行可压缩日历时间，但 M0 接口冻结与 M2 格式对拍是关键路径。composefs 不做的节省与 SELinux 全量的增加基本相抵。

## 11. 已确认决策

1. **代码位置**：`go/` 子目录，独立 Go module（module path `github.com/myml/ostree-go`）。
2. **GPG**：双后端（纯 Go OpenPGP + `gpg` 命令行），`OSTREE_GPG_BACKEND=auto|gpg|go` 选择。
3. **SELinux**：全量实现，不做「只传 xattr」的降级。
4. **composefs**：本阶段不做；启用时明确报错；2 个 composefs 测试排除在 100% 之外。
5. **验收**：每个阶段 0 fail/0 error；最终 109/109（非 composefs）+ 特权环境全通过；composefs 2 个测试明确排除。
6. **安装**：不提供 `make install`，产物在 `go/bin/ostree`。
7. **SELinux 验证环境**：当前仅 Debian；代码全量实现，Fedora VM 的标签/重启验证后置为已知缺口。

## 12. M0 并行工作流（6 路）

| # | 工作流 | 产出 | 依赖 |
|---|---|---|---|
| 1 | `gvariant` 编解码 + fuzz | 可 round-trip 的库 + corpus | 无 |
| 2 | `object` 格式 | commit/dirtree/dirmeta/file header 编解码 + fixture 测试 | 1 |
| 3 | `repo` 存储 | 布局/mode/config/refs/`.lock`/loose 读写 | 1,2 |
| 4 | `cli` 框架 | 命令表、解析器、help/version、退出码 + golden | 无 |
| 5 | 对拍工具 | fixture 生成、伪 builddir、httpd、interop 脚本 | 无 |
| 6 | 只读原型 | 打开 C 仓库 + `refs`/`rev-parse`/`cat` 端到端 | 3,4 |

接口先冻结：`gvariant.Value`、`object` 与 `repo` 的公开类型、`cli.Context`。6 路可同时开工，2/3/6 在 1 完成后合并。
