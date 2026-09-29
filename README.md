# 源码导航服务 navd

面向自定义教学语言 EduLang 的 Go 导航服务（第一版）：跨文件符号查找
（go-to-definition）与引用定位（find-references）。它不是代码片段管理器，
而是为后续补全（completion）准备的分层基础。

## 架构分层

- `snapshot`：工程快照。快照不可变、内容哈希标识；`open/change/save/close`
  管理未保存缓冲区，未保存改动立即反映到当前会话，已保存版本号为 0。
- `syntax`：词法与语法对象。所有区间是源文字节偏移 `[Start,End)`，
  `Range.Text(src)` 保证定位范围回到真实源文字段。
- `symbol`：符号身份 `SymbolID`，与语法对象分离。
  - 全局（函数、顶层变量）：`global:<name>`，跨文件按名字解析；
  - 局部（参数、局部变量）：`local:<file>|<容器路径>|<name>|<槽位>`，
    容器路径如 `greet/if#0/else#1`，同名局部变量不会串到外层或别的文件。
- `analyze`：单文件嵌套作用域绑定，跨文件名字延后为全局引用。
- `index`：增量索引。只重新分析内容变化的文件；全局定义归属变化时，
  只重绑定依赖该全局名的文件，未受影响文件的符号身份保持稳定。
- `service` / `server`：会话编排与 JSON/HTTP 接口。

查询结果携带 `snapshot_id` 与每个相关文档的 `version`，结果与文档版本绑定。

## EduLang

```edu
let limit = 10
fn greet(name) {
  if name > 0 {
    let msg = shout(name)
    reply(msg)
  } else {
    reply(name)        // 这里的 name 是参数；与 if 块内无关
  }
}
```

支持 `let`、`fn`、`if/else`、调用、二元运算（`+ - * / >`）与行/块注释。

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/files` | 登记磁盘文件 `{uri, content}` |
| POST | `/change` | 提交未保存改动 `{uri, content}`，版本 +1，返回受影响文件 |
| POST | `/save` | 固化缓冲区 `{uri}` |
| POST | `/close` | 关闭文档，回到磁盘版本 `{uri}` |
| GET/POST | `/definition` | `?uri=&offset=` 或 `{uri, offset}` |
| GET/POST | `/references` | 同上，返回含定义在内的全部引用 |

引用位置包含 `uri/start/end/line/column/text/version`。

```bash
go run ./cmd/navd -addr :8080
curl -s localhost:8080/files -d '{"uri":"file:///a.edu","content":"fn a(){b()}"}'
curl -s 'localhost:8080/definition?uri=file%3A%2F%2F%2Fa.edu&offset=7'
```

## 测试

`go test ./...` 覆盖：嵌套作用域遮蔽、跨文件相互引用、未保存缓冲、
增量受影响集合、符号身份稳定性，以及 HTTP 端到端流程。
