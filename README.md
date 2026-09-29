# sourcenav — 源码导航服务（第一版）

用 Go 实现的教学语言 **Edulang** 跨文件源码导航服务。第一版聚焦两个能力：

- 跨文件 **定义跳转（definition）**
- 跨文件 **引用定位（references）**

它不是代码片段管理器：存储的是版本化的工程快照、语法对象和符号身份，
后续补全/重命名等能力直接建立在此基础上。

## 架构：三层严格分离

| 层 | 包 | 职责 | 是否可变 |
| --- | --- | --- | --- |
| 工程快照 | `internal/snapshot` | 不可变文档（URI、版本号、是否已保存、文本）与不可变快照 | 每次编辑产生新快照 |
| 语法对象 | `internal/syntax` | 词法/语法分析得到的 AST；每个节点携带真实源码 `Range`（字节偏移 + 行列） | 与某个文档版本绑定 |
| 符号身份 | `internal/semantic` | 稳定的 `Symbol` ID、嵌套词法作用域、声明/引用出现点、诊断 | 身份跨编辑存活 |

会话层 `internal/workspace` 把三层缝合：接收未保存改动、做增量重分析、
回答查询；HTTP 层在 `internal/server`，入口在 `cmd/sourcenav`。

## 关键语义

- **跨文件**：每个文件的顶层 `let`/`fn` 提升为工程级全局名，先做一次纯语法
  收集，因此文件之间可以相互前向引用（`a.edu` 调 `b`，`b.edu` 也能调 `a`）。
- **嵌套作用域**：`fn` 参数、块 `{ }`、`if/else` 各自建立词法作用域；内层同名
  变量遮蔽外层，遮蔽变量获得**不同**符号身份，绝不会串到外部定义。
- **局部身份**：局部符号 key = `文件URI | 词法路径 | 名字:声明字节偏移`，
  不同文件/不同函数里的同名局部变量天然隔离。
- **未保存改动**：`/edit` 以 overlay 进入当前会话快照（`saved:false`），查询立即
  反映新文本；`/save` 只翻转已保存标记。
- **结果绑定版本**：所有结果带 `snapshotId` 和每个文档的 `version`；每个位置带
  所属文档版本。
- **增量更新**：编辑只重新词法/语法分析该文件；其它文件复用缓存的语法树，仅当
  出现“受影响全局名”（旧/新顶层名字集合）时才重新做语义绑定，未受影响的引用
  保持同一符号 ID，不做全量重建。
- **真实源码定位**：`Range` 是半开字节区间 `[start,end)` 加 1 基行列；响应里的
  `range.text` 直接从对应版本快照切片得到，可核对定位确实回到源文字段。

## Edulang 语法（示例）

```
let top = 10

fn add(a, b) {
  if a > 0 {
    let s = a + b
    return s
  } else {
    return b
  }
}

fn main() {
  return add(top, 2) // 跨文件时 add/top 可以定义在别的 .edu 文件
}
```

支持：`let`、`fn`、`return`、`if/else`、赋值、调用、整数、
`+ - * == != > < >= <=`、`//` 行注释；分号可选。

## HTTP 接口

所有请求/响应均为 JSON。

- `POST /open` `{uri,text,version,saved}` 打开/覆盖文档（未保存缓冲区传 `saved:false`）
- `POST /edit` `{uri,text,version}` 推送一次未保存改动
- `POST /save` `{uri}` 标记当前版本已保存
- `POST /definition` `{uri,offset}` 返回符号身份与声明位置
- `POST /references` `{uri,offset}` 返回声明 + 全部引用位置
- `GET /status` 返回快照 ID、各文档版本/保存状态
- `GET /health`

位置对象示例：

```json
{
  "uri": "file:///b.edu",
  "version": 2,
  "range": {
    "start": 0, "end": 4,
    "startLine": 1, "startColumn": 4,
    "endLine": 1, "endColumn": 8,
    "text": "beta"
  },
  "role": "declaration",
  "kind": "func"
}
```

## 运行与测试

```
go test ./...
go run ./cmd/sourcenav -addr :8080
```

`internal/workspace/workspace_test.go` 覆盖：相互引用文件、嵌套遮蔽、
未保存改动、无关编辑下身份稳定、真实源码区间切片、未定义名诊断；
`internal/server/server_test.go` 覆盖 HTTP 端到端链路。
