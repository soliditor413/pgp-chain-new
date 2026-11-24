# third_party —— 闭源上游依赖的固化副本（provenance 记录）

本目录存放**上游已闭源、无法再从公开 module proxy 获取**的 Go 依赖的原始快照，
通过根 `go.mod` 的 `replace` 指令接入构建。

## 为什么需要这个目录

根 `go.mod` 依赖两个 Elastos 模块。上游仓库转为私有后，`github.com/elastos/*`
的仓库地址对匿名访问返回 404，且公共 module proxy（`proxy.golang.org`）从未缓存过
这两个 pseudo-version，因此 `go mod download` 在任何干净的 CI 环境里都会永久 404，
`go build` / `go test` 全部无法开始。

把原始源码快照固化进仓库，是为了让构建**不依赖任何外部网络**、且**可审计**。

## 已固化的模块

| 字段 | Elastos.ELA | Elastos.ELA.SPV |
|---|---|---|
| module path | `github.com/elastos/Elastos.ELA` | `github.com/elastos/Elastos.ELA.SPV` |
| 本地路径 | `./third_party/elastos-ela` | `./third_party/elastos-ela-spv` |
| 原仓库 | `https://github.com/elastos/Elastos.ELA`（2026-11 起匿名 404，已闭源） | `https://github.com/elastos/Elastos.ELA.SPV`（同上，已闭源） |
| 版本 | `v0.9.8-0.20230822071649-363670e32ee4` | `v0.1.1-0.20240904064254-31ee1cd78518` |
| commit | `363670e32ee4` | `31ee1cd78518` |
| module h1（原 `go.sum` 记录） | `h1:Z61FysqPygkvNjYPdjz5qOrCRYqXuWGqNWzS9GaLscw=` | `h1:oCjoblPEI8feuXIhFWNSCaT6XtFADPsFcgBQBqdyGGo=` |
| 原 `go.mod` sha256 | `398e44a97ad36e1c083028b26705272d9519d99809153effb55ca1f7dcc9a97d` | `28c6f1e752530431f2bccf8ecc5a4bdc79960969c4825c17a0364bfd2befd49d` |
| 许可证 | MIT, Copyright (c) 2017-2020 The Elastos Foundation | MIT, Copyright (c) 2018 Elastos Foundation |
| 文件数 | 729 | 107 |
| 校验清单 | `./elastos-ela.SHA256SUMS` | `./elastos-ela-spv.SHA256SUMS` |

**落仓日期**：2026-11-24
**落仓规模**：合计 836 个文件，约 7.1 MB

## 内容来源（可复现）

本目录内容**不是**从某个开发者机器上的已展开模块缓存目录拷来的，而是
**从 Go module proxy 的原始 zip 包解包**得到的。原始 zip 位于：

```
$(go env GOMODCACHE)/cache/download/github.com/elastos/!elastos.!e!l!a/@v/v0.9.8-0.20230822071649-363670e32ee4.zip
$(go env GOMODCACHE)/cache/download/github.com/elastos/!elastos.!e!l!a.!s!p!v/@v/v0.1.1-0.20240904064254-31ee1cd78518.zip
```

zip 的 `*.ziphash` 文件记录的哈希与上表中「module h1」两栏完全一致，这构成了
「本地副本 == 上游原始字节」的证明链：

```
原始 zip  --(ziphash)-->  h1:Z61Fys... == 原 go.sum 中的 h1   ✅
```

解包后已与 `$GOMODCACHE` 中 Go 自己展开的模块目录做过 `diff -r` 全量比对，
结果逐字节一致（唯一差异是两个目录里各自多出的 `go.sum`，那是本地 `go mod tidy`
的产物、不属于模块 zip 内容，已删除以保证与 zip 严格一致）。

## 校验方式（重要）

加入 `replace` 之后，这两个模块**不再是**从 proxy 下载的模块，
因此 `go mod verify` 与 `go.sum` **都不再覆盖它们**——Go 把本地路径当成本地代码，
不参与模块哈希校验。

**这一点是实测验证过的（2026-11-24）**：向 `third_party/elastos-ela/LICENSE`
追加一个字节后再执行 `go mod verify`，输出仍是 `all modules verified`（即完全没检出）；
而 `shasum -a 256 -c ../elastos-ela.SHA256SUMS` 正确报出 `./LICENSE: FAILED`。
请勿把 `go mod verify` 通过当作第三方代码未被篡改的证据。

**校验以本目录下的 `SHA256SUMS` 清单为准**：

```bash
cd third_party/elastos-ela     && shasum -a 256 -c ../elastos-ela.SHA256SUMS
cd third_party/elastos-ela-spv && shasum -a 256 -c ../elastos-ela-spv.SHA256SUMS
```

清单覆盖目录内**全部** `729 + 107 = 836` 个文件（含 `.idea/` 下文件名带空格的那一个，
清单生成使用 `find -print0 | sort -z | xargs -0`，因此带空格的路径不会漏算）。

**信任链的边界（重要）**：本节的校验只能证明「目录内容与落仓那一刻一致」，
不能证明「落仓那一刻的内容就是对的」。那一刻的唯一锚点是上表的两个 `h1:`；
而 `h1` 是模块 zip 的 dirhash，**无法从本目录内容重算**。因此仓库外的原始
zip 归档（`elastos-ela.zip` / `elastos-ela-spv.zip` 及其 `.ziphash`）不是备份，
而是整条信任链的**根**，不可丢失。

## 漏洞可见性边界（重要）

**1. `go mod verify` 通过 ≠ 本目录未被篡改。**
本目录是被 `replace` 指向的本地代码，Go 不把它当作模块，因此不对它做任何哈希校验。
实测（2026-11-24）：向 `third_party/elastos-ela/LICENSE` 追加一个字节后，`go mod verify`
仍输出 `all modules verified`；同一时刻 `shasum -a 256 -c ../elastos-ela.SHA256SUMS`
正确报出 `./LICENSE: FAILED`。这与上一节互为印证：**只有 `SHA256SUMS` 具备检出能力**。

**2. SCA / 漏洞扫描工具完全不覆盖本目录。**
`govulncheck`、`golangci-lint`、nancy 一类工具都扫不到这里。需要注意准确的原因：
**不是"扫了但没发现问题"，而是它从未被识别为一个可查询的模块。**
佐证：`govulncheck -format json` 生成的 SBOM 中，被目录 replace 的模块**没有 module path、
也没有 version**，只剩一个本地路径，漏洞库因此永远无法匹配到它。
同理，OSV / Go 漏洞库中也不存在 `github.com/elastos/Elastos.ELA` 这个 module 的记录
（上游自 2026 年起闭源、GitHub 仓库匿名访问 404、官方 proxy 从未缓存其 pseudo-version）。

**3. 这是所有固化方案的共性，不是本方案的缺陷。**
全量 `vendor/` 方案同样不会被上述工具扫描；`fork + replace` 如果走模块版本形式
（即 replace 到一个有版本号的 fork）**会**被扫描，但本仓库没有选择那条路径。
所以不要把"换一种固化方式就能被扫到"当成补救措施。

**4. 结论。**
`Elastos.ELA` / `Elastos.ELA.SPV` 承载跨链 SPV 与 DPOS/PBFT 的核心逻辑，
其安全性**只能依靠人工代码评审 + 逐文件 sha256 校验**。
任何漏洞扫描报告给出的"无问题"结论，**均不覆盖**这两个模块。

## 维护规则

1. **本目录禁止编辑。** 任何直接修改（改代码、删测试、跑 `gofmt`、裁剪"用不上"的
   代码或依赖声明）都会破坏与原始 h1 的一致性，也让审计失效。省下几 MB 不值得。
2. **禁止修改 `third_party/*/go.mod`。** 它参与 MVS 版本选择，改动会连带改变整个
   项目的依赖图；其中列出的 require 即使显式用不到也必须原样保留。
3. **如需 patch 上游代码**：必须在本 README 登记 diff（改了什么、为什么、patch 来源），
   然后**重新生成并提交**对应的 `SHA256SUMS`。未登记的改动视为污染。
4. **升级依赖**时不能简单删掉 `replace`——源已死，删了就再也拉不回来。正确做法是
   用有权限的凭证从上游私有仓库取得新版本 zip，重新走一遍本文件的落仓与登记流程。
5. 该目录已被 `Makefile` 的格式化目标排除（`find . -name "*.go" -not -path "./third_party/*"`），
   避免 `gofmt` 原地改写第三方源码。
6. **本目录下不应出现内层 `go.sum`。** 它由 IDE/gopls 或 `go mod tidy` 的副作用产生，
   不属于模块 zip 内容，会污染 provenance，并让文件数从 `839` 漂移。若发现请删除，不要提交。
   （根 `.gitignore` 已加 `third_party/*/go.sum` 作兜底。）
7. **提交前必须核对文件数——上游模块自带 `.gitignore`，会静默吞掉上游文件。**
   两个模块各自带了一份 `.gitignore`（属于模块内容，**不可修改**），其中的模式在
   `git add` 时同样生效。已实测的模式包括 `.idea/**/*.xml`、`.idea/**/*.iml`、
   `.idea/codeStyles`、`*.json`、`*.db`、`*.log`、`test/`、`build/`、`vendor/` 等。

   当前 839 个文件**恰好一个都没命中——这是巧合，不是设计**。若上游新版本引入命中
   这些模式的文件（例如 `testdata/*.json`、`build/` 下的产物），普通 `git add` 会
   静默漏掉，导致提交后文件数与 `SHA256SUMS` 不符、CI 集合比对失败。

   根 `.gitignore` 已把裸 `.idea` 锚定为 `/.idea`（仅匹配仓库根），修掉了其中一个
   已知实例（`elastos-ela/.idea/fileTemplates/internal/Go File.go`——它是 `.go`
   后缀，不在上游 `.idea/**/*.xml` 那几条规则的范围内，当初只能靠根规则命中）。
   但**上游模块自己的 `.gitignore` 不在可修范围内**——改它会破坏与原始 h1 的一致性。

   发现文件数不符时，正确做法是 `git add -f third_party/<模块>` 强制纳入，
   **不要删文件，也不要重算 `SHA256SUMS`**（见下一节）。

   ```bash
   # 1) 核对文件数
   git ls-files third_party/elastos-ela     | wc -l   # 必须 729
   git ls-files third_party/elastos-ela-spv | wc -l   # 必须 107
   git ls-files third_party                 | wc -l   # 必须 839

   # 2) 查有没有文件被忽略（--no-index 才能看到已跟踪文件；期望无输出、退出码 1）
   find third_party/elastos-ela third_party/elastos-ela-spv -type f \
     | git check-ignore --no-index --stdin -v
   ```

## 审计复现（重新生成清单前必读）

`SHA256SUMS` 可以用下面的命令从目录内容重建，重建结果应与仓库中的文件**逐字节一致**：

```bash
cd third_party/elastos-ela
LC_ALL=C find . -type f -print0 | LC_ALL=C sort -z | xargs -0 shasum -a 256 \
  | diff - ../elastos-ela.SHA256SUMS && echo IDENTICAL

cd ../elastos-ela-spv
LC_ALL=C find . -type f -print0 | LC_ALL=C sort -z | xargs -0 shasum -a 256 \
  | diff - ../elastos-ela-spv.SHA256SUMS && echo IDENTICAL
```

**三个必须保留的细节，缺一个就会静默丢文件**：

> 本节只覆盖「清单 ← 目录」这一跳。更上一跳「h1 ← 归档 zip」的复算方法见
> `audit/pr-1-ci-dependency-fix.md` §4.2（用 `golang.org/x/mod/sumdb/dirhash.HashZip`
> 的 `Hash1` 从 zip 直接重算，与 `go.sum` 比对）。那一跳通过后，本目录才可宣称
> provenance 闭合——`h1` 无法从目录内容重算，归档 zip 是链的根。

| 细节 | 少了会怎样 |
|---|---|
| `-print0` + `sort -z` + `xargs -0` | 上游含一个带空格的文件 `./.idea/fileTemplates/internal/Go File.go`，朴素写法 `find \| sort \| xargs shasum` 会把它切成两段：**输出只有 728 行**（实测），stderr 报 `No such file or directory`，**退出码非 0**（macOS 为 `1`，GNU `xargs` 为 `123`）。它**会**报警——危险的是脚本吞掉 stderr 或忽略退出码（`\|\| true`、未开 `set -e`、未开 `pipefail`） |
| `LC_ALL=C` | 排序受 locale 影响，`diff` 会假报差异 |
| `-a 256` 与 `shasum` 的路径前缀 | 本清单是 `shasum` 生成的 `hash␣␣./path` 格式（macOS 与多数 Linux 自带；CI 的 `sha256sum` 输出同格式，可互换）。**不要**加 `--binary`：它写出的是 `hash␣*./path`，`shasum -c` 其实能解析（实测 6.02 通过，含带空格文件名），但与清单格式不一致，`diff` 必然报差异，"逐字节一致"这个判据随之失效 |

### 绝对禁止：为了消红而重新生成清单

如果 CI 报「清单里有、git 里没有」，正确处置是 `git add -f third_party/<模块>` 补齐，
**不是重新生成清单**。原因是会连锁退化：

```
某文件被静默漏掉 → CI 红
  → 重新生成清单（朴素写法）：只得到 728 行，丢掉带空格那个
  → CI 仍然红（清单 728 vs 文件 729）
  → 下一步"修复"：把那个文件删掉 → 绿了，但上游内容永久缺失
```

即使命令用对了、重建出 729 行，重新生成仍然会让清单从「描述上游源码」退化为
「描述 git 恰好收进来的部分」——此后永远为绿，且不再携带任何信息。**为了让检查变绿
而修改检查，比检查失败严重得多。**

