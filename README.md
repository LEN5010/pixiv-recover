# pixiv-recover

Pixiv CDN 探测工具。
从相邻作品推断投稿时间，枚举可能的原图 URL，并自动下载仍由 CDN 公开提供的多页图片。

思路来自[这篇文章](https://www.bilibili.com/opus/241162727204255123)，脚本在此基础上增加了：

- 自动查询相邻作品并将时间统一为日本时区
- Pixiv CDN 所需的 `Referer` 请求头
- 并发探测、文件格式校验和下载重试
- 自动发现多页作品，支持已有文件续跑

## 使用

需要 Python 3.10 或更高版本，无第三方依赖：

```powershell
python .\pixiv_recover.py 作品ID
```

如果无法从相邻 ID 推断时间，可手动指定投稿分钟（日本时间）：

```powershell
python .\pixiv_recover.py 123456789 --minute 2026-01-02T20:08
```

默认保存至 `recovered/`。运行 `python .\pixiv_recover.py --help` 查看并发数、输出目录、页数和超时等选项。

## Go 版本（无需 Python）

现在也提供 Go 版本，不需要安装 Python。到 [Releases](https://github.com/LEN5010/pixiv-recover/releases) 下载对应系统的程序（Windows / macOS / Linux，amd64 与 arm64），解压后运行：

```powershell
pixiv-recover 作品ID
```

```powershell
pixiv-recover 123456789 --minute 2026-01-02T20:08
```

不带参数运行（例如在 Windows 上直接双击 `pixiv-recover.exe`）会进入交互模式，按提示逐个输入作品 ID 即可。
如果作品本身仍可访问，Go 版会直接下载元数据给出的原图，不再枚举时间戳。运行 `pixiv-recover --help` 查看全部选项。

Go 版由 @ts8zs 移植（[#1](https://github.com/LEN5010/pixiv-recover/issues/1)）。

从源码构建（需要 Go 1.22 或更高版本）：

```powershell
go build ./cmd/pixiv-recover
```

Releases 中同时附带 Python 版压缩包 `pixiv-recover-python_<版本>.zip`（含 `pixiv_recover.py`、README 和 LICENSE）。

维护者发布新版本：推送形如 `v1.0.0` 的标签，GitHub Actions 会自动构建各平台程序和 Python 版压缩包，并发布到 Releases。

## 限制与使用边界

该方法只对 CDN 尚未清理、且文件名不含不可枚举哈希的近期作品有效。
脚本不会绕过登录、付费墙或其他访问控制。
只请求 Pixiv CDN 仍公开返回的 URL。

请仅恢复你有权保存的内容，尊重作者删除作品的决定，并遵守所在地法律及 Pixiv 服务条款。
不要将恢复内容提交到本仓库。

## 许可证

[GNU General Public License v3.0 only](LICENSE)。


## 友情链接
Community: [LINUX DO](https://linux.do/)
