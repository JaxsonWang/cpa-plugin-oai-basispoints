# 图片 issue #15 / #17 修复与复现说明

## 范围

- 基线：PR #16 合并后的 `a4ea957a4c73e9ceb010dca117e412306b18d0d7`。
- 只修改插件的用户图片附件规范化和安全错误摘要，不改 CPA 主程序、请求来源选择、工具结果、远程 URL 获取、认证或 WS/SSE 通道逻辑。
- 本文记录最初基于 `0.2.3` 的本地修复与负向对照。当时未发布、部署或关闭 issue；后续 `0.2.4` 发布门禁及用户更新后的真实上游验测见 [v0.2.4 验测说明](v0.2.4.md)。

## #17：文件引用携带 detail

修复前，内联图片上传后保留原 `detail`，缺省时补 `auto`；直接传入的 `file_id` 不经过处理。修复后，这两条用户图片路径都只发送：

```json
{"type":"input_image","file_id":"file-example"}
```

本地 HTTP 夹具依照报告的约束拒绝额外字段，旧实现实际返回 `422 Invalid request body.`；修改后，HTTP、SSE 宿主 JSON 回调、已有文件引用和附件缓存回归通过。直接文件引用不额外上传。混合 `file_id` 与 `image_url` 返回带位置的 400，不静默选择其中之一。

这是根据报告编写的协议回归，初次修复时未使用真实 Basis Points 账号重新完成远端 422/200 对照。后续部署的正向验证见 v0.2.4 记录；没有向生产重新注入旧实现。公开 Responses API 的 `detail` 支持不用于证明 Basis Points 私有接口接受该字段。

## #15：格式问题的可控复现

当前 macOS / Go 1.26.2 下，旧实现对 `mime.ExtensionsByType` 取第一个结果：

| 声明 MIME | 旧上传文件名 | 问题 |
| --- | --- | --- |
| `image/jpeg` | `image.jfif` | 不在报告列出的 `.jpeg/.jpg/.png/.gif/.webp` 白名单内 |
| `image/jpg` | `image` | 无扩展名 |
| `image/unknown` | `image` | 无扩展名，即使字节实际为 PNG |

测试 `TestNineImageHistoryUsesSupportedUploadedFilenames` 使用真实本地 HTTP，上传八份相同 PNG 和一份声明为 `image/jpg` 的实际 JPEG。附件服务夹具记录上传文件名，Responses 服务夹具依照报告中的白名单检查引用。用 Go overlay 加载基线的 `attachments.go` 后，测试实际失败为：

```text
Basis Points HTTP 400: Invalid input: Expected image type to be a supported format: .jpeg, .jpg, .png, .gif, .webp but got none
input_images=9
```

修复后，九张图片均保留，两次历史重放成功；相同图片复用附件缓存，总共上传两个不同文件，没有通过丢图、默认格式、吞错或虚假成功绕过校验。

格式现在由 `http.DetectContentType` 检查字节签名确定；支持 PNG、JPEG、GIF、WebP，并使用固定后缀。声明 MIME 的别名或与字节不符时，以可识别的实际字节为准。无法识别或不支持的内容在该图片上传前返回明确的 `invalid_image`。这是签名识别，不是完整图像解码，不能保证所有具有合法文件头但数据损坏的图片都会在本地被发现。

### 尚未确定的现场信息

- 上述 HTTP 服务是合成上游，不能证明真实 Basis Points 的 400 仅由扩展名引起，也不能证明报告中的九张图片恰好使用 `image/jpg`。
- 原始失败请求和 CPA 转换后的 `Payload` 未提供，没有复现原始 Windows/Codex 长对话或执行 Windows 运行时测试。
- 文本路径测试确认，正文中的 Windows `.png` 路径及 Markdown 图片路径不会被插件自动变成图片。另一测试构造原文只有路径、`SourceFormat=codex` 的 `Payload` 含九张图片，确认计数取自选中的 `Payload`；这是来源差异的可行解释，不是现场来源证明。
- 已有文件 ID 的服务端附件内容、远程 URL 图片以及工具结果中的图片不由此次格式修复重新上传或读取。

## 安全诊断

- 本地图片格式错误包含 `input[i].content[j]`。
- 上游图片错误增加 `image_refs`，仅列索引和 `file_id`、`image_url`、`data_url`、`missing` 等类别，最多列 16 项并标明剩余数量；不是判断某一项必然失败。
- 索引指向发送给上游的请求，包括插件注入的消息；不直接等于客户端原始请求中的索引。
- 不输出真实 URL、文件 ID、图片数据或凭据。复现原现场时，需要对照同一请求的原始输入、CPA 实际 `Payload`、这些安全位置及上传文件的 MIME/后缀，勿提交令牌或完整对话。

## 验证命令

在仅包含当前源码和测试的干净目录中执行：

```bash
go test -race ./...
go vet ./...
node --test internal/basispoints/source_auth_page.test.mjs
go mod tidy -diff
```

上述检查通过，页面测试共 16 项。当前工作区的旧 `build/` 目录包含历史 Go 源码备份，直接 `go test ./...` 会误扫描这些不完整备份；验证采用当前 Git 跟踪源码与本次新增文件的临时副本，没有删除或修改旧备份。

另外通过工作流 `actionlint v1.7.12`、Windows AMD64 测试二进制交叉编译及 macOS ARM64 `c-shared` 动态库构建。Windows 结果仅证明编译通过，本机动态库未在 CPA 中加载；不据此宣称 Windows 运行时或宿主 ABI 验收完成。

聚焦复现与回归命令：

```bash
go test ./internal/basispoints -run 'Test(NineImageHistoryUsesSupportedUploadedFilenames|ExecuteImageThroughLocalHTTP|ImageFileReferencesUseOnlyTypeAndFileID|ImageUploadsUseContentTypeAndSupportedFilename|CodexImageCountUsesTranslatedPayload|TextImagePathsDoNotCreateImageInputs)$' -count=1 -v
```

保留当前测试、只替换旧附件实现进行负向对照（需在有 Git 历史的仓库根目录执行，预期两个测试失败）：

```bash
baseline_dir="$(mktemp -d)"
git show a4ea957a4c73e9ceb010dca117e412306b18d0d7:internal/basispoints/attachments.go > "$baseline_dir/attachments.txt"
python3 - "$PWD" "$baseline_dir" <<'PY'
import json
import pathlib
import sys
root, baseline = map(pathlib.Path, sys.argv[1:])
(baseline / "overlay.json").write_text(json.dumps({
    "Replace": {
        str(root / "internal/basispoints/attachments.go"): str(baseline / "attachments.txt")
    }
}))
PY
go test -overlay="$baseline_dir/overlay.json" ./internal/basispoints -run 'Test(ExecuteImageThroughLocalHTTP|NineImageHistoryUsesSupportedUploadedFilenames)$' -count=1
```

该命令不切换分支、不覆盖工作区源码；旧实现的两个预期错误分别为 422 字段形状拒绝，以及 400 缺失扩展名且 `input_images=9`。取消 `-overlay` 后对应测试应通过。
