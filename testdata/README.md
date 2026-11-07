# Go 重写：C-OSTree fixtures 与测试数据

本目录保存由 **C 版 ostree** 生成的、供 Go 实现做「C→Go 读取对拍」和单元测试使用的只读仓库 fixture。

- 生成工具：`go/hack/gen-fixtures.sh`
- 目标 C 版本：**OSTree 2025.2**（`/usr/bin/ostree`）
- 生成环境：Linux，root，文件系统支持 `user.*` xattr（本仓库的 fixtures 在 tmpfs/ext4 上生成）
- 生成时间戳：commit 固定为 `--timestamp=@0`；summary 与 static-delta superblock 中的 wall clock 通过一次性 `LD_PRELOAD` shim 固定为 0（详见下文）

> **不要手工修改 `fixtures/` 下的文件。** 需要变更时修改生成脚本并整体重新生成。

## 1. Fixture 清单

五种 mode 各一个目录：

| 目录 | `core.mode` | commit 数 | refs | static delta | 备注 |
|---|---|---|---|---|---|
| `bare/` | `bare` | 2 | `test2`, `stable` | 无 | 常规 bare，对象为物理文件/符号链接 |
| `bare-user/` | `bare-user` | 2 | `test2`, `stable` | 无 | uid/gid 存在对象元数据中 |
| `bare-user-only/` | `bare-user-only` | 2 | `test2`, `stable` | 无 | commit 使用 `--owner-uid=0 --owner-gid=0 --no-xattrs --canonical-permissions` |
| `archive/` | `archive`（`archive-z2`） | 2 | `test2`, `stable` | 有，commit1→commit2 | 含 `deltas/` 与 summary 中的 delta 索引 |
| `bare-split-xattrs/` | `bare-split-xattrs` | 1 | `test2`, `stable` | 无 | **来自上游只读 fixture**，原因见 §5 |

各 mode 的 commit checksum：

| 目录 | commit1 | commit2 |
|---|---|---|
| `bare/` | `bd58a67ac796c15cdb6b19955ec7f9eada17af186997bd845221f5f36f2714ae` | `37a73e27f3837e290c276f877eb236fbca3cfc7efd8c97b148ab79116d04fc27` |
| `bare-user/` | `bd58a67ac796c15cdb6b19955ec7f9eada17af186997bd845221f5f36f2714ae` | `37a73e27f3837e290c276f877eb236fbca3cfc7efd8c97b148ab79116d04fc27` |
| `bare-user-only/` | `b7d40d9616a7171906613bda2f2da83211e3ea15c5176ca1c2f9977404fbd2e2` | `445922550db73ecc87600b32656f741749ca30fb16ab7d7884dd52fbc5fc505e` |
| `archive/` | `bd58a67ac796c15cdb6b19955ec7f9eada17af186997bd845221f5f36f2714ae` | `37a73e27f3837e290c276f877eb236fbca3cfc7efd8c97b148ab79116d04fc27` |
| `bare-split-xattrs/` | `d614c428015227259031b0f19b934dade908942fd71c49047e0daa70e7800a5d` | 同 commit1（上游 fixture 只有一个 commit） |

`bare`、`bare-user`、`archive` 的 commit checksum 相同，因为树内容与提交元数据完全一致；对象在磁盘上的编码方式因 mode 而异。

## 2. 目录布局

每个 fixture 只包含只读仓库所需的部分：

```
<mode>/
  objects/          loose 对象（.commit/.dirtree/.dirmeta/.file/.filez/...）
  refs/             refs/heads/{test2,stable}
  config            [core] repo_version / mode
  summary           由 `ostree summary --update` 生成
  deltas/           仅 archive；一个 from→to 的 static delta
  SHA256SUMS        上述 payload 的校验和（仅常规文件）
  META              生成来源、C 版本、命令、时间、符号链接目标、内容说明
```

未包含 `tmp/`、`state/`、`extensions/`。因此 fixture 是**只读**的：直接对 fixture 运行 `ostree fsck --all` 或任何写操作会创建 `tmp/` 或 `.lock`，污染工作区。需要写入时请先复制到临时目录：

```bash
tmp=$(mktemp -d)
cp -a go/testdata/fixtures/archive/. "$tmp"/
/usr/bin/ostree --repo="$tmp" fsck --all
rm -rf "$tmp"
```

### SHA256SUMS

- 由 `sha256sum` 兼容的 `<sha256>  <relative-path>` 行组成，覆盖 `objects/`、`refs/`、`config`、`summary` 以及 `deltas/` 中的**常规文件**。
- `SHA256SUMS` 与 `META` 自身不在校验范围内。
- 符号链接对象的 target 不进入 `SHA256SUMS`（`sha256sum` 会解引用链接），而是完整列在 `META` 的 `[symlinks]` 段。`bare`、`bare-user-only`、`bare-split-xattrs` 中都有符号链接对象。

校验：

```bash
for d in go/testdata/fixtures/*/; do
    (cd "$d" && sha256sum -c SHA256SUMS) || exit 1
done
```

### META

关键字段（示例值见各目录）：

```
fixture_format=1
mode=archive
generated_at_utc=...          # 本次生成的实际 UTC 时间（不进入 SHA256SUMS）
fixture_epoch=0               # commit / summary / delta 使用的固定时间
ostree_binary=/usr/bin/ostree
ostree_version=2025.2
owner_uid=1000                # 除 bare-user-only 外的 commit owner
owner_gid=1000
commit1=... commit2=...
xattrs=true|disabled|...
fake_time=yes                 # 是否成功用 shim 固定了 wall clock
source=...                    # 正常生成或上游 fixture
payload=objects refs config summary [deltas]
sha256sums=regular files only; symlink object targets are listed below
```

随后是 `[ostree --version]` 原文、实际执行的 ostree/tar 命令（临时路径归一化为 `<REPO>`、`<TREE>`、`<TMP>`）、`[symlinks]` 与 `[contents]`。

## 3. 重新生成

```bash
# 全部五种 mode
go/hack/gen-fixtures.sh

# 只生成部分 mode / 换输出目录
go/hack/gen-fixtures.sh --modes "bare archive" --out /tmp/fixtures

# 不使用 xattr，或环境没有 C 编译器（会失去 summary 时间可复现性）
go/hack/gen-fixtures.sh --no-xattrs
go/hack/gen-fixtures.sh --no-fake-time

# 保留临时工作目录用于排查
go/hack/gen-fixtures.sh --keep
```

依赖：

- C ostree（默认 `/usr/bin/ostree`，可用 `--ostree` 或 `OSTREE_BIN` 覆盖）；
- `python3`（设置确定性的 `user.*` xattr）；
- `cc`（可选：编译固定 `g_get_real_time()` 的 `LD_PRELOAD` shim）；
- `tar`（仅 bare-split-xattrs 回退路径需要）；
- 文件系统支持 `user.*` xattr；`bare-split-xattrs` 还需要该 mode 本身的支持。
- 以 root 运行（commit 使用 `--owner-uid=1000 --owner-gid=1000`；可通过 `OWNER_UID`/`OWNER_GID` 环境变量调整）。

脚本会：

1. 在临时目录建树、init、两次 commit（`--timestamp=@0`）、创建 `stable` ref、`summary --update`；
2. archive 额外生成 `static-delta generate --from=<c1> --to=<c2>`，再更新 summary；
3. 用 C `fsck --all` 校验；
4. 复制 payload 到 `go/testdata/fixtures/<mode>`（先写 `.stage-*`，再整体替换）；
5. 生成 `SHA256SUMS` 与 `META`。

### 可重复性与幂等

- commit 时间固定为 `@0`，owner uid/gid 固定，`LC_ALL=C`、`TZ=UTC`、`umask 022`。
- `ostree summary` 的 `ostree.summary.last-modified` 与 static-delta superblock 的时间戳直接取 wall clock，C 库没有提供覆盖开关。脚本在生成 summary / delta 时通过一次性编译的 `LD_PRELOAD` shim 把 `g_get_real_time()`、`g_date_time_new_now_utc()`、`g_date_time_new_now_local()` 固定为 0（delta superblock 的 `g_date_time_new_now_utc()` 不会经过可拦截的 `g_get_real_time()` 符号，因此需要分别覆盖）。
- 因此**payload 与 `SHA256SUMS` 在重复运行间逐字节一致**；`META.generated_at_utc` 记录每次运行的实际时间（可用 `FIXTURE_GENERATED_AT=...` 覆盖），且 `META` 不在 `SHA256SUMS` 内。
- 需要比较两次生成结果时，请按常规文件内容 + 符号链接 target 比较，不要直接用 `diff -r`（它会跟随并报出悬空链接的错误）：

```bash
python3 - <<'PY'
import hashlib, os
def snap(root):
    out = {}
    for dp, _, fs in os.walk(root):
        for n in fs:
            p = os.path.join(dp, n)
            rel = os.path.relpath(p, root)
            if rel.endswith("META"):
                continue
            out[rel] = ("l", os.readlink(p)) if os.path.islink(p) else (
                "f", hashlib.sha256(open(p, "rb").read()).hexdigest())
    return out
assert snap("/tmp/fixtures-run1") == snap("go/testdata/fixtures")
print("fixtures are reproducible")
PY
```

## 4. 内容树

两次 commit 使用同一棵树（第二次增加 `secondfile`）：

```
firstfile                  regular 0644，带 user.ostree.fixture / user.ostree.other
somelink -> firstfile      symlink
baz/cow                    regular 0644
baz/cowro                  regular 0600
baz/saucer                 regular 0644
baz/deeper/ohyeah          regular 0644
baz/deeper/ohyeahx         regular 0755
baz/alink -> nonexistent   dangling symlink
baz/another/y              regular 0644
baz/script.sh              executable 0755
baz/sub1/duplicate_a/b     same content
baz/sub2/duplicate_c       same content
empty-file                 zero-length regular file
empty-dir/                 empty directory
secondfile                 仅第二次 commit
```

`bare-user-only` 不使用 xattr（mode 要求 `--no-xattrs`，owner 固定 0:0）。`--no-xattrs` 会让其他 mode 跳过 xattr 设置与导入。

## 5. bare-split-xattrs 的特殊来源

C OSTree 2025.2 **只能实验性写入** `bare-split-xattrs`（需要 `OSTREE_EXP_WRITE_BARE_SPLIT_XATTRS=1`），且写入结果缺少 `file-xattrs` / `file-xattrs-link` 对象，`fsck --all` 会失败——上游 `tests/test-basic-bare-split-xattrs.sh` 正是断言这一点。

生成脚本会先尝试实验性写入；失败时回退到上游已提交的 C 生成仓库 `tests/bare-split-xattrs-basic.tar.xz`：

- 取其唯一 commit `d614c428...`；
- 用 `ostree refs --create` 补充 `test2`、`stable`；
- 用 `summary --update` 生成带 refs 的 summary（同样固定时间戳）；
- 复制 `objects/refs/config/summary` 到 fixture；
- `META.source` 记录回退来源，`xattrs` 记录上游 fixture 的 `user.mykey`。

若未来 C 版本能正确写入该 mode，脚本会自动使用新生成的仓库，`META.source` 变为 `generated by ...`。

## 6. C 版本记录

生成时 `/usr/bin/ostree --version`：

```
libostree:
 Version: '2025.2'
 Features:
  - inode64
  - initial-var
  - libcurl
  - libsoup3
  - gpgme
  - ex-fsverity
  - libarchive
  - selinux
  - avahi
  - libmount
  - systemd
  - release
  - p2p
```

每个 `META` 内也保存了生成当时的完整输出与实际命令，可直接用于复现。

## 7. 相关测试工具

- `go/hack/conformance.sh`：构造伪 builddir（PATH 首位为 Go `ostree`、`tests/ostree` 软链、Go `ostree-trivial-httpd`、`G_TEST_SRCDIR`/`G_TEST_BUILDDIR`），逐脚本运行上游 `tests/test-*.sh` 并采集 TAP。
  - `--list` 显示默认只读子集；`--list --all` 显示全部 111 个脚本及排除原因；
  - `--test NAME`、`--all`、`--timeout`、`--keep`；
  - `--ostree-binary /usr/bin/ostree` 可用于在没有 Go CLI 时验证 harness 本身。
- `go/hack/interop.sh`：双向对拍骨架。C→Go 用本目录 fixture 比较 `refs`/`rev-parse`/`show`/`log`/`ls -R`/`cat`/`summary -v`/`fsck --all`/`static-delta list`；Go→C 用 Go 建仓后要求 C `fsck --all` 通过。未实现的命令报告 SKIP（`--strict` 时视为失败）。
- `go/pkg/testhttpd`：替代 C `ostree-trivial-httpd` 的纯 Go 静态服务器，支持 Range、ETag、Last-Modified、`--force-range-requests`、基础认证、expected cookie/header、随机 500/408、`--daemonize`、`--autoexit`。

## 8. 已知限制

- `bare-split-xattrs` 只有一个 commit，且对象集合与上游 fixture 一致（见 §5）。
- 仅 archive 有 static delta；没有 GPG 签名、collection-id、tombstone、summary 签名。
- fixture 的 summary/delta 时间戳为 0（1970-01-01T00:00:00Z），不是真实生成时间；真实生成时间只记录在 `META.generated_at_utc`。
- 若生成环境没有 `cc`，脚本会告警并退化为真实 wall clock，此时 payload/`SHA256SUMS` 每次运行都会变化。
- `SHA256SUMS` 不覆盖符号链接对象（target 记录在 `META [symlinks]`）。
- `bare` / `bare-user-only` 的对象文件权限参与 checksum 语义（例如 `baz/cowro` 对应 0600 对象），fixture 需要以 root 或对应 uid 读取；不要对 `objects/` 执行 `chmod`，否则 C `fsck` 会报校验失败。
