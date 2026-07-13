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

## 限制与使用边界

该方法只对 CDN 尚未清理、且文件名不含不可枚举哈希的近期作品有效。
脚本不会绕过登录、付费墙或其他访问控制。
只请求 Pixiv CDN 仍公开返回的 URL。

请仅恢复你有权保存的内容，尊重作者删除作品的决定，并遵守所在地法律及 Pixiv 服务条款。
不要将恢复内容提交到本仓库。

## 许可证

[GNU General Public License v3.0 only](LICENSE)。
