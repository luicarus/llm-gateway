# llm-gateway

**给 LLM 调用加一层计量表。** 一个轻量的 OpenAI 兼容反向代理：转发请求的同时实时统计 token 用量，并按使用者 / 模型 / 上游拆分展示。

部署一次，团队共用一个入口；**每个工作台只拿网关发的 key，不需要上游 API key**。

```
工作台 A ──┐
工作台 B ──┼──►  网关 (:8080)  ──►  DeepSeek / OpenAI / 任意 OpenAI 兼容 API
工作台 C ──┘       │                 （真密钥只存在网关里）
                   └──► 实时面板 · /metrics · /api/stats
```

## 为什么需要它

直接调上游 API 时，你不知道**谁**用了**多少**：

- 多人 / 多服务共用一个 API key，账单是一笔糊涂账
- 想知道某个模型的消耗占比，只能去供应商后台翻
- 想接 Prometheus / Grafana，上游不提供这个能力

这个网关照常转发请求（对你的代码零侵入），在旁边记一笔账。它**不改变任何 API 语义** —— 把 SDK 的 `base_url` 指过来就行。

## 特性

- **实时面板**：SSE 推送，按使用者 / 模型 / 上游三个维度拆分，无外部依赖、可离线使用
- **密钥隔离**：网关持有上游密钥，工作台用网关下发的 key 调用；网关 key 绝不转发给上游
- **多上游**：一个网关接多个供应商，可把某个 key 钉死到某个上游
- **流式友好**：SSE 逐块转发不做缓冲，延迟不受影响
- **多种 usage 格式**：同时兼容 OpenAI 嵌套写法与 DeepSeek 扁平写法
- **Prometheus 指标**：`/metrics` 直接可被抓取
- **单二进制 / 容器部署**：只依赖 gin 和 yaml，`CGO_ENABLED=0` 静态编译

## 快速开始

### Docker Compose（推荐）

```bash
cp gateway.example.yaml gateway.yaml    # 编辑上游和客户端 key
export DEEPSEEK_API_KEY=sk-...          # 上游密钥走环境变量
docker compose up -d
```

打开 http://localhost:8080/ 看面板。

### 直接跑二进制

```bash
go build -o llmgateway .

# 最简：单上游、不鉴权（仅限本机 / 受信网络）
./llmgateway -upstream https://api.deepseek.com -listen :8080

# 正式：多上游、按 key 鉴权
./llmgateway -config gateway.yaml
```

## 接入工作台

改一行 `base_url`，不需要上游密钥：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://gateway-host:8080/v1",   # ← 指向网关
    api_key="gw-your-key",                     # ← 网关发的 key
)

resp = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.usage.total_tokens)
```

流式照常工作：

```python
stream = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "写首诗"}],
    stream=True,
)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="")
```

或用 curl：

```bash
curl http://gateway-host:8080/v1/chat/completions \
  -H "Authorization: Bearer gw-your-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

`X-Api-Key: gw-your-key` 也可以。

## 配置

完整字段见 [`gateway.example.yaml`](gateway.example.yaml)。

```yaml
listen: ":8080"
default_upstream: deepseek

upstreams:
  - name: deepseek
    base_url: https://api.deepseek.com
    api_key_env: DEEPSEEK_API_KEY        # 推荐：密钥走环境变量，不写进文件

client_keys:                              # 每个工作台一个
  - name: alice-workbench
    key: gw-alice-key                     # 或 key_env: ALICE_KEY
    upstream: deepseek                    # 可选：钉死到某个上游

cors:                                     # 仅浏览器工作台需要
  enabled: true
  allowed_origins: ["http://localhost:3000"]

# dashboard_key_env: LLM_GATEWAY_DASHBOARD_KEY   # 可选：保护面板
```

**环境变量优先级高于配置文件**，方便容器覆盖而不重建镜像：

| 变量 | 作用 |
|---|---|
| `LLM_GATEWAY_CONFIG` | 配置文件路径 |
| `LLM_GATEWAY_LISTEN` | 监听地址 |
| `LLM_GATEWAY_UPSTREAM` | 单上游简写（替换文件里的上游） |
| `LLM_GATEWAY_UPSTREAM_API_KEY` | 配合上面那个的密钥 |
| `LLM_GATEWAY_CORS_ORIGINS` | 逗号分隔的来源列表 |

## 用量数据

| 端点 | 说明 |
|---|---|
| `GET /` · `/dashboard` | 实时面板（SSE） |
| `GET /api/stats` | 当前快照 JSON |
| `GET /api/stream` | SSE 推送，每完成一次请求推一次 |
| `GET /metrics` | Prometheus 文本格式 |

```jsonc
{
  "requests": 128, "errors": 2, "in_flight": 1,
  "prompt_tokens": 40000, "completion_tokens": 12000, "total_tokens": 52000,
  "cached_tokens": 9000, "reasoning_tokens": 500,
  "models":    [{ "model": "deepseek-chat", "requests": 90, "total_tokens": 40000 }],
  "clients":   [{ "client": "alice-workbench", "requests": 60, "total_tokens": 30000 }],
  "upstreams": [{ "upstream": "deepseek", "requests": 128, "total_tokens": 52000 }],
  "recent":    [{ "client": "alice-workbench", "model": "deepseek-chat", "total_tokens": 87,
                  "stream": true, "usage_reported": true }]
}
```

`usage_reported: false` 表示上游没返回 usage —— 这是"未知"，不是"免费"。

## 安全说明

- **`client_keys` 为空 = 网关完全开放。** 只在本机或受信网络这么用；暴露前务必配 key（启动日志会警告）。
- **别把上游密钥写进配置文件**，用 `api_key_env`。仓库的 `.gitignore` 已排除 `gateway.yaml`。
- **面板公开访问时设 `dashboard_key`**，用 `http://host:8080/?key=YOUR_KEY` 打开。
- **浏览器工作台不要把网关 key 写进前端代码**，那等于公开它。前端应调你自己的后端。
- 网关 key 经**常数时间比较**校验，且未授权请求**从不转发**到上游。
- **路径不可用于 SSRF**：请求路径无法改变目标主机（有专门测试覆盖）。
- 目前**没有限流和配额**，防滥用请在前置代理层做。

## 设计要点

实现遵循一个硬约束：**统计绝不影响客户端看到的响应**。

- **流式零缓冲**：SSE 每读到一块立即转发并 flush，解析在旁路进行
- **内存恒定**：流式响应以 tee 方式喂给解析器，不因输出很长而堆积
- **统计失败不影响请求**：任何计数环节出错都被吞掉
- **自动补 `stream_options.include_usage`**：OpenAI 兼容上游默认不在流式响应返回 usage，网关自动补上（客户端已显式设置则不覆盖）
- **上游错误不泄露内部信息**：传输错误详情只写日志，回给调用方的只有上游逻辑名

### 已知限制

- **计数在内存，重启归零。** 需要持久化请订阅 `/api/stream` 落库。
- **面板不是"边生成边涨"**：token 用量只有上游算完才知道，所以每次**请求完成后**跳一次。这是原理性限制，不是实现取舍。
- **速率是进程生命周期均值**，不是滑动窗口。
- **没有限流 / 配额 / 多租户计费 / 金额换算**（只统计 token，各模型单价不同且会变）。
- **仅支持 OpenAI 兼容协议**。Anthropic `/v1/messages` 的 usage 结构不同，尚未支持。

## 开发

```bash
go test ./...      # 单元测试
go vet ./...       # 静态检查
gofmt -l .         # 格式检查

go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...  # 依赖漏洞扫描（当前 0 漏洞）
```

测试重点覆盖易错处：

- **SSE 分片重组**：在**每一个可能的字节位置**切开同一个流，结果必须一致；以及逐字节投递
- **被截断的流**：最后一个 usage 事件不能丢
- **密钥不泄露**：断言网关 key 永远不出现在发给上游的请求里
- **错误不泄露内部信息**：断言上游地址不出现在回给客户端的错误里
- **SSRF 防护**：`//host`、绝对 URL、路径穿越都不能改变目标主机
- **鉴权失败不转发**：坏 key 的请求绝不能到达上游，也不计入上游统计
- **按 key 路由**：不同 key 打到各自被钉死的上游
- **流式是增量的**：第一个 chunk 必须在流结束前到达客户端
- 配置校验：重复上游名、未知默认上游、`key_env` 未设置等都报错而非静默降级

### 本地端到端验证

`tools/` 下有三个开发用工具（不属于网关本体）：

```bash
go build -o mockupstream.exe ./tools/mockupstream   # 假的 OpenAI 兼容上游，不花额度
go build -o verifye2e.exe    ./tools/verifye2e      # 校验鉴权 / 路由 / CORS / 统计
go build -o ssestream.exe    ./tools/ssestream      # 验证 SSE 实时推送

./mockupstream.exe -listen 127.0.0.1:9099 -mode ok   # 也支持 -mode nousage / slow
./llmgateway.exe -config _e2e.yaml -release
./verifye2e.exe
```

`_e2e.yaml` 是两上游 + 两 key 的验证配置，两个上游都指向本地 mock。

## License

MIT
