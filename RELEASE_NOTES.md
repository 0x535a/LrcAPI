# v1.4.2

相较于 v1.4.1，本次更新包含以下改进。

## 歌词补全

- 将多首歌曲缺失的翻译和罗马音合并为批量请求，并按块顺序处理，减少 LLM 请求次数和并发，降低触发上游 HTTP 429 限流的概率。
- 新增受认证保护的 `GET /backfill` 接口，异步补全歌词库；可通过 `?batch=N` 指定每批处理数量。
- 批量补全支持进行中的任务去重；后续步骤失败时保留已经完成的内容。
- 新增 `OPENAI_ENABLE_THINKING` 环境变量。对支持该参数的推理模型，可设为 `false` 关闭思考模式以缩短响应时间；未设置时保持上游默认行为。

## 构建与发布

- 恢复 GitHub Actions 的跨平台二进制构建、Release 和 Docker 镜像发布。
- GitLab CI 保留跨平台二进制构建及 Release，移除 Docker 镜像发布任务。
- 二进制覆盖 Linux、Windows、macOS 的 amd64 和 arm64 架构。
- GitHub 和 GitLab Release 使用同一份版本更新日志。

## 仓库维护

- 统一历史提交的作者和提交者邮箱为 `a@0x535a.cn`；历史提交哈希已变更。
