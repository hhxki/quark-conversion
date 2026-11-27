# Quark Conversion · 夸克分享转换

批量从分享文案或 TXT 提取夸克链接。优先复用自己的现有分享，符合转存条件时生成新的分享链接。Windows 和 Linux 均可运行，内置本地网页。

## 下载与启动

Linux x86-64 可从 [最新 Release](https://github.com/hhxki/quark-conversion/releases/latest) 下载 `quark-convert-linux-amd64`。在下载目录运行以下命令，然后打开 [本地网页](http://127.0.0.1:8765)。

```sh
chmod +x quark-convert-linux-amd64
./quark-convert-linux-amd64 serve --host 127.0.0.1 --port 8765
```

## 从源码启动

需要 Go 1.25 或更高版本。在项目根目录运行下面的命令，然后打开 [本地网页](http://127.0.0.1:8765)。网页已嵌入 Go 程序，无需单独启动前端。

```sh
go run . serve
```

也可以先编译，再在 Windows 运行 `./quark-convert.exe serve`，或在 Linux 运行 `./quark-convert serve`。

```sh
# Windows
go build -o quark-convert.exe .
# Linux
go build -o quark-convert .
```

## 网页

通过上面的方式启动程序后，浏览器打开 [127.0.0.1:8765](http://127.0.0.1:8765)。

点击「连接夸克网盘」，可以扫码，也可以导入已登录网页版的 Cookie。登录后粘贴文案或拖入 TXT，预览源链接，再点击「开始转换」。首次体验可以用自己可控制的一份小文件测试。

网页可调整大小上限、筛选待手动处理的链接、查看本地历史、核对「我的分享」和导出成功链接。复制和 TXT 导出每行只有一个链接。新生成的分享采用公开链接。复用历史私密分享时，工具尝试使用带提取码的完整地址。

启动网页服务或登录时，程序会分页同步「我的分享」到本机 SQLite。转换和「我的分享」页面优先读取当前账号的索引。程序创建的分享会立即写入索引。全量同步失败时保留上次完整索引。

查重先用本地转换记录和保存的原链接映射，再用「我的分享」缓存查找相同文件或同名候选。移除本地历史仍保留原链接映射；需要刻意生成新分享时，可以使用网页的「测试新转存」。只有名字相同而无法核对内容时，会显示候选供确认。原链接包含提取码时也会保存在本机数据目录。

### 手动转存更新

安装 Node.js，准备带有 `get-share-update-list`、`get-share-update-files`、`get-share-saved-dir` 和 `saveas-update` 的[夸克官方 CLI](https://github.com/quark-clouddrive/quarkclouddrive_offical)，用运行网页服务的同一系统用户执行一次 `node /path/to/quark-drive.cjs login`，授权相同的夸克账号。网页原有的扫码登录用于转换，更新页使用官方 CLI 的授权。

```sh
quark-convert serve --port 8766 --quark-cli "/path/to/quark-drive.cjs"
```

打开「转存更新」→「检查更新」→「查看新增」。核对夸克记录的上次转存目录后，点击「转存全部新增」。夸克用源分享的记录计算增量，本程序只提交增量转存，自己加到目标目录的文件不参与计算，也没有同步删除操作。历史链接能否更新取决于夸克是否识别到该账号的原转存记录。

更新页没有后台扫描。检查更新只请求官方列表一页，翻页和查看具体文件由用户触发。遇到限流或转存结果不明时，页面显示错误，请先到网盘核对再重新检查。

## 命令行

以下示例使用 `quark-convert` 作为命令名。从源码编译后，可以在项目根目录用 Windows 的 `.\quark-convert.exe` 或 Linux 的 `./quark-convert` 运行。如果网页服务通过 `--data-dir` 指定了数据目录，CLI 也要传入同一个目录，才能共用登录状态和历史。

~~~sh
quark-convert login --cookie-file /path/to/quark-cookie.txt
quark-convert convert "https://pan.quark.cn/s/your-share-id"
quark-convert convert --file input.txt --limit 0.5T --json
quark-convert serve --port 8765
quark-convert serve --host 127.0.0.1 --port 8765
~~~

`--host` 可指定监听的 IPv4 地址或主机名。省略时监听 `0.0.0.0`，允许局域网设备访问。指定 `--host 127.0.0.1` 时，仅本机可以访问。端口被占用时，可改为 `--port 8766` 或用 `--port 0` 让系统分配空闲端口。访问网址以终端输出为准。

网页扫码和命令行会话导入共享本机数据。普通模式把成功链接写入标准输出，将失败和跳过项写入标准错误。`--json` 输出逐条结果。退出码 `0` 代表所有条目完成，`1` 代表请求未开始或程序错误，`2` 代表部分条目需要处理。

默认数据目录位于系统用户配置目录下的 `quark-convert`。也可以为子命令指定 `--data-dir`。SQLite 数据库包含账号会话、历史和分享索引，请保留在本机，提交问题时也请用占位符代替 Cookie 和私密分享链接。

### 转换时附加固定文件

在登录账号的网盘根目录创建「常用附件」，把要重复使用的多个普通文件直接放进去。加上 `--attach` 时，本次转换的每条链接都附加这组文件。

```sh
quark-convert convert --json --attach --file input.txt
```

附件直接复制到这条链接转存得到的单个顶层文件夹。纯散文件和多顶层项目需要先整理为单个共享文件夹。已有链接可继续使用，但必须核对为当前账号的单文件夹分享。同名文件无法确认一致时，结果会提示冲突，且不会覆盖。发起新分享时，先确认附件复制完成再生成链接。

其他项目读取原来的 JSON 数组即可。显式请求且附件就绪时，该条结果额外返回 `"attachments_ready": true`。省略附件参数则按普通流程转换。HTTP 调用可传 `"attach": true`，网页也有对应的批次开关。

### 自定义分享文案

把下面两行保存到 UTF-8 的 `share.txt`。`input.txt` 放待转换的原链接或分享文案。每条成功结果会套用一次模板，保留模板里的换行。

```text
夸克网盘给你分享了「{{.Title}}」
链接：{{.URL}}
```

```powershell
.\quark-convert.exe convert --template-file share.txt --file input.txt
```

也可以直接传入单行模板。

```sh
quark-convert convert --template '{{.Title}}｜{{.URL}}' --file input.txt
```

### 让其他软件读取转换结果

调用 `quark-convert convert --json --file input.txt`，读取标准输出里的 **一个 JSON 数组**。模板变量和 JSON 字段的对应关系如下。

| 模板变量 | JSON 字段 | 含义 |
| --- | --- | --- |
| `{{.Title}}` | `title` | 分享标题。标题无法获取时，JSON 可能省略此字段 |
| `{{.URL}}` | `link` | 转换后生成或复用的分享链接。未成功的记录没有此字段 |
| `{{.Source}}` | `source` | 原始分享链接 |
| `{{.Status}}` | `status` | `created` 表示新建，`reused` 表示复用。其他状态说明该条未完成 |

例如，PowerShell 可以读取 JSON，再把成功的记录拼成分享文案。

```powershell
$output = .\quark-convert.exe convert --json --file input.txt
$code = $LASTEXITCODE
if ($code -notin 0, 2) { throw "转换命令失败，退出码 $code" }
$results = $output | ConvertFrom-Json
foreach ($item in $results) {
    if ($item.link) {
        $title = if ($item.title) { $item.title } else { '未命名分享' }
        "夸克网盘给你分享了「$title」"
        "链接：$($item.link)"
    }
}
```

退出码 `2` 时，数组里仍可能有成功记录，可以先处理带 `link` 的记录，再查看其他记录的 `status` 和 `message`。`--json`、`--template` 和 `--template-file` 三选一。省略这三项时，每行输出一个成功链接。

## HTTP API

管理页与 API 使用同一个监听地址与端口，并共用当前登录的夸克会话。默认监听 `0.0.0.0:8765`，局域网设备可通过运行程序的电脑 IPv4 地址访问，请在可信网络中使用。仅供本机调用时，使用 `serve --host 127.0.0.1 --port 8765`，并向 `http://127.0.0.1:8765` 发送请求。

`GET /api/v1/settings` 读取当前登录状态和转存大小上限。Python、Node.js 或 Next.js 服务端可向 `POST /api/v1/convert` 发送 JSON。

~~~json
{"text":"分享正文或一批链接","limit":"0.5T"}
~~~

`limit` 可省略。响应是逐条结果数组，字段与 CLI 的 JSON 输出相同。加上 `?stream=1` 可逐行接收 NDJSON。`POST /api/v1/preview` 只解析输入。`POST /api/v1/confirm` 接收 `source` 和 `link`，用于确认要复用的旧分享。

`GET /api/v1/shares` 读取本机分享索引。加上 `?refresh=1` 可以主动同步网盘列表。

## 转存规则

- 顶层只有一个文件夹时，直接保存到「来自：分享」，再分享新保存的文件夹。目录内的子目录随之保存。
- 顶层有多个文件夹，或文件夹与散文件混合时，保留原链接并标为待手动筛选。纯文件分享整条处理。
- 默认每条上限为 100G。转存前统计整个分享的文件体积。超限或大小无法核算时，保留原链接供手动处理。
- 触发频控时遵循服务端等待时间并保留进度。遇到验证页面时，到夸克网页版处理后，可重新提交输入。
- 状态不明的写入会先核对记录和网盘结果。可确认的转存结果直接用于继续分享。

## 开发

用 Go 1.25 或更新版本运行 `go test ./...`、`go vet ./...` 和 `go build -o quark-convert .`。网页静态资源嵌入可执行文件，构建和运行均无需 Node.js。真实接口按网页端行为实现，相关字段需要持续用测试资料验证。

- [夸克网盘官方 Skill](https://github.com/quark-clouddrive/quarkclouddrive_offical)
- [社区网页版接口记录](https://github.com/zhaocongqi/clouddrive-auto-save/blob/main/docs/quark_apis.md)
- [社区扫码流程](https://github.com/imeiming/quark-drive/blob/main/references/auth.md)

## 许可证

项目源码与文档采用 [MIT 许可证](LICENSE)。这是独立项目，转存和分享由用户登录的夸克账号执行。
