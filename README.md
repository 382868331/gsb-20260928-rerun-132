# sourcenav — 源码导航服务（第一版）

用 Go 实现的教学语言 **Edulang** 跨文件源码导航服务。第一版聚焦两个能力：

- 跨文件 **定义跳转（definition）**
- 跨文件 **引用定位（references）**
 - 跨文件 **带预览的重命名（rename）**：先预览、确认后原子落笔
- 跨文件 **声明迁移（move）**：把顶层声明搬到另一个文件并可同时改名，
  选定保留的引用通过源文件里的兼容入口（import + export 别名）继续解析；
  迁移可撤销，逐字节恢复原声明、引用与文件状态

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
 - `POST /diagnostics` `{uri}` 返回该文档的语义诊断
- `POST /rename/prepare` `{uri,offset,newName}` 生成重命名预览方案（不改任何文本）
- `POST /rename/apply` 传入预览方案整体，校验通过后原子应用；方案过期返回 409
- `POST /move/prepare` `{uri,offset,targetUri,newName,keepUris}` 生成声明迁移预览：
  顶层声明移入 `targetUri` 并改名为 `newName`（空则保持原名）；`keepUris`
  中的文件引用保持原文不变
- `POST /move/apply` 传入迁移方案整体，逐文档校验版本与原文后原子应用；过期返回 409
- `POST /move/undo` 撤销最近一次迁移，恢复全部触及文档的文本、版本与保存状态
- `GET /status` 返回快照 ID、各文档版本/保存状态
- `GET /health`

## 模块与声明迁移

- **模块指令**：文件首条语句可为 `module "a.b"`；无指令的文件属于匿名世界，
  顶层名仍是工程级裸名（与第一版一致）。
- **导入与限定名**：`import "a.b" [as x]` 绑定模块限定符，`x.name` 解析到
  该模块导出的符号；限定符绝不能指向局部变量。
- **再导出**：`export local [as external]` 把本文件可见的名字（自身顶层声明
  或已导入模块的导出）以同一符号身份别名出去——消费方绑定的是原声明，不是副本。
- **迁移兼容入口**：声明跨文件移动（尤其改名）时，`keepUris` 里选定保留的
  引用不改写；源文件自动补上 `import` + `export new as old` 兼容入口，
  旧的限定名/裸名继续解析到已迁移的符号。
- **撤销**：每次迁移落笔前记录触及文档的完整状态；`/move/undo` 校验迁移后
  无人再改动这些文档，然后逐字节恢复原文、版本号与保存标记。

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

## 重命名语义

- **只改真正绑定的出现点**：预览收集的是与目标符号同一身份（Symbol ID）的
  声明与引用；遮蔽内层局部、同名无关声明天然不在方案里，绝不会被批量替换。
- **冲突拒绝**：`newName` 必须是合法标识符且非关键字；若目标文件集合中已存在
  绑定到其他符号的同名出现点（会捕获或被遮蔽），或全局新名已被占用，预览直接报错。
- **过期方案拒绝**：方案绑定每个待改文档的 `version` 与每处编辑覆盖的原文。
  预览后任一**相关**文档变化（如声明前插入文本、并发编辑），应用时逐字核对失败，
  整体拒绝（HTTP 409）且**不产生任何部分修改**；无关文档的编辑不影响方案有效性。
- **原子落笔**：所有新文本先在内存算好并校验，再一次快照提交全部文档，
  不存在部分文件已改、其他文件未改的中间态。
- **会话文本 vs 落盘**：重命名只改会话文本，触及的文档变为 `saved:false` 未保存
  overlay；落盘需显式 `/save`，两者始终可区分。未保存内容同样参与分析。

```
go test ./...
go run ./cmd/sourcenav -addr :8080
```

`internal/workspace/workspace_test.go` 覆盖：相互引用文件、嵌套遮蔽、
未保存改动、无关编辑下身份稳定、真实源码区间切片、未定义名诊断；
`internal/server/server_test.go` 覆盖 HTTP 端到端链路。
